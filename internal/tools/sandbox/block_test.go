package sandbox

import (
	"context"
	"strings"
	"testing"

	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/auth"
	"github.com/JetManiack/mcp-sandbox/internal/storage"
)

// mustBlock blocks agentID's sandbox on behalf of a freshly-provisioned admin.
func mustBlock(t *testing.T, db *gorm.DB, agentID, reason string) *storage.SandboxBlock {
	t.Helper()
	admin, err := storage.GetOrCreateHumanActor(db, "admin-subject", "Grace", "admin")
	if err != nil {
		t.Fatalf("GetOrCreateHumanActor: %v", err)
	}
	block, err := storage.BlockSandbox(db, agentID, admin, reason)
	if err != nil {
		t.Fatalf("BlockSandbox: %v", err)
	}
	return block
}

func TestBlockedSandboxRefusesEveryTool(t *testing.T) {
	db := openTestDB(t)
	agent, token := mustAgentWithToken(t, db, "runaway")
	registrar := newTestRegistrar(t, db)
	session := connectSession(t, newTestServer(t, registrar, db), token)

	if res := callTool(t, session, "write_file", map[string]any{"path": "a.txt", "content": "x"}); res.IsError {
		t.Fatalf("write_file before block: %s", contentText(res.Content))
	}

	mustBlock(t, db, agent.ID, "spawning processes in a loop")

	calls := []struct {
		tool string
		args map[string]any
	}{
		{"exec_command", map[string]any{"command": "echo still running"}},
		{"read_file", map[string]any{"path": "a.txt"}},
		{"write_file", map[string]any{"path": "b.txt", "content": "x"}},
		{"list_dir", map[string]any{}},
		{"delete_file", map[string]any{"path": "a.txt"}},
		{"get_sandbox_status", map[string]any{}},
	}

	for _, call := range calls {
		t.Run(call.tool, func(t *testing.T) {
			res := callTool(t, session, call.tool, call.args)
			if !res.IsError {
				t.Fatalf("%s succeeded on a blocked sandbox", call.tool)
			}

			message := contentText(res.Content)
			for _, want := range []string{"administratively blocked", "Grace", "spawning processes in a loop", "do not retry"} {
				if !strings.Contains(message, want) {
					t.Errorf("error message %q does not mention %q", message, want)
				}
			}
		})
	}
}

func TestReleasedSandboxResumes(t *testing.T) {
	db := openTestDB(t)
	agent, token := mustAgentWithToken(t, db, "runaway")
	registrar := newTestRegistrar(t, db)
	session := connectSession(t, newTestServer(t, registrar, db), token)

	mustBlock(t, db, agent.ID, "just checking")
	if res := callTool(t, session, "list_dir", map[string]any{}); !res.IsError {
		t.Fatal("list_dir succeeded while blocked")
	}

	admin, err := storage.GetOrCreateHumanActor(db, "admin-subject", "Grace", "admin")
	if err != nil {
		t.Fatalf("GetOrCreateHumanActor: %v", err)
	}
	if err := storage.ReleaseSandbox(db, agent.ID, admin); err != nil {
		t.Fatalf("ReleaseSandbox: %v", err)
	}

	if res := callTool(t, session, "list_dir", map[string]any{}); res.IsError {
		t.Errorf("list_dir still refused after release: %s", contentText(res.Content))
	}
}

func TestBlockIsReadPerCallNotCached(t *testing.T) {
	db := openTestDB(t)
	agent, token := mustAgentWithToken(t, db, "agent-1")
	registrar := newTestRegistrar(t, db)
	session := connectSession(t, newTestServer(t, registrar, db), token)

	if res := callTool(t, session, "list_dir", map[string]any{}); res.IsError {
		t.Fatalf("list_dir before block: %s", contentText(res.Content))
	}

	mustBlock(t, db, agent.ID, "blocked elsewhere")

	if res := callTool(t, session, "list_dir", map[string]any{}); !res.IsError {
		t.Error("an established session kept working after the sandbox was blocked in the database")
	}
}

func TestUnreadableBlockStateFailsClosed(t *testing.T) {
	db := openTestDB(t)
	agent := mustAgent(t, db, "agent-1")
	registrar := newTestRegistrar(t, db)

	if err := db.Migrator().DropTable(&storage.SandboxBlock{}); err != nil {
		t.Fatalf("drop sandbox_blocks: %v", err)
	}

	ctx := auth.WithActorForTesting(context.Background(), agent)
	if _, _, err := listDirHandler(registrar)(ctx, nil, ListDirInput{}); err == nil {
		t.Error("list_dir succeeded although block state could not be verified")
	}
}

func TestTimeoutKeepsPartialOutput(t *testing.T) {
	db := openTestDB(t)
	_, token := mustAgentWithToken(t, db, "agent-1")
	registrar := newTestRegistrar(t, db)
	session := connectSession(t, newTestServer(t, registrar, db), token)

	result := callTool(t, session, "exec_command", map[string]any{
		"command":     "/bin/sh",
		"args":        []string{"-c", "echo before-the-hang; sleep 30"},
		"timeout_sec": 1,
	})
	if !result.IsError {
		t.Fatal("a command that outran its timeout was reported as success")
	}
	if got := outputField(t, result, "stdout"); !strings.Contains(got, "before-the-hang") {
		t.Errorf("stdout = %q, want the output produced before the timeout", got)
	}
	if text := contentText(result.Content); !strings.Contains(text, "before-the-hang") {
		t.Errorf("content = %q, want the partial output alongside the reason", text)
	}
}
