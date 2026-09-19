package localexec_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JetManiack/mcp-sandbox/internal/localexec"
)

func TestReadAndWriteFile(t *testing.T) {
	dir := t.TempDir()
	runner := newRunner(t, localexec.Config{Dir: dir})

	if err := runner.WriteFile("nested/deep/hello.txt", []byte("world")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Parent directories are created, as the server's write_file does.
	onDisk, err := os.ReadFile(filepath.Join(dir, "nested/deep/hello.txt"))
	if err != nil {
		t.Fatalf("read back from disk: %v", err)
	}
	if string(onDisk) != "world" {
		t.Errorf("on disk = %q, want %q", onDisk, "world")
	}

	content, truncated, err := runner.ReadFile("nested/deep/hello.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if content != "world" || truncated {
		t.Errorf("ReadFile = %q (truncated %v), want %q and false", content, truncated, "world")
	}
}

// TestReadFileIsCapped is a difference from the server's read_file, which returns
// a file whole however large it is. Here the contents travel back inside one MCP
// response, so an unbounded read would let a caller exhaust this process.
func TestReadFileIsCapped(t *testing.T) {
	dir := t.TempDir()
	runner := newRunner(t, localexec.Config{Dir: dir, MaxOutputBytes: 64})

	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(strings.Repeat("x", 4096)), 0o600); err != nil {
		t.Fatalf("seed the file: %v", err)
	}

	content, truncated, err := runner.ReadFile("big.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !truncated {
		t.Error("a file larger than the cap was not reported as truncated")
	}
	if len(content) > 64 {
		t.Errorf("content is %d bytes, want at most the 64-byte cap", len(content))
	}
}

func TestReadFileAtExactlyTheCapIsNotTruncated(t *testing.T) {
	dir := t.TempDir()
	runner := newRunner(t, localexec.Config{Dir: dir, MaxOutputBytes: 16})

	if err := os.WriteFile(filepath.Join(dir, "exact.txt"), []byte(strings.Repeat("x", 16)), 0o600); err != nil {
		t.Fatalf("seed the file: %v", err)
	}

	content, truncated, err := runner.ReadFile("exact.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	// A file of exactly the cap is complete; reporting it as cut would make every
	// boundary-sized read look lossy.
	if truncated {
		t.Error("a file of exactly the cap was reported as truncated")
	}
	if len(content) != 16 {
		t.Errorf("content is %d bytes, want 16", len(content))
	}
}

func TestReadFileRejectsADirectory(t *testing.T) {
	runner := newRunner(t, localexec.Config{Dir: t.TempDir()})

	if _, _, err := runner.ReadFile("."); err == nil {
		t.Error("reading a directory succeeded")
	}
}

func TestListDir(t *testing.T) {
	dir := t.TempDir()
	runner := newRunner(t, localexec.Config{Dir: dir})

	if err := runner.WriteFile("a.txt", []byte("a")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o750); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	entries, err := runner.ListDir(".")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2: %+v", len(entries), entries)
	}

	byName := map[string]localexec.FileInfo{}
	for _, entry := range entries {
		byName[entry.Name] = entry
	}
	if file, ok := byName["a.txt"]; !ok || file.IsDir || file.Size != 1 {
		t.Errorf("a.txt = %+v, want a 1-byte file", file)
	}
	if sub, ok := byName["sub"]; !ok || !sub.IsDir {
		t.Errorf("sub = %+v, want a directory", sub)
	}
	// Paths come back relative to the working directory, so they can be handed
	// straight back to the other tools.
	if byName["a.txt"].Path != "a.txt" {
		t.Errorf("path = %q, want %q", byName["a.txt"].Path, "a.txt")
	}
}

