package storage_test

import (
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/storage"
)

func TestAnActionWritesTwoRowsSharingACallID(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		agent := mustAgent(t, db, "busy-agent")

		callID, err := storage.AppendAuditStarted(db, storage.AuditRecord{
			Actor: agent, Action: "exec_command", Target: "/bin/go test ./...", WorkerID: "worker-a",
		})
		if err != nil {
			t.Fatalf("AppendAuditStarted: %v", err)
		}
		if callID == "" {
			t.Fatal("no call id returned, so nothing ties the two rows together")
		}

		if err := storage.AppendAuditFinished(db, storage.AuditRecord{
			CallID: callID, Actor: agent, Action: "exec_command", Target: "/bin/go test ./...",
			WorkerID: "worker-a", Outcome: storage.AuditOutcomeOK, DurationMs: 1200, Bytes: 4096,
		}); err != nil {
			t.Fatalf("AppendAuditFinished: %v", err)
		}

		events, err := storage.ListAudit(db, storage.AuditFilter{ActorID: agent.ID})
		if err != nil {
			t.Fatalf("ListAudit: %v", err)
		}
		if len(events) != 2 {
			t.Fatalf("rows = %d, want the started and finished pair", len(events))
		}
		for _, e := range events {
			if e.CallID != callID {
				t.Errorf("row %s has call id %q, want %q", e.Phase, e.CallID, callID)
			}
		}

		// Newest first, so the finished row leads.
		if events[0].Phase != storage.AuditFinished || events[1].Phase != storage.AuditStarted {
			t.Errorf("phases = %q, %q; want finished then started", events[0].Phase, events[1].Phase)
		}
		if events[0].Outcome != storage.AuditOutcomeOK || events[0].DurationMs != 1200 {
			t.Errorf("finished row = %+v, want the outcome and duration", events[0])
		}
	})
}

func TestAStartedRowSurvivesAnActionThatNeverFinished(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		agent := mustAgent(t, db, "hung-agent")

		// The case the second row exists for: a command that hung until the pod
		// was killed. One row written on completion would leave no trace at all,
		// and the action would read as one that never happened.
		if _, err := storage.AppendAuditStarted(db, storage.AuditRecord{
			Actor: agent, Action: "exec_command", Target: "/bin/sleep infinity",
		}); err != nil {
			t.Fatalf("AppendAuditStarted: %v", err)
		}

		events, err := storage.ListAudit(db, storage.AuditFilter{ActorID: agent.ID})
		if err != nil {
			t.Fatalf("ListAudit: %v", err)
		}
		if len(events) != 1 || events[0].Phase != storage.AuditStarted {
			t.Fatalf("events = %+v, want one started row", events)
		}
		if events[0].Outcome != "" {
			t.Errorf("outcome = %q, want it empty on a started row", events[0].Outcome)
		}
	})
}

func TestTheJournalStillReadsAfterTheActorIsGone(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		agent := mustAgent(t, db, "since-deleted")
		if _, err := storage.AppendAuditStarted(db, storage.AuditRecord{
			Actor: agent, Action: "delete_file", Target: "important.txt",
		}); err != nil {
			t.Fatalf("AppendAuditStarted: %v", err)
		}

		if err := db.Delete(&storage.Actor{}, "id = ?", agent.ID).Error; err != nil {
			t.Fatalf("delete actor: %v", err)
		}

		// "Who deleted this" is the question the journal exists for, and a row
		// that says only "actor 4f2e…" answers it with nothing.
		events, err := storage.ListAudit(db, storage.AuditFilter{ActorID: agent.ID})
		if err != nil {
			t.Fatalf("ListAudit: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("rows = %d, want the row to outlive the actor", len(events))
		}
		if events[0].ActorName != "since-deleted" {
			t.Errorf("actor name = %q, want it denormalised onto the row", events[0].ActorName)
		}
	})
}

