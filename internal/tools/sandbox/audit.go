package sandbox

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/auth"
	"github.com/JetManiack/mcp-sandbox/internal/storage"
)

// workerNamer is implemented by the hub. Consulted through an optional
// interface rather than added to Executor, because knowing which worker served
// a call is the journal's concern and not something a tool handler needs.
type workerNamer interface {
	WorkerFor(agentID string) (string, bool)
}

// audited wraps a tool handler so every call leaves two rows in the journal:
// one before it is attempted and one when it returns.
//
// Wrapped at registration rather than written inside each handler, so a tool
// added later is journalled by construction instead of by remembering. The
// cost is one insert on each side of a call that is already crossing a process
// boundary.
func audited[In, Out any](
	r *Registrar,
	action string,
	target func(In) string,
	h mcp.ToolHandlerFor[In, Out],
) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		// No database, or no identity to attribute the action to: the call
		// still runs. A journal that can refuse work is one that eventually
		// gets switched off.
		actor, ok := auth.ActorFromContext(ctx)
		if r.db == nil || !ok {
			return h(ctx, req, in)
		}

		var describes string
		if target != nil {
			describes = target(in)
		}
		var workerID string
		if namer, ok := r.executor.(workerNamer); ok {
			workerID, _ = namer.WorkerFor(actor.ID)
		}

		record := storage.AuditRecord{
			Actor: actor, Action: action, Target: describes, WorkerID: workerID,
		}
		callID, err := storage.AppendAuditStarted(r.db, record)
		if err != nil {
			slog.Error("could not journal the start of a tool call", "action", action, "error", err)
		}
		record.CallID = callID

		began := time.Now()
		result, out, callErr := h(ctx, req, in)

		record.DurationMs = time.Since(began).Milliseconds()
		record.Outcome = storage.AuditOutcomeOK
		// A handler that decided something the error does not carry says so
		// here. A command cut short by its timeout returns no error — the
		// tool call succeeded in reporting it — so without this the journal
		// records the one word that hides a kill the service performed.
		if reported, ok := any(out).(interface{ auditOutcome() string }); ok {
			if outcome := reported.auditOutcome(); outcome != "" {
				record.Outcome = outcome
			}
		}
		if coded, ok := any(out).(interface{ auditExitCode() int }); ok {
			code := coded.auditExitCode()
			record.ExitCode = &code
		}
		switch {
		case callErr != nil && errors.Is(callErr, ErrSandboxBlocked):
			// An administrator's decision is not a failure, and a journal that
			// files the two together cannot answer "was this agent stopped".
			record.Outcome = storage.AuditOutcomeBlocked
			record.Error = callErr.Error()
		case callErr != nil:
			record.Outcome = storage.AuditOutcomeError
			record.Error = callErr.Error()
		}
		// The worker may have been resolved only by the call itself, for an
		// agent that had never been pinned before.
		if record.WorkerID == "" {
			if namer, ok := r.executor.(workerNamer); ok {
				record.WorkerID, _ = namer.WorkerFor(actor.ID)
			}
		}
		if sized, ok := any(out).(interface{ auditBytes() int }); ok {
			record.Bytes = sized.auditBytes()
		}

		if err := storage.AppendAuditFinished(r.db, record); err != nil {
			slog.Error("could not journal the end of a tool call", "action", action, "error", err)
		}
		return result, out, callErr
	}
}

// useDB returns r.db to expose it to audited without direct field access across
// compilation units. In practice audited is in the same package so direct
// access works — this alias keeps the code readable.
func useDB(r *Registrar) *gorm.DB { return r.db }
