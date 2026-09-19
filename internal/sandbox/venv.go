package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/JetManiack/mcp-sandbox/internal/procexec"
	"github.com/JetManiack/mcp-sandbox/internal/sandboxop"
)

// DefaultVenvDir is where each sandbox gets its Python environment.
//
// Inside the sandbox rather than in the image, because a virtual environment is
// something an agent installs into and the image's filesystem is read-only. One
// per sandbox rather than one shared: `pip install` is a thing agents do, and a
// shared environment would make one agent's dependency the next agent's problem.
const DefaultVenvDir = ".venv"

// venvProvisionTimeout bounds creating the environment. Generous — the first one
// on a cold page cache is slower than it looks — but bounded, because an agent's
// first command is waiting behind it.
const venvProvisionTimeout = 2 * time.Minute

// venvBinDir is the directory a virtual environment puts executables in.
func (s *Sandbox) venvBinDir() string {
	if s.config.VenvDir == "" {
		return ""
	}
	return filepath.Join(s.config.RootDir, s.config.VenvDir, "bin")
}

// ensureVenv creates the sandbox's Python environment the first time it is
// needed, and does nothing after that.
//
// Failure is logged rather than returned: an agent running `go test` should not
// be refused because Python is absent from the image, and the environment being
// missing degrades to the interpreter on PATH rather than to a broken sandbox.
//
// Created on first use rather than when the sandbox is registered, because
// registration happens under the manager's lock — provisioning there would make
// one agent's first call block every other agent's.
func (s *Sandbox) ensureVenv(ctx context.Context) {
	if s.config.VenvDir == "" {
		return
	}
	s.venvOnce.Do(func() {
		if err := s.createVenv(ctx); err != nil {
			slog.Warn("could not create the sandbox's Python environment; commands will use the interpreter on PATH",
				"sandbox_id", s.id, "error", err)
		}
	})
}

// createVenv runs the interpreter's own venv module inside the sandbox.
//
// It goes through the same fork-and-drop-privileges path an agent's command does,
// which is not incidental: the environment has to belong to the agent, or the
// first `pip install` fails on a directory the worker owns.
func (s *Sandbox) createVenv(ctx context.Context) error {
	python, err := s.pythonProgram()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, venvProvisionTimeout)
	defer cancel()

	// The environment deliberately excludes the venv itself — it does not exist
	// yet, and pointing PATH at it while creating it would be circular.
	// #nosec G204 -- the program is resolved from the operator's own configuration
	// and the arguments are compiled in; nothing an agent supplies reaches here.
	cmd := exec.CommandContext(ctx, python, "-m", "venv", s.config.VenvDir)
	cmd.Dir = s.config.RootDir
	cmd.Env = buildEnv(s.config, s.config.RootDir, "")
	procexec.Configure(cmd)
	procexec.DropTo(cmd, s.uid)

	output, err := cmd.CombinedOutput()
	if err != nil {
		// The interpreter's own message, because "exit status 1" from `python -m
		// venv` is almost always "ensurepip is not available" on a distribution
		// that packages it separately, and that is a one-package fix an operator
		// can act on.
		return fmt.Errorf("create %s with %s: %w: %s", s.config.VenvDir, python, err, output)
	}

	slog.Info("created the sandbox's Python environment", "sandbox_id", s.id, "dir", s.config.VenvDir)
	return nil
}

// pythonProgram finds the interpreter to build the environment with.
func (s *Sandbox) pythonProgram() (string, error) {
	candidates := []string{s.config.PythonProgram}
	if s.config.PythonProgram == "" {
		// python3 first: on a system with both, `python` is as likely to be a
		// Python 2 left over as it is to be a symlink to 3.
		candidates = []string{"python3", "python"}
	}

	env := buildEnv(s.config, s.config.RootDir, "")
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if resolved, err := lookProgram(candidate, env); err == nil {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("no Python interpreter on the sandbox's PATH (looked for %v)", candidates)
}

// lookProgramForAgent resolves a program name to a path the agent can execute.
//
// With per-agent ids the search has to happen in a child running as the agent:
// the sandbox's virtual environment is first on PATH and the worker cannot read
// inside the sandbox, so resolving here would report every interpreter in it as
// missing.
func (s *Sandbox) lookProgramForAgent(program string, env []string) (string, error) {
	if s.ops == nil {
		return lookProgram(program, env)
	}

	var path string
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "PATH="); ok {
			path = value
		}
	}

	out, err := s.ops.Do(context.Background(), s.uid, sandboxop.Request{
		Root: s.config.RootDir, Op: sandboxop.OpLook, Name: program, PathEnv: path,
	})
	if err != nil {
		return "", err
	}
	if err := out.Err(); err != nil {
		return "", fmt.Errorf("%q not found in PATH (%s): %w", program, path, err)
	}
	return out.Program, nil
}
