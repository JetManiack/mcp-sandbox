package humanauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/storage"
)

// OIDCConfig holds the settings needed to talk to Keycloak.
type OIDCConfig struct {
	Issuer        string
	ClientID      string
	ClientSecret  string
	PublicURL     string // this server's externally-reachable base URL
	AdminGroup    string // Keycloak group whose members get role "admin"; everyone else gets "viewer"
	EncryptionKey []byte // AES-256 key (32 bytes) used to encrypt Session.RefreshToken at rest
}

// OIDCHandlers are the three browser-navigation routes that drive the OAuth2
// Authorization Code flow.
type OIDCHandlers struct {
	Login    http.HandlerFunc
	Callback http.HandlerFunc
	Logout   http.HandlerFunc
}

const oauthStateCookieName = "executor_oauth_state"

// keycloakRefresher is the real tokenRefresher, backed by Keycloak's token
// endpoint.
type keycloakRefresher struct {
	oauth2Config oauth2.Config
	verifier     *oidc.IDTokenVerifier
	adminGroup   string
}

func (k keycloakRefresher) Refresh(refreshToken string) (Identity, string, error) {
	ctx := context.Background()
	token, err := k.oauth2Config.TokenSource(ctx, &oauth2.Token{RefreshToken: refreshToken}).Token()
	if err != nil {
		return Identity{}, "", fmt.Errorf("refresh token exchange: %w", err)
	}
	identity, err := k.verifyAndExtract(ctx, token)
	if err != nil {
		return Identity{}, "", err
	}
	newRefreshToken := token.RefreshToken
	if newRefreshToken == "" {
		newRefreshToken = refreshToken // Keycloak doesn't always rotate it
	}
	return identity, newRefreshToken, nil
}

// verifyAndExtract verifies token's ID token and extracts an Identity from
// its claims, mapping AdminGroup membership to role "admin" and everything
// else to "viewer".
func (k keycloakRefresher) verifyAndExtract(ctx context.Context, token *oauth2.Token) (Identity, error) {
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		return Identity{}, errors.New("token response missing id_token")
	}
	idToken, err := k.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return Identity{}, fmt.Errorf("verify id_token: %w", err)
	}

	var claims struct {
		Subject           string   `json:"sub"`
		PreferredUsername string   `json:"preferred_username"`
		Name              string   `json:"name"`
		Groups            []string `json:"groups"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("parse claims: %w", err)
	}

	displayName := claims.Name
	if displayName == "" {
		displayName = claims.PreferredUsername
	}

	return Identity{
		Subject:     claims.Subject,
		DisplayName: displayName,
		Role:        roleForGroups(claims.Groups, k.adminGroup),
	}, nil
}

// roleForGroups maps an ID token's groups claim to this app's role. Keycloak
// reports group membership as a path ("/admins"), but a realm configured with a
// plain-name mapper reports the bare name — both have to match, or role gating
// silently degrades every admin to a viewer.
//
// Anything that isn't a member of adminGroup gets "viewer": a new group added
// to the realm later must never become an accidental privilege escalation.
func roleForGroups(groups []string, adminGroup string) string {
	adminGroupPath := "/" + adminGroup
	for _, group := range groups {
		if group == adminGroupPath || group == adminGroup {
			return "admin"
		}
	}
	return "viewer"
}

// NewOIDCHandlers discovers cfg.Issuer's OIDC configuration, builds the
// OAuth2/OIDC client, and returns an OIDCProvider sharing the same
// Keycloak-backed refresher as the login/callback/logout handlers.
func NewOIDCHandlers(ctx context.Context, db *gorm.DB, cfg OIDCConfig) (OIDCProvider, OIDCHandlers, error) {
	discovered, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return OIDCProvider{}, OIDCHandlers{}, fmt.Errorf("discover OIDC issuer %q: %w", cfg.Issuer, err)
	}

	oauth2Config := oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		Endpoint:     discovered.Endpoint(),
		RedirectURL:  cfg.PublicURL + "/auth/callback",
		Scopes:       []string{oidc.ScopeOpenID, "profile", "offline_access"},
	}
	verifier := discovered.Verifier(&oidc.Config{ClientID: cfg.ClientID})
	refresher := keycloakRefresher{oauth2Config: oauth2Config, verifier: verifier, adminGroup: cfg.AdminGroup}
	provider := NewOIDCProvider(db, refresher, cfg.EncryptionKey)

	loginHandler := func(w http.ResponseWriter, r *http.Request) {
		state := randomOAuthState()
		http.SetCookie(w, &http.Cookie{
			Name:     oauthStateCookieName,
			Value:    state,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
			MaxAge:   300,
		})
		http.Redirect(w, r, oauth2Config.AuthCodeURL(state), http.StatusFound)
	}

	callbackHandler := func(w http.ResponseWriter, r *http.Request) {
		stateCookie, err := r.Cookie(oauthStateCookieName)
		if err != nil || r.URL.Query().Get("state") != stateCookie.Value {
			http.Error(w, "invalid OAuth state", http.StatusBadRequest)
			return
		}

		token, err := oauth2Config.Exchange(r.Context(), r.URL.Query().Get("code"))
		if err != nil {
			http.Error(w, "token exchange failed", http.StatusBadGateway)
			return
		}
		identity, err := refresher.verifyAndExtract(r.Context(), token)
		if err != nil {
			http.Error(w, "identity verification failed", http.StatusBadGateway)
			return
		}

		encryptedRefreshToken, err := encryptRefreshToken(cfg.EncryptionKey, token.RefreshToken)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		// rawSessionID is what goes in the cookie and is shown to the browser;
		// only its hash is ever persisted (see hashSessionID in
		// oidc_provider.go) — a database read/leak alone must never yield a
		// usable session token.
		rawSessionID := uuid.NewString()
		session := &storage.Session{
			ID:           hashSessionID(rawSessionID),
			Subject:      identity.Subject,
			DisplayName:  identity.DisplayName,
			Role:         identity.Role,
			RefreshToken: encryptedRefreshToken,
			ExpiresAt:    time.Now().Add(sessionRefreshInterval),
		}
		if err := db.Create(session).Error; err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     SessionCookieName,
			Value:    rawSessionID,
			Path:     "/",
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, "/", http.StatusFound)
	}

	logoutHandler := func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(SessionCookieName); err == nil {
			db.Delete(&storage.Session{}, "id = ?", hashSessionID(cookie.Value))
		}
		http.SetCookie(w, &http.Cookie{
			Name:     SessionCookieName,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, "/", http.StatusFound)
	}

	return provider, OIDCHandlers{Login: loginHandler, Callback: callbackHandler, Logout: logoutHandler}, nil
}

func randomOAuthState() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}

// RandomOAuthStateForTesting exposes randomOAuthState for this package's own
// tests — there is nothing else in this file testable without a live OIDC
// issuer.
func RandomOAuthStateForTesting() string {
	return randomOAuthState()
}
