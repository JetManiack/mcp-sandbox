package mcpserver

// Auth helpers are implemented in internal/auth. This file re-exports the
// symbols the mcpserver tests need so they continue to compile without a
// package change, while production call sites import auth directly.

import (
	"context"

	"github.com/JetManiack/mcp-sandbox/internal/auth"
	"github.com/JetManiack/mcp-sandbox/internal/storage"
)

// ActorFromContext re-exports auth.ActorFromContext for package-internal use.
func ActorFromContext(ctx context.Context) (*storage.Actor, bool) {
	return auth.ActorFromContext(ctx)
}

// WithActorForTesting re-exports auth.WithActorForTesting so that white-box
// tests in this package can inject actors without importing auth.
func WithActorForTesting(ctx context.Context, actor *storage.Actor) context.Context {
	return auth.WithActorForTesting(ctx, actor)
}
