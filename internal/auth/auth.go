// Package auth provides bearer-token authentication for AI agents:
// minting and verifying tokens, middleware that enforces the check on every
// request, and the request-context helpers that carry the resolved Actor to
// tool handlers downstream.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/storage"
)

type contextKey string

const actorContextKey contextKey = "actor"

// ErrNoActor is returned by tool handlers reached without an authenticated
// actor in the request context.
var ErrNoActor = errors.New("no authenticated actor in context")

func withActor(ctx context.Context, actor *storage.Actor) context.Context {
	return context.WithValue(ctx, actorContextKey, actor)
}

// ActorFromContext returns the Actor authenticated for the current request,
// if any. Tool handlers need the whole Actor: the ID selects the sandbox
// directory, and the display name goes into the terminal stream and into the
// message an agent sees when its sandbox is blocked.
func ActorFromContext(ctx context.Context) (*storage.Actor, bool) {
	actor, ok := ctx.Value(actorContextKey).(*storage.Actor)
	return actor, ok
}

// WithActorForTesting injects actor into ctx the same way RequireBearer does,
// for tests that exercise tool handlers without an HTTP round trip.
func WithActorForTesting(ctx context.Context, actor *storage.Actor) context.Context {
	return withActor(ctx, actor)
}

// hashToken is how a bearer token is stored: only its SHA-256 digest ever
// reaches the database, so reading the credentials table yields nothing usable
// as a token.
func hashToken(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}

// Authenticate resolves a raw bearer token to the Actor that owns it,
// rejecting revoked credentials.
func Authenticate(db *gorm.DB, raw string) (*storage.Actor, error) {
	var cred storage.AgentCredential
	if err := db.Where("token_hash = ?", hashToken(raw)).First(&cred).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, storage.ErrCredentialNotFound
		}
		return nil, err
	}
	if cred.RevokedAt != nil {
		return nil, storage.ErrTokenRevoked
	}
	now := time.Now().UTC()
	_ = db.Model(&cred).Update("last_used_at", now).Error
	return storage.GetActorByID(db, cred.ActorID)
}

// Issue mints a new bearer token for actorID with an optional human-readable
// label, and returns it in the clear exactly once — only its hash is stored.
func Issue(db *gorm.DB, actorID string, label string) (string, *storage.AgentCredential, error) {
	if _, err := storage.GetActorByID(db, actorID); err != nil {
		return "", nil, err
	}
	rawToken := "age_" + uuid.NewString()
	cred := &storage.AgentCredential{
		ID:        uuid.NewString(),
		ActorID:   actorID,
		Label:     label,
		TokenHash: hashToken(rawToken),
		CreatedAt: time.Now().UTC(),
	}
	if err := db.Create(cred).Error; err != nil {
		return "", nil, fmt.Errorf("issue agent token: %w", err)
	}
	return rawToken, cred, nil
}

// RequireBearer authenticates every request by its Authorization: Bearer
// header and injects the resulting Actor into the request context for
// downstream handlers to read via ActorFromContext.
func RequireBearer(db *gorm.DB, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sandbox"`)
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}

		actor, err := Authenticate(db, token)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="sandbox"`)
			http.Error(w, "invalid or revoked token", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r.WithContext(withActor(r.Context(), actor)))
	})
}