func TestListDirReportsNestedPathsRelativeToTheRoot(t *testing.T) {
	dir := t.TempDir()
	runner := newRunner(t, localexec.Config{Dir: dir})

	if err := runner.WriteFile("sub/inner.txt", []byte("x")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	entries, err := runner.ListDir("sub")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Path != filepath.Join("sub", "inner.txt") {
		t.Errorf("entries = %+v, want one entry at sub/inner.txt", entries)
	}
}

func TestDeleteFileReportsWhatItRemoved(t *testing.T) {
	dir := t.TempDir()
	runner := newRunner(t, localexec.Config{Dir: dir})

	if err := runner.WriteFile("tree/one.txt", []byte("x")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	file, err := runner.DeleteFile("tree/one.txt")
	if err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if !file.Existed || file.WasDirectory {
		t.Errorf("result = %+v, want an existing non-directory", file)
	}

	tree, err := runner.DeleteFile("tree")
	if err != nil {
		t.Fatalf("DeleteFile (directory): %v", err)
	}
	if !tree.Existed || !tree.WasDirectory {
		t.Errorf("result = %+v, want an existing directory", tree)
	}

	// RemoveAll succeeds on a path that was never there, so the result is the only
	// way a caller can tell that apart from a real deletion.
	missing, err := runner.DeleteFile("never-existed")
	if err != nil {
		t.Fatalf("DeleteFile (missing): %v", err)
	}
	if missing.Existed {
		t.Errorf("result = %+v, want Existed false", missing)
	}
}

// TestDeleteRefusesTheCatastrophicTargets is not confinement — the same agent can
// delete anything else the operator can — but both of these are almost certainly a
// mistake rather than an instruction, and the cost of being wrong is total.
func TestDeleteRefusesTheCatastrophicTargets(t *testing.T) {
	dir := t.TempDir()
	runner := newRunner(t, localexec.Config{Dir: dir})

	if _, err := runner.DeleteFile("/"); err == nil {
		t.Error("deleting the filesystem root was accepted")
	}
	if _, err := runner.DeleteFile("."); err == nil {
		t.Error("deleting the working directory itself was accepted")
	}
	if _, err := runner.DeleteFile(dir); err == nil {
		t.Error("deleting the working directory by absolute path was accepted")
	}

	// It is still there.
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the working directory was removed: %v", err)
	}
}

// TestPathsAreNotConfined states the documented boundary rather than leaving a
// reader to assume the sandbox's containment applies here too.
func TestPathsAreNotConfined(t *testing.T) {
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("SECRET"), 0o600); err != nil {
		t.Fatalf("seed the file: %v", err)
	}
	runner := newRunner(t, localexec.Config{Dir: t.TempDir()})

	// Deliberately allowed: refusing here would be a gesture, since exec_command
	// could `cat` the same file.
	content, _, err := runner.ReadFile(outsideFile)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if content != "SECRET" {
		t.Errorf("content = %q, want the file outside the working directory", content)
	}
}

func TestStatusSaysItIsNotSandboxed(t *testing.T) {
	dir := t.TempDir()
	runner := newRunner(t, localexec.Config{Dir: dir, MaxOutputBytes: 4096})

	status := runner.Status()
	if status.Sandboxed {
		t.Error("status claims to be sandboxed")
	}
	if status.RootDir != runner.Dir() {
		t.Errorf("root_dir = %q, want %q", status.RootDir, runner.Dir())
	}
	if status.Shell != runner.Shell() {
		t.Errorf("shell = %q, want %q", status.Shell, runner.Shell())
	}
	if status.MaxOutputBytes != 4096 {
		t.Errorf("max_output_bytes = %d, want 4096", status.MaxOutputBytes)
	}
}

// TestExecWithArgsMatchesTheServerContract covers the reason the input is a
// superset rather than a variant: a caller written against the server sends args,
// and must get direct execution with no shell interpreting anything.
func TestExecWithArgsMatchesTheServerContract(t *testing.T) {
	runner := newRunner(t, localexec.Config{Dir: t.TempDir()})

	res, err := runner.Run(t.Context(), "echo", []string{"a && b | c > d"}, 0, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "a && b | c > d" {
		t.Errorf("stdout = %q, want the argument verbatim — no shell should have interpreted it", res.Stdout)
	}

	// No file was created, which is what proves the redirection was not run.
	entries, err := runner.ListDir(".")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("directory contains %+v, want nothing — shell syntax in an argument was interpreted", entries)
	}
}

func TestExecWithoutArgsUsesTheShell(t *testing.T) {
	runner := newRunner(t, localexec.Config{Dir: t.TempDir()})

	res, err := runner.Run(t.Context(), "echo one > written.txt && cat written.txt", nil, 0, "")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "one" {
		t.Errorf("stdout = %q, want the shell to have run the whole line", res.Stdout)
	}
}
