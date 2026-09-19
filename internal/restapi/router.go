package restapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/humanauth"
	"github.com/JetManiack/mcp-sandbox/internal/stream"
	"github.com/JetManiack/mcp-sandbox/internal/workerhub"
)

// Options is what the REST API needs: the database, the event bus it streams
// terminals from, the worker hub it stops sandboxes through, and the provider that
// authenticates the humans doing it.
type Options struct {
	DB           *gorm.DB
	Bus          *stream.Broadcaster
	Hub          *workerhub.Hub
	AuthProvider humanauth.Provider
}

// NewRouter builds the API handler, mounted with no path prefix (the caller
// mounts it under /api — see cmd/executor/main.go).
//
// Every route requires a human session. Reading — the sandbox list and the
// terminal stream — is open to any authenticated human; everything that changes
// state, meaning the emergency block and agent credentials, additionally
// requires role admin.
func NewRouter(opts Options) http.Handler {
	return NewRouterWithDomain(opts)
}

// NewRouterWithDomain builds the API handler and optionally mounts additional
// domain-specific sub-routes via the provided functions.
func NewRouterWithDomain(opts Options, domain ...func(chi.Router)) http.Handler {
	r := chi.NewRouter()
	r.Use(humanauth.RequireHumanAuth(opts.DB, opts.AuthProvider))

	r.Get("/me", meHandler())

	r.Route("/sandboxes", func(r chi.Router) {
		r.Get("/", listSandboxesHandler(opts))
		r.Get("/{id}", getSandboxHandler(opts))
		r.Get("/{id}/stream", streamSandboxHandler(opts))

		r.Group(func(r chi.Router) {
			r.Use(humanauth.RequireAdmin)
			r.Post("/{id}/block", blockSandboxHandler(opts))
			r.Delete("/{id}/block", releaseSandboxHandler(opts))
		})
	})

	// /actors (renamed from /agents)
	r.Route("/actors", func(r chi.Router) {
		r.Use(humanauth.RequireAdmin)
		r.Get("/", listAgentsHandler(opts.DB))
		r.Post("/", createAgentHandler(opts.DB))
		r.Delete("/{id}", deleteAgentHandler(opts.DB))
		r.Get("/{id}/credentials", listAgentTokensHandler(opts.DB))
		r.Post("/{id}/credentials", issueTokenHandler(opts.DB))
	})

	// /credentials/{id} for revocation (renamed from /agents/{id}/tokens/{tokenID})
	r.Route("/credentials", func(r chi.Router) {
		r.Use(humanauth.RequireAdmin)
		r.Delete("/{id}", revokeTokenHandler(opts.DB))
	})

	// /tool-calls — adapter over the AuditEvent journal
	r.Get("/tool-calls", listToolCallsHandler(opts.DB))
	r.Get("/tool-calls/{id}", getToolCallHandler(opts.DB))

	// Domain-specific routes (e.g. /workers from workerhub)
	for _, fn := range domain {
		fn(r)
	}

	return r
}
