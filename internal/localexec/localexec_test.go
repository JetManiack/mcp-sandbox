package localexec_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JetManiack/mcp-sandbox/internal/localexec"
)

func newRunner(t *testing.T, cfg localexec.Config) *localexec.Runner {
	t.Helper()
	runner, err := localexec.New(cfg)
	if err != nil {
		t.Fatalf("localexec.New: %v", err)
	}
	return runner
}

func TestRunsInTheConfiguredDirectory(t *testing.T) {
	dir := t.TempDir()
	runner := newRunner(t, localexec.Config{Dir: dir})

	res, err := runner.Run(context.Background(), "pwd", nil, 10*time.Second, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The temp dir may be reached through a symlink (/var vs /private/var on
	// macOS), so compare the resolved paths.
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	got, err := filepath.EvalSymlinks(strings.TrimSpace(res.Stdout))
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", res.Stdout, err)
	}
	if got != wantDir {
		t.Errorf("pwd = %q, want %q", got, wantDir)
	}
}

// TestDefaultsToTheProcessWorkingDirectory is the behaviour the helper exists for:
// started in a repository, it operates on that repository.
func TestDefaultsToTheProcessWorkingDirectory(t *testing.T) {
	runner := newRunner(t, localexec.Config{})

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	wantDir, err := filepath.EvalSymlinks(wd)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	gotDir, err := filepath.EvalSymlinks(runner.Dir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if gotDir != wantDir {
		t.Errorf("Dir() = %q, want the process working directory %q", gotDir, wantDir)
	}
}

// TestShellFeaturesWork is the whole point of taking a command line rather than an
// argument vector: locally the agent has the operator's authority already, so
// there is nothing to protect by refusing to interpret a pipeline.
func TestShellFeaturesWork(t *testing.T) {
	runner := newRunner(t, localexec.Config{Dir: t.TempDir()})

	tests := []struct {
		name    string
		command string
		want    string
	}{
		{name: "pipe", command: "printf 'b\\na\\n' | sort | tr -d '\\n'", want: "ab"},
		{name: "sequencing", command: "echo one && echo two", want: "one\ntwo\n"},
		{name: "redirection", command: "echo hello > out.txt && cat out.txt", want: "hello\n"},
		{name: "variable", command: "x=5; echo $x", want: "5\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := runner.Run(context.Background(), tt.command, nil, 10*time.Second, "")
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if res.Stdout != tt.want {
				t.Errorf("stdout = %q, want %q", res.Stdout, tt.want)
			}
		})
	}
}

func TestUsesTheConfiguredShell(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "shell-was-used")

	// A fake shell that records it was invoked, so this asserts the configured
	// shell actually runs the command rather than inferring it from output.
	fakeShell := filepath.Join(dir, "fake-shell")
	script := "#!/bin/sh\necho \"$2\" > " + marker + "\nexec /bin/sh \"$@\"\n"
	if err := os.WriteFile(fakeShell, []byte(script), 0o700); err != nil {
		t.Fatalf("write the fake shell: %v", err)
	}

	runner := newRunner(t, localexec.Config{Dir: dir, Shell: fakeShell})
	if runner.Shell() != fakeShell {
		t.Fatalf("Shell() = %q, want %q", runner.Shell(), fakeShell)
	}
	if _, err := runner.Run(context.Background(), "echo hi", nil, 10*time.Second, ""); err != nil {
		t.Fatalf("Run: %v", err)
	}

	recorded, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("the configured shell was not used: %v", err)
	}
	if strings.TrimSpace(string(recorded)) != "echo hi" {
		t.Errorf("the shell received %q, want %q", strings.TrimSpace(string(recorded)), "echo hi")
	}
}

func TestFallsBackToShellEnvThenBinSh(t *testing.T) {
	t.Setenv("SHELL", "/bin/bash")
	if got := newRunner(t, localexec.Config{Dir: t.TempDir()}).Shell(); got != "/bin/bash" {
		t.Errorf("Shell() = %q, want $SHELL", got)
	}

	if err := os.Unsetenv("SHELL"); err != nil {
		t.Fatalf("unset SHELL: %v", err)
	}
	if got := newRunner(t, localexec.Config{Dir: t.TempDir()}).Shell(); got != localexec.FallbackShell {
		t.Errorf("Shell() = %q, want %q", got, localexec.FallbackShell)
	}
}

