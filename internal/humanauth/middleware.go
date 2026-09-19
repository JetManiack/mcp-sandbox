package humanauth

import (
	"context"
	"net/http"
	"strings"

	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/storage"
)

type contextKey string

const humanContextKey contextKey = "human"

type authenticatedHuman struct {
	actor *storage.Actor
	role  string
}

// ActorFromContext returns the human Actor authenticated by RequireHumanAuth for
// the current request, if any.
func ActorFromContext(ctx context.Context) (*storage.Actor, bool) {
	human, ok := ctx.Value(humanContextKey).(*authenticatedHuman)
	if !ok {
		return nil, false
	}
	return human.actor, true
}

// RoleFromContext returns the authenticated human's role for the current
// request, if any.
func RoleFromContext(ctx context.Context) (string, bool) {
	human, ok := ctx.Value(humanContextKey).(*authenticatedHuman)
	if !ok {
		return "", false
	}
	return human.role, true
}

// withHuman is the single place the request context is populated, shared by the
// middleware and by WithHumanForTesting.
func withHuman(ctx context.Context, actor *storage.Actor, role string) context.Context {
	return context.WithValue(ctx, humanContextKey, &authenticatedHuman{actor: actor, role: role})
}

// WithHumanForTesting injects an authenticated human into ctx the way
// RequireHumanAuth does, for tests that exercise handlers without an HTTP round
// trip.
func WithHumanForTesting(ctx context.Context, actor *storage.Actor, role string) context.Context {
	return withHuman(ctx, actor, role)
}

// RequireHumanAuth authenticates every request via provider, JIT-provisions the
// corresponding human Actor/UserIdentity if this is its first request, and
// injects both into the request context for downstream handlers to read via
// ActorFromContext/RoleFromContext.
func RequireHumanAuth(db *gorm.DB, provider Provider) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity, err := provider.Authenticate(r)
			if err != nil {
				// A browser navigation gets sent to log in; an XHR or API client
				// gets a 401 it can act on, rather than a redirect it would
				// follow into an HTML login page.
				if strings.Contains(r.Header.Get("Accept"), "text/html") {
					http.Redirect(w, r, "/auth/login", http.StatusFound)
					return
				}
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			actor, err := storage.GetOrCreateHumanActor(db, identity.Subject, identity.DisplayName, identity.Role)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}

			next.ServeHTTP(w, r.WithContext(withHuman(r.Context(), actor, identity.Role)))
		})
	}
}

// RequireAdmin wraps a handler so it only proceeds for a request already
// authenticated as RoleAdmin by RequireHumanAuth (which must run first in the
// middleware chain). Anything else — a viewer, or a request somehow missing role
// context entirely — gets 403 Forbidden.
func RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		role, ok := RoleFromContext(r.Context())
		if !ok || role != RoleAdmin {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
