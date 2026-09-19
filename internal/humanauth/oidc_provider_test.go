package humanauth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/storage"
)

// fakeRefresher stands in for Keycloak's token endpoint.
type fakeRefresher struct {
	identity        Identity
	newRefreshToken string
	err             error
	calls           int
}

func (f *fakeRefresher) Refresh(refreshToken string) (Identity, string, error) {
	f.calls++
	if f.err != nil {
		return Identity{}, "", f.err
	}
	return f.identity, f.newRefreshToken, nil
}

// seedSession writes a session row the way the callback handler does, and
// returns the raw (unhashed) session ID that belongs in the cookie.
func seedSession(t *testing.T, db *gorm.DB, key []byte, refreshToken string, expiresAt time.Time) string {
	t.Helper()
	encrypted, err := encryptRefreshToken(key, refreshToken)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	raw := "raw-session-id"
	session := &storage.Session{
		ID:           hashSessionID(raw),
		Subject:      "sub-1",
		DisplayName:  "Ada",
		Role:         "viewer",
		RefreshToken: encrypted,
		ExpiresAt:    expiresAt,
	}
	if err := db.Create(session).Error; err != nil {
		t.Fatalf("create session: %v", err)
	}
	return raw
}

func requestWithSession(rawID string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/me", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: rawID})
	return req
}

// Only the hash of a session ID is stored, so a database dump is not a set of
// usable sessions.
func TestSessionIDIsStoredHashed(t *testing.T) {
	db := openTestDB(t)
	raw := seedSession(t, db, testKey(t), "refresh-1", time.Now().Add(time.Hour))

	var stored storage.Session
	if err := db.First(&stored).Error; err != nil {
		t.Fatalf("load session: %v", err)
	}
	if stored.ID == raw {
		t.Error("the raw session ID was stored verbatim")
	}
	if len(stored.ID) != 64 {
		t.Errorf("stored ID is %d chars, want a 64-char SHA-256 hex digest", len(stored.ID))
	}
}

func TestAuthenticateUsesCachedClaimsBeforeExpiry(t *testing.T) {
	db := openTestDB(t)
	key := testKey(t)
	raw := seedSession(t, db, key, "refresh-1", time.Now().Add(time.Hour))

	refresher := &fakeRefresher{}
	provider := NewOIDCProvider(db, refresher, key)

	identity, err := provider.Authenticate(requestWithSession(raw))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if identity.Subject != "sub-1" || identity.Role != "viewer" {
		t.Errorf("identity = %+v, want the stored claims", identity)
	}
	// A token exchange per request would put Keycloak in the hot path of every
	// page load.
	if refresher.calls != 0 {
		t.Errorf("refresher was called %d times before expiry, want 0", refresher.calls)
	}
}

// Past the checkpoint the claims are re-read from Keycloak, which is how a role
// change propagates without waiting for the user to log in again.
func TestAuthenticateRefreshesAfterExpiryAndPicksUpNewRole(t *testing.T) {
	db := openTestDB(t)
	key := testKey(t)
	raw := seedSession(t, db, key, "refresh-1", time.Now().Add(-time.Minute))

	refresher := &fakeRefresher{
		identity:        Identity{Subject: "sub-1", DisplayName: "Ada", Role: "admin"},
		newRefreshToken: "refresh-2",
	}
	provider := NewOIDCProvider(db, refresher, key)

	identity, err := provider.Authenticate(requestWithSession(raw))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if identity.Role != "admin" {
		t.Errorf("Role = %q, want admin (the refreshed claims)", identity.Role)
	}
	if refresher.calls != 1 {
		t.Errorf("refresher was called %d times, want 1", refresher.calls)
	}

	var stored storage.Session
	if err := db.First(&stored).Error; err != nil {
		t.Fatalf("load session: %v", err)
	}
	if stored.Role != "admin" {
		t.Errorf("stored Role = %q, want admin", stored.Role)
	}
	if !stored.ExpiresAt.After(time.Now()) {
		t.Error("the session checkpoint was not pushed forward after a refresh")
	}
	// The rotated refresh token must be re-encrypted, not stored in the clear.
	if stored.RefreshToken == "refresh-2" {
		t.Error("the rotated refresh token was stored unencrypted")
	}
	decrypted, err := decryptRefreshToken(key, stored.RefreshToken)
	if err != nil {
		t.Fatalf("decrypt stored token: %v", err)
	}
	if decrypted != "refresh-2" {
		t.Errorf("stored refresh token = %q, want refresh-2", decrypted)
	}
}

// A refresh failure means Keycloak no longer honors the session — most likely
// because it was revoked. The row has to go, or a revoked login keeps working
// until its next checkpoint.
func TestAuthenticateDeletesSessionWhenRefreshFails(t *testing.T) {
	db := openTestDB(t)
	key := testKey(t)
	raw := seedSession(t, db, key, "refresh-1", time.Now().Add(-time.Minute))

	provider := NewOIDCProvider(db, &fakeRefresher{err: errors.New("invalid_grant")}, key)

	if _, err := provider.Authenticate(requestWithSession(raw)); err == nil {
		t.Fatal("Authenticate succeeded, want error")
	}

	var count int64
	if err := db.Model(&storage.Session{}).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("%d sessions remain, want the failed session deleted", count)
	}
}

// If the stored token can't be decrypted (a rotated or wrong key), the session
// is unusable and must be cleared rather than retried forever.
func TestAuthenticateDeletesSessionWhenDecryptionFails(t *testing.T) {
	db := openTestDB(t)
	raw := seedSession(t, db, testKey(t), "refresh-1", time.Now().Add(-time.Minute))

	provider := NewOIDCProvider(db, &fakeRefresher{}, testKey(t))

	if _, err := provider.Authenticate(requestWithSession(raw)); err == nil {
		t.Fatal("Authenticate succeeded, want error")
	}

	var count int64
	if err := db.Model(&storage.Session{}).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Errorf("%d sessions remain, want the undecryptable session deleted", count)
	}
}

func TestAuthenticateWithoutUsableCookie(t *testing.T) {
	db := openTestDB(t)
	provider := NewOIDCProvider(db, &fakeRefresher{}, testKey(t))

	t.Run("no cookie", func(t *testing.T) {
		if _, err := provider.Authenticate(httptest.NewRequest(http.MethodGet, "/api/me", nil)); err == nil {
			t.Error("Authenticate succeeded without a session cookie")
		}
	})

	t.Run("unknown session", func(t *testing.T) {
		if _, err := provider.Authenticate(requestWithSession("no-such-session")); err == nil {
			t.Error("Authenticate succeeded with an unknown session ID")
		}
	})
}