// TestInheritsTheOperatorEnvironment is the deliberate difference from the server,
// which allowlists: this process holds no credentials of its own, and a command
// the operator would have run in their terminal should see what their terminal
// sees.
func TestInheritsTheOperatorEnvironment(t *testing.T) {
	t.Setenv("MY_LOCAL_TOOL_CONFIG", "visible-to-the-command")
	runner := newRunner(t, localexec.Config{Dir: t.TempDir()})

	res, err := runner.Run(context.Background(), "echo $MY_LOCAL_TOOL_CONFIG", nil, 10*time.Second, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "visible-to-the-command" {
		t.Errorf("stdout = %q, want the inherited variable", res.Stdout)
	}
}

func TestNonZeroExitIsReportedNotAnError(t *testing.T) {
	runner := newRunner(t, localexec.Config{Dir: t.TempDir()})

	res, err := runner.Run(context.Background(), "echo out; echo err 1>&2; exit 7", nil, 10*time.Second, "")
	if err != nil {
		t.Fatalf("Run returned an error for a non-zero exit: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit code = %d, want 7", res.ExitCode)
	}
	if !strings.Contains(res.Stdout, "out") || !strings.Contains(res.Stderr, "err") {
		t.Errorf("output lost: stdout=%q stderr=%q", res.Stdout, res.Stderr)
	}
}

func TestTimeoutIsReported(t *testing.T) {
	runner := newRunner(t, localexec.Config{Dir: t.TempDir()})

	res, err := runner.Run(context.Background(), "sleep 30", nil, 300*time.Millisecond, "")
	if !errors.Is(err, localexec.ErrCommandTimeout) {
		t.Fatalf("error = %v, want ErrCommandTimeout", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("exit code = %d, want -1", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "timed out") {
		t.Errorf("stderr = %q, want it to say the command timed out", res.Stderr)
	}
}

// TestTimeoutTearsDownBackgroundedChildren checks that the local helper keeps the
// mechanic that is about correctness rather than confinement: a `make` that
// spawned workers must not leave them behind.
func TestTimeoutTearsDownBackgroundedChildren(t *testing.T) {
	runner := newRunner(t, localexec.Config{Dir: t.TempDir()})

	res, err := runner.Run(context.Background(), "sleep 60 & echo $!; sleep 60", nil, 500*time.Millisecond, "")
	if !errors.Is(err, localexec.ErrCommandTimeout) {
		t.Fatalf("error = %v, want ErrCommandTimeout", err)
	}

	pid := strings.TrimSpace(res.Stdout)
	if pid == "" {
		t.Fatal("the backgrounded child's PID was not captured")
	}

	// `kill -0` is the portable existence check, and it runs as the same user.
	check := newRunner(t, localexec.Config{Dir: t.TempDir()})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		probe, probeErr := check.Run(context.Background(), "kill -0 "+pid+" 2>/dev/null; echo $?", nil, 5*time.Second, "")
		if probeErr != nil {
			t.Fatalf("probe: %v", probeErr)
		}
		if strings.TrimSpace(probe.Stdout) != "0" {
			return // gone
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("backgrounded child %s outlived the timeout", pid)
}

func TestOutputIsCapped(t *testing.T) {
	runner := newRunner(t, localexec.Config{Dir: t.TempDir(), MaxOutputBytes: 512})

	res, err := runner.Run(context.Background(), "for i in $(seq 1 5000); do echo line-$i; done", nil, 30*time.Second, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Truncated {
		t.Error("result is not marked truncated although output exceeded the cap")
	}
	if len(res.Stdout) > 512 {
		t.Errorf("stdout is %d bytes, want at most the 512-byte cap", len(res.Stdout))
	}
}

func TestWorkDirIsRelativeToTheConfiguredDirectory(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "sub")
	if err := os.Mkdir(nested, 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	runner := newRunner(t, localexec.Config{Dir: dir})

	res, err := runner.Run(context.Background(), "pwd", nil, 10*time.Second, "sub")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := filepath.EvalSymlinks(strings.TrimSpace(res.Stdout))
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	want, err := filepath.EvalSymlinks(nested)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if got != want {
		t.Errorf("pwd = %q, want %q", got, want)
	}
}

// TestWorkDirIsNotConfined states the documented boundary rather than leaving a
// reader to assume the sandbox's containment applies here too.
func TestWorkDirIsNotConfined(t *testing.T) {
	runner := newRunner(t, localexec.Config{Dir: t.TempDir()})

	// Deliberately allowed: this helper confines nothing, because the agent
	// already has the operator's authority. Refusing here would be a gesture, not
	// a boundary — the same command could `cd` itself.
	res, err := runner.Run(context.Background(), "pwd", nil, 10*time.Second, "/")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "/" {
		t.Errorf("pwd = %q, want /", res.Stdout)
	}
}

func TestRejectsAnEmptyCommand(t *testing.T) {
	runner := newRunner(t, localexec.Config{Dir: t.TempDir()})

	if _, err := runner.Run(context.Background(), "", nil, 10*time.Second, ""); err == nil {
		t.Error("an empty command was accepted")
	}
}

func TestRejectsAMissingDirectory(t *testing.T) {
	if _, err := localexec.New(localexec.Config{Dir: filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Error("a nonexistent working directory was accepted")
	}

	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := localexec.New(localexec.Config{Dir: file}); err == nil {
		t.Error("a file was accepted as a working directory")
	}
}

func TestDefaultsAreApplied(t *testing.T) {
	runner := newRunner(t, localexec.Config{Dir: t.TempDir()})

	if runner.DefaultTimeout() != localexec.DefaultTimeout {
		t.Errorf("DefaultTimeout() = %v, want %v", runner.DefaultTimeout(), localexec.DefaultTimeout)
	}
	if runner.MaxOutputBytes() != localexec.DefaultMaxOutputBytes {
		t.Errorf("MaxOutputBytes() = %d, want %d", runner.MaxOutputBytes(), localexec.DefaultMaxOutputBytes)
	}
}
