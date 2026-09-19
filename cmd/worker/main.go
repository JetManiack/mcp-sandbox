// Command worker runs the commands agents ask for. It dials the server, serves
// the operations that arrive on that connection against local sandboxes, and
// streams their output back.
//
// It holds no database, no OIDC secrets and no listening socket, and it never
// learns an agent's token — everything it knows about a request arrives in the
// request. That is what makes it something a cluster can lock down: no inbound
// NetworkPolicy, no service account token, a read-only root filesystem and an
// emptyDir for the sandboxes.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"

	"github.com/JetManiack/mcp-sandbox/internal/sandbox"
	"github.com/JetManiack/mcp-sandbox/internal/sandboxop"
	"github.com/JetManiack/mcp-sandbox/internal/workerlink"
)

// version is stamped at build time by the Makefile (-X main.version=...).
var version = "dev"

func newRootCommand() *cli.Command {
	return &cli.Command{
		Name:    "worker",
		Usage:   "execution worker: dials the executor server and runs agents' commands in local sandboxes",
		Version: version,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "server-url",
				Usage:   "the server's base URL, e.g. http://executor:8080 (ws:// and wss:// are accepted too)",
				Sources: cli.EnvVars("SERVER_URL"),
			},
			&cli.StringFlag{
				Name:    "worker-token",
				Usage:   "shared secret this worker presents to the server; must match the server's --worker-token",
				Sources: cli.EnvVars("WORKER_TOKEN"),
			},
			&cli.StringFlag{
				Name: "worker-token-file",
				// Preferred over the flag and the variable, and the reason is
				// specific rather than hygienic: commands run as this process's own
				// user, so an agent can read /proc/1/environ and take anything in
				// this worker's environment. The token is the only credential here,
				// and with it an agent can register as a worker and be handed other
				// agents' commands.
				//
				// A file does not close that — same user, same read — but it takes
				// the credential out of the one place a single line of shell dumps
				// wholesale, and it is what makes a future UID split enforceable by
				// file ownership. See "What exec_command is not" in the README.
				Usage:   "read the shared secret from this file instead of a flag or the environment (preferred: keeps it out of /proc/<pid>/environ)",
				Sources: cli.EnvVars("WORKER_TOKEN_FILE"),
			},
			&cli.StringFlag{
				Name: "worker-id",
				// Defaulting to the hostname makes a pod name the worker name,
				// which is what an operator reading the sandbox list wants to see.
				Usage:   "name this worker reports to the server (default: hostname, which in a pod is the pod name)",
				Sources: cli.EnvVars("WORKER_ID", "HOSTNAME"),
			},
			&cli.StringFlag{
				Name:    "sandbox-dir",
				Value:   "/sandboxes",
				Usage:   "root directory under which each agent gets its own jailed sandbox; mount an emptyDir here",
				Sources: cli.EnvVars("SANDBOX_DIR"),
			},
			&cli.DurationFlag{
				Name:    "default-timeout",
				Value:   30 * time.Second,
				Usage:   "default per-command execution timeout, when the caller doesn't set one",
				Sources: cli.EnvVars("DEFAULT_TIMEOUT"),
			},
			&cli.IntFlag{
				Name:    "max-output-bytes",
				Value:   512 << 10,
				Usage:   "maximum stdout/stderr a single command returns; longer output is truncated",
				Sources: cli.EnvVars("MAX_OUTPUT_BYTES"),
			},
			&cli.IntFlag{
				Name:  "max-file-bytes",
				Value: workerlink.DefaultMaxFileBytes,
				// This and --max-output-bytes are what the server sizes the
				// connection from, so raising either raises the memory a frame may
				// occupy on both sides.
				Usage:   "maximum content of one read_file or write_file; larger files are refused, not truncated",
				Sources: cli.EnvVars("MAX_FILE_BYTES"),
			},
			&cli.StringSliceFlag{
				Name:    "env-passthrough",
				Value:   sandbox.DefaultEnvPassthrough,
				Usage:   "environment variable names sandboxed commands inherit from this process; everything else is dropped",
				Sources: cli.EnvVars("ENV_PASSTHROUGH"),
			},
			&cli.StringFlag{
				Name:  "venv-dir",
				Value: sandbox.DefaultVenvDir,
				// Created on the agent's first command and put on every command's
				// PATH, which is all activation is — there is no shell here to
				// source a script in. Empty disables it.
				Usage:   "Python virtual environment created in each sandbox and put on every command's PATH; empty disables it",
				Sources: cli.EnvVars("VENV_DIR"),
			},
			&cli.StringFlag{
				Name:    "python",
				Usage:   "interpreter used to create each sandbox's environment (default: python3, then python)",
				Sources: cli.EnvVars("PYTHON"),
			},
			&cli.StringFlag{
				Name:    "uid-range",
				Usage:   "give each agent its own user id from this range, e.g. 20000-20999 — requires CAP_SETUID/CAP_SETGID/CAP_CHOWN/CAP_KILL; unset runs every agent as this process's user",
				Sources: cli.EnvVars("UID_RANGE"),
			},
			&cli.StringSliceFlag{
				Name:    "env",
				Usage:   "extra KEY=VALUE entries for every sandboxed command (repeatable) — where a token an agent's task genuinely needs belongs",
				Sources: cli.EnvVars("SANDBOX_ENV"),
			},
		},

		Commands: []*cli.Command{opHelperSubcommand()},

		Action: func(ctx context.Context, cmd *cli.Command) error {
			workerID := cmd.String("worker-id")
			if workerID == "" {
				hostname, err := os.Hostname()
				if err != nil {
					return fmt.Errorf("determine a worker id: %w", err)
				}
				workerID = hostname
			}

			uidRange, err := parseUIDRange(cmd.String("uid-range"))
			if err != nil {
				return err
			}
			if !uidRange.Enabled() {
				// Said out loud, because the difference is not visible from the
				// outside: without it every agent shares this process's user, so one
				// agent can read another's files and this worker's own token.
				slog.Warn("no --uid-range configured: agents are not isolated from each other or from this worker's credentials")
			}

			token, err := workerToken(cmd)
			if err != nil {
				return err
			}

			passthrough := cmd.StringSlice("env-passthrough")
			if err := sandbox.ValidateEnvPassthrough(passthrough); err != nil {
				return err
			}
			// Not refused, only surfaced: an agent whose task is to call an API does
			// need its token, and only the operator knows which variable that is.
			if suspicious := sandbox.SuspiciousEnvNames(passthrough); len(suspicious) > 0 {
				slog.Warn("passing possibly-sensitive environment variables to sandboxed commands", "names", suspicious)
			}

			link, err := workerlink.New(workerlink.Config{
				ServerURL:    cmd.String("server-url"),
				Token:        token,
				WorkerID:     workerID,
				Version:      version,
				MaxFileBytes: cmd.Int("max-file-bytes"),
			}, sandbox.Config{
				RootDir:        cmd.String("sandbox-dir"),
				DefaultTimeout: cmd.Duration("default-timeout"),
				MaxOutputBytes: cmd.Int("max-output-bytes"),
				EnvPassthrough: passthrough,
				ExtraEnv:       cmd.StringSlice("env"),
				UIDRange:       uidRange,
				OpHelperArgs:   []string{opHelperCommand},
				VenvDir:        cmd.String("venv-dir"),
				PythonProgram:  cmd.String("python"),
			})
			if err != nil {
				return err
			}

			slog.Info("worker starting", "worker_id", workerID, "server", cmd.String("server-url"),
				"sandbox_dir", cmd.String("sandbox-dir"), "version", version)
			return link.Run(ctx)
		},
	}
}

