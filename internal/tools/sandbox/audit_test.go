package sandbox

import (
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/storage"
)

// journalFor reads back what the journal recorded for one agent, oldest first.
func journalFor(t *testing.T, db *gorm.DB, actorID string) []storage.AuditEvent {
	t.Helper()

	events, err := storage.ListAudit(db, storage.AuditFilter{ActorID: actorID})
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	return events
}

func TestAToolCallLeavesAPairOfJournalRows(t *testing.T) {
	db := openTestDB(t)
	agent, token := mustAgentWithToken(t, db, "agent-1")
	registrar := newTestRegistrar(t, db)
	session := connectSession(t, newTestServer(t, registrar, db), token)

	if res := callTool(t, session, "write_file", map[string]any{
		"path": "notes.txt", "content": "twelve bytes",
	}); res.IsError {
		t.Fatalf("write_file: %s", contentText(res.Content))
	}

	events := journalFor(t, db, agent.ID)
	if len(events) != 2 {
		t.Fatalf("rows = %d, want the started and finished pair", len(events))
	}

	started, finished := events[0], events[1]
	if started.Phase != storage.AuditStarted || finished.Phase != storage.AuditFinished {
		t.Fatalf("phases = %q, %q", started.Phase, finished.Phase)
	}
	if started.CallID != finished.CallID {
		t.Error("the two rows do not share a call id, so nothing pairs them")
	}
	if started.Action != "write_file" || started.Target != "notes.txt" {
		t.Errorf("started row = %+v, want write_file on notes.txt", started)
	}
	if finished.Outcome != storage.AuditOutcomeOK {
		t.Errorf("outcome = %q, want ok", finished.Outcome)
	}
	if finished.Bytes != len("twelve bytes") {
		t.Errorf("bytes = %d, want %d", finished.Bytes, len("twelve bytes"))
	}
	if finished.WorkerID == "" {
		t.Error("no worker recorded")
	}
	if started.ActorName != "agent-1" {
		t.Errorf("actor name = %q, want it on the row", started.ActorName)
	}
}

func TestTheJournalRecordsWhatACommandWas(t *testing.T) {
	db := openTestDB(t)
	agent, token := mustAgentWithToken(t, db, "agent-1")
	registrar := newTestRegistrar(t, db)
	session := connectSession(t, newTestServer(t, registrar, db), token)

	if res := callTool(t, session, "exec_command", map[string]any{
		"command": "/bin/echo", "args": []any{"hello", "world"},
	}); res.IsError {
		t.Fatalf("exec_command: %s", contentText(res.Content))
	}

	events := journalFor(t, db, agent.ID)
	if len(events) != 2 {
		t.Fatalf("rows = %d, want two", len(events))
	}
	if got := events[0].Target; !strings.Contains(got, `"/bin/echo"`) ||
		!strings.Contains(got, `"hello"`) || !strings.Contains(got, `"world"`) {
		t.Errorf("target = %s, want the program and each argument", got)
	}
	if events[1].DurationMs < 0 {
		t.Errorf("duration = %d", events[1].DurationMs)
	}
}

func TestATimedOutCommandIsNotJournalledAsSuccess(t *testing.T) {
	db := openTestDB(t)
	agent, token := mustAgentWithToken(t, db, "agent-1")
	registrar := newTestRegistrar(t, db)
	session := connectSession(t, newTestServer(t, registrar, db), token)

	res := callTool(t, session, "exec_command", map[string]any{
		"command": "/bin/sleep", "args": []any{"30"}, "timeout_sec": 1,
	})
	if !res.IsError {
		t.Fatal("a timed-out command reported success to the agent")
	}

	events := journalFor(t, db, agent.ID)
	if len(events) != 2 {
		t.Fatalf("rows = %d, want two", len(events))
	}
	finished := events[1]
	if finished.Outcome != storage.AuditOutcomeTimeout {
		t.Errorf("outcome = %q, want %q", finished.Outcome, storage.AuditOutcomeTimeout)
	}
	if finished.Outcome == storage.AuditOutcomeOK {
		t.Error("a killed command is indistinguishable from one that succeeded")
	}
	if finished.ExitCode == nil {
		t.Fatal("no exit code recorded")
	}
	if *finished.ExitCode != -1 {
		t.Errorf("exit code = %d, want -1 for a command that was killed", *finished.ExitCode)
	}
}

