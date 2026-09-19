package sandbox

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/JetManiack/mcp-sandbox/internal/auth"
	"github.com/JetManiack/mcp-sandbox/internal/mcpserver"
	"github.com/JetManiack/mcp-sandbox/internal/storage"
)

func TestListToolsAdvertisesSchemas(t *testing.T) {
	db := openTestDB(t)
	_, token := mustAgentWithToken(t, db, "agent-1")
	registrar := newTestRegistrar(t, db)
	session := connectSession(t, newTestServer(t, registrar, db), token)

	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
		if tool.InputSchema == nil {
			t.Errorf("tool %q advertises no input schema", tool.Name)
		}
		if tool.Description == "" {
			t.Errorf("tool %q advertises no description", tool.Name)
		}
	}

	for _, want := range []string{"exec_command", "read_file", "write_file", "list_dir", "delete_file", "get_sandbox_status"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools = %v, want it to contain %q", names, want)
		}
	}
}

func TestRequireAgentTokenRejectsBadCredentials(t *testing.T) {
	db := openTestDB(t)
	_, token := mustAgentWithToken(t, db, "agent-1")
	registrar := newTestRegistrar(t, db)
	handler := mcpserver.Handler(db, "test", []mcpserver.ToolRegistrar{registrar})

	tests := []struct {
		name   string
		header string
	}{
		{name: "no header", header: ""},
		{name: "wrong scheme", header: "Basic " + token},
		{name: "empty bearer", header: "Bearer "},
		{name: "unknown token", header: "Bearer nope"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("401 response carries no WWW-Authenticate header")
			}
		})
	}
}

func TestRevokedTokenIsRejected(t *testing.T) {
	db := openTestDB(t)
	agent := mustAgent(t, db, "agent-1")
	token, cred, err := storage.IssueAgentToken(db, agent.ID)
	if err != nil {
		t.Fatalf("IssueAgentToken: %v", err)
	}
	if err := storage.RevokeAgentToken(db, cred.ID); err != nil {
		t.Fatalf("RevokeAgentToken: %v", err)
	}

	registrar := newTestRegistrar(t, db)
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	mcpserver.Handler(db, "test", []mcpserver.ToolRegistrar{registrar}).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d for a revoked token", rec.Code, http.StatusUnauthorized)
	}
}

// TestToolsFailClosedWithoutActor covers the invariant that a handler reached
// without an authenticated actor must error, not quietly operate.
func TestToolsFailClosedWithoutActor(t *testing.T) {
	db := openTestDB(t)
	registrar := newTestRegistrar(t, db)
	ctx := context.Background()

	t.Run("read_file", func(t *testing.T) {
		_, _, err := readFileHandler(registrar)(ctx, nil, ReadFileInput{Path: "x"})
		requireErrNoActor(t, err)
	})
	t.Run("write_file", func(t *testing.T) {
		_, _, err := writeFileHandler(registrar)(ctx, nil, WriteFileInput{Path: "x", Content: "y"})
		requireErrNoActor(t, err)
	})
	t.Run("list_dir", func(t *testing.T) {
		_, _, err := listDirHandler(registrar)(ctx, nil, ListDirInput{})
		requireErrNoActor(t, err)
	})
	t.Run("delete_file", func(t *testing.T) {
		_, _, err := deleteFileHandler(registrar)(ctx, nil, DeleteFileInput{Path: "x"})
		requireErrNoActor(t, err)
	})
	t.Run("get_sandbox_status", func(t *testing.T) {
		_, _, err := sandboxStatusHandler(registrar)(ctx, nil, SandboxStatusInput{})
		requireErrNoActor(t, err)
	})
	t.Run("exec_command", func(t *testing.T) {
		_, _, err := execCommandHandler(registrar)(ctx, nil, ExecCommandInput{Command: "true"})
		requireErrNoActor(t, err)
	})
}

func requireErrNoActor(t *testing.T, err error) {
	t.Helper()
	if !errors.Is(err, auth.ErrNoActor) {
		t.Errorf("error = %v, want ErrNoActor", err)
	}
}

