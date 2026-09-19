// Command executor-local is a single-user MCP server over stdin/stdout that runs
// shell command lines in the directory it was started from.
//
// It is the local counterpart to the executor server: no HTTP, no web UI, no
// database, no authentication and no sandbox. The agent acts with the operator's
// own privileges, which is what it already has on a developer's machine.
//
// It writes nothing to stdout but MCP frames — anything else would corrupt the
// protocol — and nothing to stderr unless it cannot start.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli/v3"

	"github.com/JetManiack/mcp-sandbox/internal/localexec"
	"github.com/JetManiack/mcp-sandbox/internal/localmcp"
)

// version is stamped at build time by the Makefile (-X main.version=...).
var version = "dev"

func newRootCommand() *cli.Command {
	return &cli.Command{
		Name:    "executor-local",
		Usage:   "local stdio MCP server: runs shell command lines in the current directory, unsandboxed",
		Version: version,

		// The MCP protocol owns stdout, so usage text and errors must never go
		// there: a help message printed into the stream reads to the client as a
		// malformed frame, and the session dies with nothing explaining why.
		Writer:    os.Stderr,
		ErrWriter: os.Stderr,

		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "dir",
				Usage:   "working directory for commands (default: the directory this was started in)",
				Sources: cli.EnvVars("EXECUTOR_LOCAL_DIR"),
			},
			&cli.StringFlag{
				Name:    "shell",
				Usage:   "shell that runs the command lines (default: $SHELL, then /bin/sh)",
				Sources: cli.EnvVars("EXECUTOR_LOCAL_SHELL"),
			},
			&cli.DurationFlag{
				Name:    "timeout",
				Value:   localexec.DefaultTimeout,
				Usage:   "default per-command timeout when a call does not set one",
				Sources: cli.EnvVars("EXECUTOR_LOCAL_TIMEOUT"),
			},
			&cli.IntFlag{
				Name:    "max-output-bytes",
				Value:   localexec.DefaultMaxOutputBytes,
				Usage:   "per-stream cap on the output a command returns; more than this is truncated",
				Sources: cli.EnvVars("EXECUTOR_LOCAL_MAX_OUTPUT_BYTES"),
			},
		},

		Action: func(ctx context.Context, cmd *cli.Command) error {
			runner, err := localexec.New(localexec.Config{
				Shell:          cmd.String("shell"),
				Dir:            cmd.String("dir"),
				DefaultTimeout: cmd.Duration("timeout"),
				MaxOutputBytes: cmd.Int("max-output-bytes"),
			})
			if err != nil {
				return err
			}

			// Deliberately silent: no startup banner, no request log. The operator
			// asked for a tool their client drives, not for a process narrating
			// itself into their terminal.
			return localmcp.Serve(ctx, runner, version)
		},
	}
}

func main() {
	// SIGTERM and Ctrl-C cancel the session's context, which cancels any command
	// in flight — and, through procexec, its whole process tree, so a build the
	// helper started does not outlive it.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCommand().Run(ctx, os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "executor-local:", err)
		os.Exit(1)
	}
}
