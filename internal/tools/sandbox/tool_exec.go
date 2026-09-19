package sandbox

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	sandboxpkg "github.com/JetManiack/mcp-sandbox/internal/sandbox"
	"github.com/JetManiack/mcp-sandbox/internal/storage"
	"github.com/JetManiack/mcp-sandbox/internal/workerproto"
)

type ExecCommandInput struct {
	// Command and Args are separate because the program is executed directly,
	// with no shell in between. A single string would have to be split by
	// something, and every splitter either guesses at quoting or reintroduces a
	// shell — so the caller states the boundaries instead.
	Command string   `json:"command" jsonschema:"the program to execute: a bare name resolved against PATH, or a path relative to the sandbox (./build.sh)"`
	Args    []string `json:"args,omitempty" jsonschema:"arguments passed to the program verbatim; no shell is involved, so pipes, redirections, globs and && are NOT interpreted and are passed through as literal text"`

	TimeoutSec int    `json:"timeout_sec,omitempty" jsonschema:"optional execution timeout in seconds (default: the server's configured timeout)"`
	WorkDir    string `json:"work_dir,omitempty" jsonschema:"optional working directory relative to sandbox root"`
}

type ExecCommandOutput struct {
	Stdout     string `json:"stdout" jsonschema:"everything the command wrote to stdout, up to the server's output cap"`
	Stderr     string `json:"stderr" jsonschema:"everything the command wrote to stderr, up to the server's output cap"`
	ExitCode   int    `json:"exit_code" jsonschema:"the command's exit status; -1 when it was killed by a timeout or stopped by an administrator"`
	DurationMs int64  `json:"duration_ms" jsonschema:"wall-clock execution time in milliseconds"`
	Truncated  bool   `json:"truncated" jsonschema:"true when output exceeded the server's cap and was cut"`

	// outcome is how this handler tells the journal what it decided, and it is
	// unexported because it is not part of the tool's contract — an agent learns
	// the same thing from the error text it already gets.
	outcome string
}

func execCommandHandler(r *Registrar) mcp.ToolHandlerFor[ExecCommandInput, ExecCommandOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in ExecCommandInput) (*mcp.CallToolResult, ExecCommandOutput, error) {
		agentID, err := agentForCall(ctx, r.db)
		if err != nil {
			return nil, ExecCommandOutput{}, err
		}

		res, execErr := r.executor.Exec(ctx, agentID, workerproto.ExecRequest{
			Command:    in.Command,
			Args:       in.Args,
			TimeoutSec: in.TimeoutSec,
			WorkDir:    in.WorkDir,
		})
		output := ExecCommandOutput{
			Stdout:     res.Stdout,
			Stderr:     res.Stderr,
			ExitCode:   res.ExitCode,
			DurationMs: res.DurationMs,
			Truncated:  res.Truncated,
		}

		// A command that exits non-zero is a successful tool call reporting a
		// failed command, so only a genuine execution failure becomes a tool
		// error.
		switch {
		case errors.Is(execErr, sandboxpkg.ErrCommandTimeout):
			output.outcome = storage.AuditOutcomeTimeout
			return cutShortResult(execErr, output.Stdout, output.Stderr), output, nil
		case errors.Is(execErr, sandboxpkg.ErrCommandStopped):
			output.outcome = storage.AuditOutcomeStopped
			return cutShortResult(execErr, output.Stdout, output.Stderr), output, nil
		case execErr != nil:
			return nil, output, execErr
		}
		return nil, output, nil
	}
}

// cutShortResult reports a command that ran and was then cut short, keeping the
// output it produced.
func cutShortResult(reason error, stdout, stderr string) *mcp.CallToolResult {
	text := reason.Error()
	if stdout != "" || stderr != "" {
		text += "\n\npartial output before the command was cut:\n--- stdout ---\n" + stdout +
			"\n--- stderr ---\n" + stderr
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
	}
}

// Audit interface implementations for ExecCommandOutput.
func (o ExecCommandOutput) auditBytes() int      { return len(o.Stdout) + len(o.Stderr) }
func (o ExecCommandOutput) auditOutcome() string { return o.outcome }
func (o ExecCommandOutput) auditExitCode() int   { return o.ExitCode }
