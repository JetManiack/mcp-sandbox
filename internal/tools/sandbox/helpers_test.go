package sandbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/mcpserver"
	"github.com/JetManiack/mcp-sandbox/internal/storage"
	"github.com/JetManiack/mcp-sandbox/internal/workertest"
)

// openTestDB gives each test its own migrated SQLite database.
func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	return db
}

func mustAgent(t *testing.T, db *gorm.DB, name string) *storage.Actor {
	t.Helper()
	agent, err := storage.CreateAgent(db, name)
	if err != nil {
		t.Fatalf("CreateAgent(%q): %v", name, err)
	}
	return agent
}

// mustAgentWithToken registers an agent and issues it a usable bearer token.
func mustAgentWithToken(t *testing.T, db *gorm.DB, name string) (*storage.Actor, string) {
	t.Helper()
	agent := mustAgent(t, db, name)
	token, _, err := storage.IssueAgentToken(db, agent.ID)
	if err != nil {
		t.Fatalf("IssueAgentToken: %v", err)
	}
	return agent, token
}

// newTestRegistrar wires the sandbox tools to a real worker over a real link.
func newTestRegistrar(t *testing.T, db *gorm.DB) *Registrar {
	t.Helper()
	hub := workertest.StartOne(t).Hub
	return NewRegistrar(hub, db)
}

// newTestServer boots the real MCP handler over HTTP.
func newTestServer(t *testing.T, registrar *Registrar, db *gorm.DB) *httptest.Server {
	t.Helper()
	handler := mcpserver.Handler(db, "test", []mcpserver.ToolRegistrar{registrar})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

// bearerTransport attaches a fixed bearer token to every request.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	if b.token != "" {
		clone.Header.Set("Authorization", "Bearer "+b.token)
	}
	base := b.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// callTool invokes name on session and fails the test on a transport-level error.
func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%q): %v", name, err)
	}
	return result
}

// contentText flattens a tool result's content blocks into one string.
func contentText(content []mcp.Content) string {
	var sb strings.Builder
	for _, block := range content {
		if text, ok := block.(*mcp.TextContent); ok {
			sb.WriteString(text.Text)
		}
	}
	return sb.String()
}

// outputField reads one field of a tool's structured output.
func outputField(t *testing.T, result *mcp.CallToolResult, key string) string {
	t.Helper()
	fields, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content is %T, want a JSON object", result.StructuredContent)
	}
	value, present := fields[key]
	if !present {
		t.Fatalf("structured output has no %q field: %v", key, fields)
	}
	text, ok := value.(string)
	if !ok {
		t.Fatalf("field %q is %T, want a string", key, value)
	}
	return text
}

// connectSession returns a connected MCP client session against server.
func connectSession(t *testing.T, server *httptest.Server, token string) *mcp.ClientSession {
	t.Helper()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.1"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:             server.URL,
		HTTPClient:           &http.Client{Transport: bearerTransport{token: token}},
		DisableStandaloneSSE: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}