func main() {
	// SIGTERM is how k8s asks a pod to stop. Cancelling the context ends the link
	// and cancels every command in flight, which tears down their process groups —
	// so a scale-down does not leave builds running in a pod that is going away.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newRootCommand().Run(ctx, os.Args); err != nil {
		slog.Error("worker exited with error", "error", err)
		os.Exit(1)
	}
}

// workerToken resolves the shared secret, preferring the file over the flag.
//
// Whitespace is trimmed because a secret written with a here-doc or an editor
// arrives with a trailing newline, and a token that differs from the server's by
// one invisible byte fails as "invalid worker token" — an authentication error
// that sends an operator looking at the wrong thing entirely.
func workerToken(cmd *cli.Command) (string, error) {
	path := cmd.String("worker-token-file")
	if path == "" {
		return cmd.String("worker-token"), nil
	}

	// #nosec G304 -- the path is an operator's own flag, read once at startup.
	// Nothing an agent can influence reaches here: agents arrive over a
	// connection this function's result is needed to establish.
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the worker token from %s: %w", path, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("the worker token file %s is empty", path)
	}
	if cmd.String("worker-token") != "" {
		slog.Warn("both --worker-token and --worker-token-file are set; using the file",
			"file", path)
	}
	return token, nil
}

// opHelperCommand is the argument that selects the file-operation helper.
const opHelperCommand = "sandbox-op"

// opHelperSubcommand is how this binary re-enters itself as an agent.
//
// Not a separate binary, so the image holds one thing and the two halves cannot
// drift apart. It is hidden because it is not an operator's interface: the worker
// invokes it, having already dropped to the agent's user, and it reads one
// request from stdin and writes one response to stdout.
func opHelperSubcommand() *cli.Command {
	return &cli.Command{
		Name:   opHelperCommand,
		Hidden: true,
		Usage:  "internal: perform one sandbox file operation, reading a request on stdin",
		Action: func(_ context.Context, _ *cli.Command) error {
			return sandboxop.Serve(os.Stdin, os.Stdout)
		},
	}
}

// parseUIDRange reads a "first-last" range, inclusive.
func parseUIDRange(spec string) (sandbox.UIDRange, error) {
	if spec == "" {
		return sandbox.UIDRange{}, nil
	}

	first, last, ok := strings.Cut(spec, "-")
	if !ok {
		return sandbox.UIDRange{}, fmt.Errorf("uid range %q must be written first-last, e.g. 20000-20999", spec)
	}
	from, err := strconv.ParseUint(strings.TrimSpace(first), 10, 32)
	if err != nil {
		return sandbox.UIDRange{}, fmt.Errorf("uid range %q: %w", spec, err)
	}
	to, err := strconv.ParseUint(strings.TrimSpace(last), 10, 32)
	if err != nil {
		return sandbox.UIDRange{}, fmt.Errorf("uid range %q: %w", spec, err)
	}
	if to < from {
		return sandbox.UIDRange{}, fmt.Errorf("uid range %q ends before it starts", spec)
	}

	r := sandbox.UIDRange{First: uint32(from), Count: uint32(to - from + 1)}
	return r, r.Validate()
}