func TestSandboxesAreIsolatedPerAgent(t *testing.T) {
	db := openTestDB(t)
	_, tokenA := mustAgentWithToken(t, db, "agent-a")
	_, tokenB := mustAgentWithToken(t, db, "agent-b")

	registrar := newTestRegistrar(t, db)
	server := newTestServer(t, registrar, db)
	sessionA := connectSession(t, server, tokenA)
	sessionB := connectSession(t, server, tokenB)

	if written := callTool(t, sessionA, "write_file", map[string]any{
		"path":    "secret.txt",
		"content": "agent A only",
	}); written.IsError {
		t.Fatalf("agent A write_file failed: %s", contentText(written.Content))
	}

	read := callTool(t, sessionB, "read_file", map[string]any{"path": "secret.txt"})
	if !read.IsError {
		t.Errorf("agent B read agent A's file; result = %s", contentText(read.Content))
	}

	listed := callTool(t, sessionB, "list_dir", map[string]any{})
	if strings.Contains(contentText(listed.Content), "secret.txt") {
		t.Errorf("agent B sees agent A's file in list_dir: %s", contentText(listed.Content))
	}
}

func TestExecCommandRunsInTheAgentSandbox(t *testing.T) {
	db := openTestDB(t)
	_, token := mustAgentWithToken(t, db, "agent-1")
	registrar := newTestRegistrar(t, db)
	session := connectSession(t, newTestServer(t, registrar, db), token)

	pwd := callTool(t, session, "exec_command", map[string]any{"command": "pwd"})
	if pwd.IsError {
		t.Fatalf("exec_command(pwd) failed: %s", contentText(pwd.Content))
	}

	status := callTool(t, session, "get_sandbox_status", map[string]any{})
	if status.IsError {
		t.Fatalf("get_sandbox_status failed: %s", contentText(status.Content))
	}
	fields, _ := status.StructuredContent.(map[string]any)
	reported, _ := fields["status"].(map[string]any)
	root, _ := reported["root_dir"].(string)
	if root == "" {
		t.Fatalf("status did not report a sandbox root: %v", fields)
	}
	if out := outputField(t, pwd, "stdout"); !strings.Contains(out, root) {
		t.Errorf("output %q does not show the sandbox root as the working directory (want %q)", out, root)
	}

	echo := callTool(t, session, "exec_command", map[string]any{
		"command": "echo",
		"args":    []string{"hello", "world"},
	})
	if echo.IsError {
		t.Fatalf("exec_command(echo) failed: %s", contentText(echo.Content))
	}
	if out := outputField(t, echo, "stdout"); !strings.Contains(out, "hello world") {
		t.Errorf("stdout %q does not contain the echoed arguments", out)
	}
}

func TestExecCommandDoesNotInterpretShellSyntax(t *testing.T) {
	db := openTestDB(t)
	_, token := mustAgentWithToken(t, db, "agent-1")
	registrar := newTestRegistrar(t, db)
	session := connectSession(t, newTestServer(t, registrar, db), token)

	result := callTool(t, session, "exec_command", map[string]any{"command": "pwd && echo hello"})
	if !result.IsError {
		t.Errorf("a compound command was accepted as a program name: %s", contentText(result.Content))
	}

	echoed := callTool(t, session, "exec_command", map[string]any{
		"command": "echo",
		"args":    []string{"a && b | c > d"},
	})
	if echoed.IsError {
		t.Fatalf("exec_command failed: %s", contentText(echoed.Content))
	}
	if out := outputField(t, echoed, "stdout"); !strings.Contains(out, "a && b | c > d") {
		t.Errorf("stdout %q does not contain the argument verbatim", out)
	}
}

func TestExecCommandRejectsAMissingProgram(t *testing.T) {
	db := openTestDB(t)
	_, token := mustAgentWithToken(t, db, "agent-1")
	registrar := newTestRegistrar(t, db)
	session := connectSession(t, newTestServer(t, registrar, db), token)

	result := callTool(t, session, "exec_command", map[string]any{"command": "definitely-not-installed"})
	if !result.IsError {
		t.Fatal("a missing program was reported as success")
	}
	if message := contentText(result.Content); !strings.Contains(message, "not found in PATH") {
		t.Errorf("error %q does not say the program was not found", message)
	}
}
