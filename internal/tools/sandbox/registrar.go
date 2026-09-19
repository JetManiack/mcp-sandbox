// Package sandbox provides the MCP tools for shell execution and filesystem
// access, each scoped to the calling agent's own jailed directory.
package sandbox

import (
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/mcpserver"
)

// Registrar holds the executor and wires the six sandbox tools onto an MCP
// server. It implements mcpserver.ToolRegistrar.
type Registrar struct {
	executor mcpserver.Executor
	db       *gorm.DB
}

// NewRegistrar returns a Registrar that routes tool calls through executor.
// db may be nil in single-user local mode; journalling is skipped when it is.
func NewRegistrar(executor mcpserver.Executor, db *gorm.DB) *Registrar {
	return &Registrar{executor: executor, db: db}
}

// Register adds the six sandbox tools to srv. db is the audit database; it
// shadows r.db so the caller can override it per server if needed.
func (r *Registrar) Register(srv *mcp.Server, db *gorm.DB) {
	// Use the db supplied by Handler (which may differ from the one the
	// Registrar was constructed with in tests).
	ar := &Registrar{executor: r.executor, db: db}

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "exec_command",
		Description: "Execute a shell command inside the sandbox directory with timeout and environment isolation",
	}, audited(ar, "exec_command", func(in ExecCommandInput) string {
		return fmt.Sprintf("%q", append([]string{in.Command}, in.Args...))
	}, execCommandHandler(ar)))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "read_file",
		Description: "Read file contents relative to the sandbox directory",
	}, audited(ar, "read_file", func(in ReadFileInput) string { return in.Path }, readFileHandler(ar)))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "write_file",
		Description: "Write text content to a file relative to the sandbox directory",
	}, audited(ar, "write_file", func(in WriteFileInput) string { return in.Path }, writeFileHandler(ar)))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_dir",
		Description: "List files and subdirectories inside the sandbox directory",
	}, audited(ar, "list_dir", func(in ListDirInput) string { return in.Path }, listDirHandler(ar)))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_file",
		Description: "Delete a file or directory inside the sandbox directory",
	}, audited(ar, "delete_file", func(in DeleteFileInput) string { return in.Path }, deleteFileHandler(ar)))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_sandbox_status",
		Description: "Get configuration status and root path of the sandbox environment",
	}, audited(ar, "get_sandbox_status", nil, sandboxStatusHandler(ar)))
}
