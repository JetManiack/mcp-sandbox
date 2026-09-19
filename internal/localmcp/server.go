// Package localmcp is the MCP surface of the local stdio helper: the same six
// tools the server exposes, over stdin/stdout, with no HTTP, no authentication
// and no web UI.
//
// The tool names match the server's deliberately. The two are never connected at
// the same time — a client points at one or the other — so a shared vocabulary
// means a prompt or a client config written for one works against the other.
// Where a contract had to differ it is a superset rather than a variant: see
// ExecCommandInput.
//
// Authentication is absent because there is nothing to authenticate. The client
// is whoever started the process, the transport is that process's own stdin and
// stdout, and a token would only be one the operator hands to themselves.
package localmcp

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JetManiack/mcp-sandbox/internal/localexec"
)

// ServerName is the MCP implementation name advertised to clients. It differs
// from the server binary's so a client's logs say which one it is talking to,
// even though the tools are named the same.
const ServerName = "mcp-sandbox-local"

const fallbackVersion = "dev"

// NewServer builds the MCP server with every tool registered.
func NewServer(runner *localexec.Runner, version string) *mcp.Server {
	if version == "" {
		version = fallbackVersion
	}
	server := mcp.NewServer(&mcp.Implementation{Name: ServerName, Version: version}, nil)
	RegisterTools(server, runner)
	return server
}

// RegisterTools adds every tool this helper exposes to server.
//
// Descriptions state the local differences plainly — a shell is available, paths
// are not confined — rather than reusing the server's wording, which would
// describe a sandbox that is not there.
func RegisterTools(server *mcp.Server, runner *localexec.Runner) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "exec_command",
		Description: "Run a command in the local working directory. With args set, the program is executed directly; with args omitted, command is a shell command line and pipes, redirection, globs and && all work.",
	}, execCommandHandler(runner))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "read_file",
		Description: "Read a file, relative to the local working directory or absolute. Long files are truncated at the output cap and say so.",
	}, readFileHandler(runner))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "write_file",
		Description: "Write text to a file, relative to the local working directory or absolute, creating parent directories",
	}, writeFileHandler(runner))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_dir",
		Description: "List a directory, relative to the local working directory or absolute, non-recursive",
	}, listDirHandler(runner))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "delete_file",
		Description: "Delete a file, or a directory and its whole subtree, relative to the local working directory or absolute",
	}, deleteFileHandler(runner))

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_sandbox_status",
		Description: "Report the working directory, shell and limits in use — and that nothing here is sandboxed",
	}, sandboxStatusHandler(runner))
}

// Serve runs the MCP server over stdin/stdout until ctx is cancelled or the
// client disconnects.
func Serve(ctx context.Context, runner *localexec.Runner, version string) error {
	return NewServer(runner, version).Run(ctx, &mcp.StdioTransport{})
}
