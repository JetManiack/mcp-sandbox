package sandboxop_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JetManiack/mcp-sandbox/internal/sandboxop"
)

// TestHelperProcess is the child. It is a test rather than a fixture binary so
// the parent can invoke this very executable, which is what the worker does with
// its own — the plumbing under test is the fork and the pipe, and a fake helper
// would leave both uncovered.
//
// The uid drop itself cannot be exercised here: setuid needs CAP_SETUID, which a
// developer's machine has not got. What this covers is everything around it.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("SANDBOXOP_HELPER") == "" && !hasHelperArg() {
		t.Skip("not the helper process")
	}
	if err := sandboxop.Serve(os.Stdin, os.Stdout); err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

func hasHelperArg() bool {
	for _, arg := range os.Args {
		if arg == "sandboxop-helper" {
			return true
		}
	}
	return false
}

// runner invokes this test binary as the helper.
func runner(t *testing.T) *sandboxop.Runner {
	t.Helper()

	return &sandboxop.Runner{
		Exe: os.Args[0],
		// A positional argument rather than a flag: the test binary's flag parser
		// rejects names it does not know, and this one belongs to us.
		Args: []string{"-test.run=^TestHelperProcess$", "sandboxop-helper"},
	}
}

func TestAWriteAndReadCrossTheProcessBoundary(t *testing.T) {
	root := t.TempDir()
	r := runner(t)
	ctx := context.Background()

	// The two failures are checked apart, because the code distinguishes them: an
	// error from Do means the helper did not run, an error inside the response
	// means it ran and the operation failed.
	out, err := r.Do(ctx, 0, sandboxop.Request{
		Root: root, Op: sandboxop.OpWrite, Name: "notes/today.txt", Content: []byte("written by the child"),
	})
	if err != nil {
		t.Fatalf("the helper did not run: %v", err)
	}
	if err := out.Err(); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Parent directories are created, as the in-process path does.
	if _, err := os.Stat(filepath.Join(root, "notes", "today.txt")); err != nil {
		t.Fatalf("the file is not on disk: %v", err)
	}

	read, err := r.Do(ctx, 0, sandboxop.Request{Root: root, Op: sandboxop.OpRead, Name: "notes/today.txt"})
	if err != nil {
		t.Fatalf("the helper did not run: %v", err)
	}
	if err := read.Err(); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(read.Content) != "written by the child" {
		t.Errorf("content = %q", read.Content)
	}
}

func TestListingAndDeletingCrossTheProcessBoundary(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"keep.txt", "drop.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0o600); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
	r := runner(t)
	ctx := context.Background()

	out, err := r.Do(ctx, 0, sandboxop.Request{Root: root, Op: sandboxop.OpList, Name: "."})
	if err != nil {
		t.Fatalf("the helper did not run: %v", err)
	}
	if err := out.Err(); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(out.Files) != 2 {
		t.Fatalf("files = %+v, want two", out.Files)
	}

	deleted, err := r.Do(ctx, 0, sandboxop.Request{Root: root, Op: sandboxop.OpDelete, Name: "drop.txt"})
	if err != nil {
		t.Fatalf("the helper did not run: %v", err)
	}
	if err := deleted.Err(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !deleted.Existed || deleted.WasDirectory {
		t.Errorf("delete = %+v, want an existing file", deleted)
	}
	if _, err := os.Stat(filepath.Join(root, "drop.txt")); !os.IsNotExist(err) {
		t.Error("the file survived the delete")
	}
}

func TestTheChildRefusesToLeaveTheSandbox(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(filepath.Dir(root), "outside.txt")
	if err := os.WriteFile(outside, []byte("not yours"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	r := runner(t)
	ctx := context.Background()

	// The parent resolves paths, but the child is what holds the sandbox open —
	// so it enforces containment itself rather than trusting the name it was
	// handed. os.Root refuses in the kernel, not in arithmetic here.
	for _, name := range []string{"../outside.txt", "../../etc/passwd", "/etc/passwd"} {
		out, err := r.Do(ctx, 0, sandboxop.Request{Root: root, Op: sandboxop.OpRead, Name: name})
		if err == nil && out.Err() == nil {
			t.Errorf("read(%q) was allowed", name)
		}
	}
}

func TestAnOperationFailureComesBackAsAnError(t *testing.T) {
	r := runner(t)

	out, err := r.Do(context.Background(), 0, sandboxop.Request{
		Root: t.TempDir(), Op: sandboxop.OpRead, Name: "not-there.txt",
	})
	// The transport succeeded; the operation did not. Those are different things
	// and the caller has to be able to tell them apart.
	if err != nil {
		t.Fatalf("the helper itself failed: %v", err)
	}
	opErr := out.Err()
	if opErr == nil {
		t.Fatal("reading a missing file reported success")
	}
	if !strings.Contains(opErr.Error(), "not-there.txt") {
		t.Errorf("err = %v, want it to name the file", opErr)
	}
}

func TestAnUnknownOperationIsRefusedRatherThanIgnored(t *testing.T) {
	out, err := runner(t).Do(context.Background(), 0, sandboxop.Request{
		Root: t.TempDir(), Op: "chmod-everything", Name: ".",
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if out.Err() == nil {
		t.Fatal("an unknown operation reported success, so a typo would silently do nothing")
	}
}

func TestAMissingHelperFailsLoudly(t *testing.T) {
	r := &sandboxop.Runner{Exe: filepath.Join(t.TempDir(), "no-such-binary")}

	// The worker invokes its own path; if that ever stops resolving, every file
	// operation has to fail with something an operator can act on.
	if _, err := r.Do(context.Background(), 0, sandboxop.Request{
		Root: t.TempDir(), Op: sandboxop.OpRead, Name: "x",
	}); err == nil {
		t.Fatal("a missing helper reported success")
	}
}
