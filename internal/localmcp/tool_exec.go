package localmcp

import (
	"context"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/JetManiack/mcp-sandbox/internal/localexec"
)

// ExecCommandInput is a superset of the server's input of the same name.
//
// With args set it means exactly what it means there — a program and its argument
// vector, executed directly — so a caller written against the server works here
// unchanged. With args omitted, command is a command line for $SHELL, which is
// what makes this helper convenient locally: the agent already has the operator's
// authority, so a shell buys pipes and redirection with nothing to protect.
type ExecCommandInput struct {
	Command    string   `json:"command" jsonschema:"a program to execute when args is set; otherwise a shell command line, where pipes, redirection, globs and && all work"`
	Args       []string `json:"args,omitempty" jsonschema:"arguments passed verbatim to the program; when set, no shell is involved and shell syntax is NOT interpreted"`
	TimeoutSec int      `json:"timeout_sec,omitempty" jsonschema:"optional timeout in seconds; the helper's default applies when unset"`
	WorkDir    string   `json:"work_dir,omitempty" jsonschema:"optional working directory, relative to the helper's directory or absolute"`
}

type ExecCommandOutput struct {
	Stdout     string `json:"stdout" jsonschema:"everything the command wrote to stdout, up to the output cap"`
	Stderr     string `json:"stderr" jsonschema:"everything the command wrote to stderr, up to the output cap"`
	ExitCode   int    `json:"exit_code" jsonschema:"the command's exit status; -1 when it was killed by the timeout"`
	DurationMs int64  `json:"duration_ms" jsonschema:"wall-clock execution time in milliseconds"`
	Truncated  bool   `json:"truncated" jsonschema:"true when output exceeded the cap and was cut"`
}

func execCommandHandler(runner *localexec.Runner) mcp.ToolHandlerFor[ExecCommandInput, ExecCommandOutput] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in ExecCommandInput) (*mcp.CallToolResult, ExecCommandOutput, error) {
		if in.Command == "" {
			return nil, ExecCommandOutput{}, errors.New("command cannot be empty")
		}

		var timeout time.Duration
		if in.TimeoutSec > 0 {
			timeout = time.Duration(in.TimeoutSec) * time.Second
		}

		res, err := runner.Run(ctx, in.Command, in.Args, timeout, in.WorkDir)
		output := ExecCommandOutput{
			Stdout:     res.Stdout,
			Stderr:     res.Stderr,
			ExitCode:   res.ExitCode,
			DurationMs: res.DurationMs,
			Truncated:  res.Truncated,
		}

		// A non-zero exit is the command reporting failure, which is an answer, not
		// a failed tool call.
		switch {
		case errors.Is(err, localexec.ErrCommandTimeout):
			// It ran and was cut short, so keep what it printed: for a build that
			// hung, that output is where it hung.
			return cutShortResult(err, output.Stdout, output.Stderr), output, nil
		case err != nil:
			return nil, output, err
		}
		return nil, output, nil
	}
}

// cutShortResult reports a command that ran and was then cut short, keeping the
// output it produced.
//
// Returning the error to the SDK instead would drop it: the typed-handler path
// only assembles structured content when the handler returns no error, so the
// agent would get "timed out" and nothing else. Partial output is usually the
// most useful thing a timed-out build produces — it is where it got stuck.
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