func TestPruneDeletesByAgeAndLeavesTheRest(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		agent := mustAgent(t, db, "long-lived")

		callID, err := storage.AppendAuditStarted(db, storage.AuditRecord{Actor: agent, Action: "read_file"})
		if err != nil {
			t.Fatalf("AppendAuditStarted: %v", err)
		}
		// Backdate it past the retention window. Writing the row and then moving
		// it is the only way to test age without waiting.
		if err := db.Model(&storage.AuditEvent{}).Where("call_id = ?", callID).
			Update("at", time.Now().UTC().Add(-8*24*time.Hour)).Error; err != nil {
			t.Fatalf("backdate: %v", err)
		}

		fresh, err := storage.AppendAuditStarted(db, storage.AuditRecord{Actor: agent, Action: "write_file"})
		if err != nil {
			t.Fatalf("AppendAuditStarted: %v", err)
		}

		deleted, err := storage.PruneAudit(db, 7*24*time.Hour)
		if err != nil {
			t.Fatalf("PruneAudit: %v", err)
		}
		if deleted != 1 {
			t.Errorf("deleted = %d, want just the row past retention", deleted)
		}

		events, err := storage.ListAudit(db, storage.AuditFilter{ActorID: agent.ID})
		if err != nil {
			t.Fatalf("ListAudit: %v", err)
		}
		if len(events) != 1 || events[0].CallID != fresh {
			t.Errorf("remaining = %+v, want only the fresh row", events)
		}
	})
}

func TestPruneWithoutARetentionWindowDeletesNothing(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		agent := mustAgent(t, db, "kept-forever")
		if _, err := storage.AppendAuditStarted(db, storage.AuditRecord{Actor: agent, Action: "read_file"}); err != nil {
			t.Fatalf("AppendAuditStarted: %v", err)
		}

		// Zero means "keep everything", not "delete everything" — the difference
		// between an operator disabling rotation and an operator losing the
		// journal.
		deleted, err := storage.PruneAudit(db, 0)
		if err != nil {
			t.Fatalf("PruneAudit: %v", err)
		}
		if deleted != 0 {
			t.Errorf("deleted = %d, want nothing", deleted)
		}
	})
}

func TestAnEnormousTargetIsTruncatedRatherThanRefused(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		agent := mustAgent(t, db, "verbose")

		// Three-byte characters, so a naive byte cut lands mid-rune — which
		// Postgres rejects outright, turning a long command line into a journal
		// write that fails.
		huge := strings.Repeat("яблоко ", 4000)
		if _, err := storage.AppendAuditStarted(db, storage.AuditRecord{
			Actor: agent, Action: "exec_command", Target: huge,
		}); err != nil {
			t.Fatalf("AppendAuditStarted: %v", err)
		}

		events, err := storage.ListAudit(db, storage.AuditFilter{ActorID: agent.ID})
		if err != nil {
			t.Fatalf("ListAudit: %v", err)
		}
		if len(events) != 1 {
			t.Fatalf("rows = %d, want one", len(events))
		}
		if got := events[0].Target; len(got) >= len(huge) {
			t.Errorf("target kept %d bytes, want it truncated", len(got))
		}
		if !strings.HasSuffix(events[0].Target, "…") {
			t.Error("truncation is not marked, so a cut command reads as the whole one")
		}
	})
}

func TestListFiltersByActionAndTime(t *testing.T) {
	runOnEachBackend(t, func(t *testing.T, db *gorm.DB) {
		agent := mustAgent(t, db, "mixed")
		for _, action := range []string{"exec_command", "write_file", "exec_command"} {
			if _, err := storage.AppendAuditStarted(db, storage.AuditRecord{Actor: agent, Action: action}); err != nil {
				t.Fatalf("AppendAuditStarted: %v", err)
			}
		}

		events, err := storage.ListAudit(db, storage.AuditFilter{Action: "exec_command"})
		if err != nil {
			t.Fatalf("ListAudit: %v", err)
		}
		if len(events) != 2 {
			t.Errorf("rows = %d, want the two exec_command rows", len(events))
		}

		future, err := storage.ListAudit(db, storage.AuditFilter{Since: time.Now().UTC().Add(time.Hour)})
		if err != nil {
			t.Fatalf("ListAudit: %v", err)
		}
		if len(future) != 0 {
			t.Errorf("rows after a future cutoff = %d, want none", len(future))
		}
	})
}