func TestACommandsExitCodeIsRecorded(t *testing.T) {
	db := openTestDB(t)
	agent, token := mustAgentWithToken(t, db, "agent-1")
	registrar := newTestRegistrar(t, db)
	session := connectSession(t, newTestServer(t, registrar, db), token)

	if res := callTool(t, session, "exec_command", map[string]any{
		"command": "/bin/sh", "args": []any{"-c", "exit 42"},
	}); res.IsError {
		t.Fatalf("exec_command: %s", contentText(res.Content))
	}

	events := journalFor(t, db, agent.ID)
	finished := events[1]
	if finished.Outcome != storage.AuditOutcomeOK {
		t.Errorf("outcome = %q, want ok", finished.Outcome)
	}
	if finished.ExitCode == nil || *finished.ExitCode != 42 {
		t.Errorf("exit code = %v, want 42", finished.ExitCode)
	}
}

func TestTheArgumentVectorIsRecordedAsAVector(t *testing.T) {
	db := openTestDB(t)
	agent, token := mustAgentWithToken(t, db, "agent-1")
	registrar := newTestRegistrar(t, db)
	session := connectSession(t, newTestServer(t, registrar, db), token)

	callTool(t, session, "exec_command", map[string]any{
		"command": "/bin/sh", "args": []any{"-c", "true & true"},
	})

	target := journalFor(t, db, agent.ID)[0].Target
	if !strings.Contains(target, `"-c"`) || !strings.Contains(target, `"true & true"`) {
		t.Errorf("target = %s, want the arguments kept apart", target)
	}
}

func TestABlockedCallIsJournalledAsBlockedNotFailed(t *testing.T) {
	db := openTestDB(t)
	agent, token := mustAgentWithToken(t, db, "agent-1")
	admin, err := storage.GetOrCreateHumanActor(db, "admin-subject", "Grace", "admin")
	if err != nil {
		t.Fatalf("GetOrCreateHumanActor: %v", err)
	}
	if _, err := storage.BlockSandbox(db, agent.ID, admin, "spending the afternoon in a loop"); err != nil {
		t.Fatalf("BlockSandbox: %v", err)
	}

	registrar := newTestRegistrar(t, db)
	session := connectSession(t, newTestServer(t, registrar, db), token)
	if res := callTool(t, session, "read_file", map[string]any{"path": "anything.txt"}); !res.IsError {
		t.Fatal("a blocked agent's call succeeded")
	}

	events := journalFor(t, db, agent.ID)
	if len(events) != 2 {
		t.Fatalf("rows = %d, want the refusal recorded as a pair like any other call", len(events))
	}
	if events[1].Outcome != storage.AuditOutcomeBlocked {
		t.Errorf("outcome = %q, want %q", events[1].Outcome, storage.AuditOutcomeBlocked)
	}
	if !strings.Contains(events[1].Error, "Grace") {
		t.Errorf("error = %q, want it to name who blocked", events[1].Error)
	}
}

func TestAFailedCallIsJournalledWithItsError(t *testing.T) {
	db := openTestDB(t)
	agent, token := mustAgentWithToken(t, db, "agent-1")
	registrar := newTestRegistrar(t, db)
	session := connectSession(t, newTestServer(t, registrar, db), token)

	if res := callTool(t, session, "read_file", map[string]any{"path": "not-there.txt"}); !res.IsError {
		t.Fatal("reading a missing file succeeded")
	}

	events := journalFor(t, db, agent.ID)
	if len(events) != 2 {
		t.Fatalf("rows = %d, want two", len(events))
	}
	if events[1].Outcome != storage.AuditOutcomeError {
		t.Errorf("outcome = %q, want error", events[1].Outcome)
	}
	if events[1].Error == "" {
		t.Error("no error recorded, so the row says it failed without saying how")
	}
}
