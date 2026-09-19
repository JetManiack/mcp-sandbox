package sandbox

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/auth"
	"github.com/JetManiack/mcp-sandbox/internal/storage"
)

// ErrSandboxBlocked marks a refusal as an administrator's decision rather than
// a failure. Wrapped rather than returned bare, so the agent still gets the
// whole sentence while the journal can tell the two apart.
var ErrSandboxBlocked = errors.New("sandbox is administratively blocked")

// agentForCall resolves the authenticated agent and refuses if an administrator
// has blocked its sandbox.
//
// Fails closed on a missing actor: without one there is no sandbox to name,
// and guessing would mean dispatching somebody else's work.
//
// The block is read from the database on every call — no caching — so a block
// applied by another replica takes effect on the very next request.
func agentForCall(ctx context.Context, db *gorm.DB) (string, error) {
	actor, ok := auth.ActorFromContext(ctx)
	if !ok {
		return "", auth.ErrNoActor
	}

	if db != nil {
		block, err := storage.ActiveSandboxBlock(db, actor.ID)
		if err != nil {
			// Fail closed: an unreadable block table must not be the way an
			// agent gets past a block.
			return "", fmt.Errorf("cannot verify sandbox block state: %w", err)
		}
		if block != nil {
			return "", blockedError(block)
		}
	}

	return actor.ID, nil
}

func blockedError(block *storage.SandboxBlock) error {
	return fmt.Errorf(
		"%w by %s at %s: %s — this is a human decision, not a transient error; do not retry, contact an operator to resume",
		ErrSandboxBlocked,
		block.BlockedByName,
		block.BlockedAt.Format("2006-01-02 15:04:05 MST"),
		block.Reason,
	)
}
