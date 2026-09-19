// Package mcpserver exposes this service's sandbox tools over MCP: shell
// execution and filesystem access, each scoped to the calling agent's own
// jailed directory.
package mcpserver

import (
	"context"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/auth"
	"github.com/JetManiack/mcp-sandbox/internal/workerproto"
)

// Executor is where commands actually run: a pool of worker processes reached over
// the worker link. The interface exists so this package depends on the operations
// rather than on the transport, and so a test can drive a fake without standing up
// a worker.
//
// Every method takes the agent ID because execution is no longer in this process:
// the hub uses it to route to the worker holding that agent's sandbox.
type Executor interface {
	Exec(ctx context.Context, agentID string, in workerproto.ExecRequest) (workerproto.ExecResponse, error)
	ReadFile(ctx context.Context, agentID, path string) (string, error)
	WriteFile(ctx context.Context, agentID, path, content string) (int, error)
	ListDir(ctx context.Context, agentID, path string) ([]workerproto.FileInfo, error)
	DeleteFile(ctx context.Context, agentID, path string) (workerproto.DeleteFileResponse, error)
	Status(ctx context.Context, agentID string) (workerproto.StatusResponse, error)
	Kill(ctx context.Context, agentID, byActor, reason string) (int, error)
}

// ToolRegistrar is implemented by domain tool packages. Handler calls Register
// on each registrar to let it add its tools to the MCP server.
type ToolRegistrar interface {
	Register(srv *mcp.Server, db *gorm.DB)
}

// ServerName is the MCP implementation name advertised to clients.
const ServerName = "mcp-sandbox"

// fallbackVersion is reported when no version was stamped at build time.
const fallbackVersion = "dev"

// clearWriteDeadline removes any per-connection write deadline set by the HTTP
// server before the request handler runs. The MCP streamable-HTTP transport
// holds a response open for the lifetime of the session; any finite deadline
// set at the server level would sever it mid-session.
func clearWriteDeadline(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NewResponseController(w).SetWriteDeadline(time.Time{})
		next.ServeHTTP(w, r)
	})
}

// NewServer builds an *mcp.Server with all provided tool registrars applied.
// db may be nil; registrars that use it skip journalling when it is absent.
func NewServer(version string, tools []ToolRegistrar, db *gorm.DB) *mcp.Server {
	v := version
	if v == "" {
		v = fallbackVersion
	}
	srv := mcp.NewServer(&mcp.Implementation{Name: ServerName, Version: v}, nil)
	for _, t := range tools {
		t.Register(srv, db)
	}
	return srv
}

// Handler builds the /mcp HTTP handler from a database and a set of tool
// registrars. Registrars are called in order; the resulting handler is wrapped
// in bearer-token auth and write-deadline clearing.
func Handler(db *gorm.DB, version string, tools []ToolRegistrar) http.Handler {
	srv := NewServer(version, tools, db)
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	return clearWriteDeadline(auth.RequireBearer(db, mcpHandler))
}
