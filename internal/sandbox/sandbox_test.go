package sandbox_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/JetManiack/mcp-sandbox/internal/sandbox"
)

func setupTestSandbox(t *testing.T) (*sandbox.Sandbox, string) {
	t.Helper()
	tempDir := t.TempDir()
	sb, err := sandbox.New(sandbox.Config{
		RootDir:        tempDir,
		DefaultTimeout: 2 * time.Second,
		MaxOutputBytes: 1024,
	})
	if err != nil {
		t.Fatalf("failed to create sandbox: %v", err)
	}
	return sb, tempDir
}

func TestResolvePath_Security(t *testing.T) {
	sb, _ := setupTestSandbox(t)

	validPaths := []string{
		"file.txt",
		"sub/dir/file.txt",
		"./sub/file.txt",
	}
	for _, p := range validPaths {
		resolved, err := sb.ResolvePath(p)
		if err != nil {
			t.Errorf("expected path %q to be valid, got error: %v", p, err)
		}
		if resolved == "" {
			t.Errorf("expected non-empty resolved path for %q", p)
		}
	}

	invalidPaths := []string{
		"../outside.txt",
		"sub/../../outside.txt",
		"/etc/passwd",
	}
	for _, p := range invalidPaths {
		_, err := sb.ResolvePath(p)
		if err == nil {
			t.Errorf("expected path %q to be rejected as outside sandbox, but it passed", p)
		}
	}
}

func TestExecCommand(t *testing.T) {
	sb, _ := setupTestSandbox(t)
	ctx := context.Background()

	res, err := sb.ExecCommand(ctx, "echo", []string{"hello world"}, 1*time.Second, "")
	if err != nil {
		t.Fatalf("ExecCommand failed: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("expected exit code 0, got %d", res.ExitCode)
	}
	if res.Stdout != "hello world\n" {
		t.Errorf("unexpected stdout: %q", res.Stdout)
	}
}

func TestExecCommand_Timeout(t *testing.T) {
	sb, _ := setupTestSandbox(t)
	ctx := context.Background()

	_, err := sb.ExecCommand(ctx, "sleep", []string{"5"}, 500*time.Millisecond, "")
	if err == nil {
		t.Fatal("expected command to time out, but got no error")
	}
	if err != sandbox.ErrCommandTimeout {
		t.Errorf("expected ErrCommandTimeout, got: %v", err)
	}
}

func TestFilesystemOperations(t *testing.T) {
	sb, root := setupTestSandbox(t)

	// Write File
	err := sb.WriteFile("test/hello.txt", []byte("world"), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	// Verify file exists on real disk
	content, err := os.ReadFile(filepath.Join(root, "test/hello.txt"))
	if err != nil || string(content) != "world" {
		t.Fatalf("file content on disk mismatch: %v", err)
	}

	// Read File
	readData, err := sb.ReadFile("test/hello.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(readData) != "world" {
		t.Errorf("read content mismatch: %q", string(readData))
	}

	// List Dir
	files, err := sb.ListDir("test")
	if err != nil {
		t.Fatalf("ListDir failed: %v", err)
	}
	if len(files) != 1 || files[0].Name != "hello.txt" {
		t.Errorf("ListDir returned unexpected files: %+v", files)
	}

	// Delete File
	deleted, err := sb.DeleteFile("test/hello.txt")
	if err != nil {
		t.Fatalf("DeleteFile failed: %v", err)
	}
	if !deleted.Existed || deleted.WasDirectory {
		t.Errorf("delete result = %+v, want an existing non-directory", deleted)
	}

	// RemoveAll succeeds on a path that was never there, so the result is the
	// only way an agent can tell that apart from a real deletion.
	missing, err := sb.DeleteFile("test/never-existed.txt")
	if err != nil {
		t.Fatalf("DeleteFile on a missing path: %v", err)
	}
	if missing.Existed {
		t.Errorf("delete result = %+v, want Existed false", missing)
	}
	_, err = sb.ReadFile("test/hello.txt")
	if err == nil {
		t.Errorf("expected ReadFile to fail after deletion")
	}
}
