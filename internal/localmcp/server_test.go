package localmcp_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JetManiack/mcp-sandbox/internal/localexec"
	"github.com/JetManiack/mcp-sandbox/internal/localmcp"
	"github.com/JetManiack/mcp-sandbox/internal/mcpserver"
	sandboxtools "github.com/JetManiack/mcp-sandbox/internal/tools/sandbox"
)

// connectSession wires a client to the server over in-memory transports, so tool
// dispatch and schema generation are exercised the way a real client drives them.
func connectSession(t *testing.T, dir string) (*mcp.ClientSession, *localexec.Runner) {
	t.Helper()

	runner, err := localexec.New(localexec.Config{Dir: dir})
	if err != nil {
		t.Fatalf("localexec.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := localmcp.NewServer(runner, "test")
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return session, runner
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%q): %v", name, err)
	}
	return result
}

func field(t *testing.T, result *mcp.CallToolResult, key string) any {
	t.Helper()
	fields, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content is %T, want a JSON object", result.StructuredContent)
	}
	value, present := fields[key]
	if !present {
		t.Fatalf("structured output has no %q field: %v", key, fields)
	}
	return value
}

func TestAdvertisesItsTools(t *testing.T) {
	session, _ := connectSession(t, t.TempDir())

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

// TestToolNamesMatchTheServer is the test behind the shared vocabulary: the two
// are never connected at the same time, so a client points at one or the other,
// and a prompt written for either has to work against both. Comparing against the
// server's actual registration rather than a copied list is what makes a rename on
// one side fail here instead of surfacing as a tool an agent cannot find.
func TestToolNamesMatchTheServer(t *testing.T) {
	local := toolNames(t, localmcp.NewServer(mustRunner(t, t.TempDir()), "test"))

	// nil executor and nil db: listing tools never reaches a handler, so no
	// sandbox manager or database is needed to ask the server what it advertises.
	registrar := sandboxtools.NewRegistrar(nil, nil)
	server := toolNames(t, mcpserver.NewServer("test", []mcpserver.ToolRegistrar{registrar}, nil))

	slices.Sort(local)
	slices.Sort(server)
	if !slices.Equal(local, server) {
		t.Errorf("tool names differ:\n  local:  %v\n  server: %v", local, server)
	}
}

// toolNames lists what a server advertises, over a real in-memory session.
func toolNames(t *testing.T, server *mcp.Server) []string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	result, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func mustRunner(t *testing.T, dir string) *localexec.Runner {
	t.Helper()
	runner, err := localexec.New(localexec.Config{Dir: dir})
	if err != nil {
		t.Fatalf("localexec.New: %v", err)
	}
	return runner
}

func TestExecCommandReturnsOutput(t *testing.T) {
	session, _ := connectSession(t, t.TempDir())

	result := callTool(t, session, "exec_command", map[string]any{"command": "echo hello && echo world"})
	if result.IsError {
		t.Fatalf("exec_command failed: %v", result.Content)
	}
	if got := field(t, result, "stdout"); !strings.Contains(got.(string), "hello") ||
		!strings.Contains(got.(string), "world") {
		t.Errorf("stdout = %q, want both echoes — the shell should have run the whole line", got)
	}
}

func TestExecCommandReportsANonZeroExit(t *testing.T) {
	session, _ := connectSession(t, t.TempDir())

	result := callTool(t, session, "exec_command", map[string]any{"command": "exit 4"})
	// A command that fails is an answer, not a failed tool call.
	if result.IsError {
		t.Fatalf("exec_command reported a tool error for a non-zero exit: %v", result.Content)
	}
	if got := field(t, result, "exit_code"); got != float64(4) {
		t.Errorf("exit_code = %v, want 4", got)
	}
}

func TestExecCommandRejectsAnEmptyCommand(t *testing.T) {
	session, _ := connectSession(t, t.TempDir())

	result := callTool(t, session, "exec_command", map[string]any{"command": ""})
	if !result.IsError {
		t.Error("an empty command was accepted")
	}
}

func TestExecCommandHonoursItsTimeout(t *testing.T) {
	session, _ := connectSession(t, t.TempDir())

	result := callTool(t, session, "exec_command", map[string]any{"command": "sleep 30", "timeout_sec": 1})
	if !result.IsError {
		t.Error("a command that outran its timeout was reported as success")
	}
}

func TestSandboxStatusSaysItIsNotSandboxed(t *testing.T) {
	dir := t.TempDir()
	session, runner := connectSession(t, dir)

	result := callTool(t, session, "get_sandbox_status", map[string]any{})
	if result.IsError {
		t.Fatalf("get_sandbox_status failed: %v", result.Content)
	}

	status, ok := field(t, result, "status").(map[string]any)
	if !ok {
		t.Fatalf("status is %T, want a JSON object", field(t, result, "status"))
	}
	if status["root_dir"] != runner.Dir() {
		t.Errorf("root_dir = %v, want %q", status["root_dir"], runner.Dir())
	}
	if status["shell"] != runner.Shell() {
		t.Errorf("shell = %v, want %q", status["shell"], runner.Shell())
	}
	// Reported in the tool output, not only in the README: the tool is named
	// get_sandbox_status to match the server, so an agent reading the name alone
	// would assume a sandbox that is not there.
	if status["sandboxed"] != false {
		t.Errorf("sandboxed = %v, want false", status["sandboxed"])
	}
}

// TestTimeoutKeepsPartialOutput covers a defect the SDK's typed-handler path
// makes easy to ship: returning the error to it drops the structured output, so a
// timed-out build reported "timed out" and nothing else. Where it got stuck is
// usually the only thing worth having.
func TestTimeoutKeepsPartialOutput(t *testing.T) {
	session, _ := connectSession(t, t.TempDir())

	result := callTool(t, session, "exec_command", map[string]any{
		"command":     "echo before-the-hang; sleep 30",
		"timeout_sec": 1,
	})
	if !result.IsError {
		t.Fatal("a command that outran its timeout was reported as success")
	}
	if got := field(t, result, "stdout"); !strings.Contains(got.(string), "before-the-hang") {
		t.Errorf("stdout = %q, want the output produced before the timeout", got)
	}

	// Clients that render only the text content should see it too.
	var text strings.Builder
	for _, block := range result.Content {
		if content, ok := block.(*mcp.TextContent); ok {
			text.WriteString(content.Text)
		}
	}
	if !strings.Contains(text.String(), "before-the-hang") {
		t.Errorf("content = %q, want the partial output alongside the reason", text.String())
	}
	if !strings.Contains(text.String(), "timed out") {
		t.Errorf("content = %q, want it to say why the command was cut", text.String())
	}
}

// TestFileToolsRoundTrip exercises the four file tools over a real session, since
// their whole purpose is that a client can call them by the same names it uses
// against the server.
func TestFileToolsRoundTrip(t *testing.T) {
	session, runner := connectSession(t, t.TempDir())

	written := callTool(t, session, "write_file", map[string]any{
		"path":    "nested/notes.txt",
		"content": "hello",
	})
	if written.IsError {
		t.Fatalf("write_file failed: %v", written.Content)
	}
	if got := field(t, written, "bytes"); got != float64(5) {
		t.Errorf("bytes = %v, want 5", got)
	}

	read := callTool(t, session, "read_file", map[string]any{"path": "nested/notes.txt"})
	if read.IsError {
		t.Fatalf("read_file failed: %v", read.Content)
	}
	if got := field(t, read, "content"); got != "hello" {
		t.Errorf("content = %v, want %q", got, "hello")
	}
	if got := field(t, read, "truncated"); got != false {
		t.Errorf("truncated = %v, want false", got)
	}

	listed := callTool(t, session, "list_dir", map[string]any{"path": "nested"})
	if listed.IsError {
		t.Fatalf("list_dir failed: %v", listed.Content)
	}
	files, ok := field(t, listed, "files").([]any)
	if !ok || len(files) != 1 {
		t.Fatalf("files = %v, want one entry", field(t, listed, "files"))
	}

	deleted := callTool(t, session, "delete_file", map[string]any{"path": "nested"})
	if deleted.IsError {
		t.Fatalf("delete_file failed: %v", deleted.Content)
	}
	if got := field(t, deleted, "was_directory"); got != true {
		t.Errorf("was_directory = %v, want true", got)
	}
	if got := field(t, deleted, "existed"); got != true {
		t.Errorf("existed = %v, want true", got)
	}

	if entries, err := runner.ListDir("."); err != nil || len(entries) != 0 {
		t.Errorf("working directory = %+v (err %v), want empty after the delete", entries, err)
	}
}

func TestListDirOnAnEmptyDirectoryReturnsAnArray(t *testing.T) {
	session, _ := connectSession(t, t.TempDir())

	listed := callTool(t, session, "list_dir", map[string]any{})
	if listed.IsError {
		t.Fatalf("list_dir failed: %v", listed.Content)
	}
	// [] rather than null: a client distinguishing "no entries" from "field
	// absent" would otherwise see the latter.
	files, ok := field(t, listed, "files").([]any)
	if !ok {
		t.Fatalf("files is %T, want an array", field(t, listed, "files"))
	}
	if len(files) != 0 {
		t.Errorf("files = %v, want empty", files)
	}
}

func TestFileToolsRejectAnEmptyPath(t *testing.T) {
	session, _ := connectSession(t, t.TempDir())

	for _, tool := range []string{"read_file", "write_file", "delete_file"} {
		result := callTool(t, session, tool, map[string]any{"path": "", "content": "x"})
		if !result.IsError {
			t.Errorf("%s accepted an empty path", tool)
		}
	}
}

// TestExecCommandWithArgsSkipsTheShell is the client-visible half of the superset
// contract: a call written against the server sends args and must get direct
// execution, with shell syntax in an argument left as text.
func TestExecCommandWithArgsSkipsTheShell(t *testing.T) {
	session, _ := connectSession(t, t.TempDir())

	result := callTool(t, session, "exec_command", map[string]any{
		"command": "echo",
		"args":    []string{"a && b > c"},
	})
	if result.IsError {
		t.Fatalf("exec_command failed: %v", result.Content)
	}
	if got := field(t, result, "stdout").(string); !strings.Contains(got, "a && b > c") {
		t.Errorf("stdout = %q, want the argument verbatim", got)
	}

	listed := callTool(t, session, "list_dir", map[string]any{})
	files, _ := field(t, listed, "files").([]any)
	if len(files) != 0 {
		t.Errorf("files = %v, want none — the redirection in an argument was executed", files)
	}
}
