// The tests live in an external package because the harness they use starts a
// real hub with real workers, and so imports this one.
package workerlink_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JetManiack/mcp-sandbox/internal/sandbox"
	"github.com/JetManiack/mcp-sandbox/internal/stream"
	"github.com/JetManiack/mcp-sandbox/internal/workerlink"
	"github.com/JetManiack/mcp-sandbox/internal/workerproto"
	"github.com/JetManiack/mcp-sandbox/internal/workertest"
)

const waitFor = 10 * time.Second

func TestNewRejectsAnIncompleteConfiguration(t *testing.T) {
	full := workerlink.Config{
		ServerURL: "http://executor:8080",
		Token:     "secret",
		WorkerID:  "worker-a",
	}

	for name, mutate := range map[string]func(*workerlink.Config){
		"no server URL": func(c *workerlink.Config) { c.ServerURL = "" },
		"no token":      func(c *workerlink.Config) { c.Token = "" },
		"no worker id":  func(c *workerlink.Config) { c.WorkerID = "" },
		// A bare host would dial nothing in particular; better to say so at
		// startup than to fail every reconnect for the life of the pod.
		"a scheme that is not a URL": func(c *workerlink.Config) { c.ServerURL = "executor:8080" },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := full
			mutate(&cfg)
			if _, err := workerlink.New(cfg, sandbox.DefaultConfig(t.TempDir())); err == nil {
				t.Error("New accepted the configuration")
			}
		})
	}
}

func TestNewAcceptsBothHTTPAndWebSocketURLs(t *testing.T) {
	// An operator writing the server's own base URL into SERVER_URL is the
	// obvious thing to do, so it has to work alongside the ws form.
	for _, url := range []string{
		"http://executor:8080", "https://executor", "ws://executor:8080", "wss://executor",
		"http://executor:8080/", // a trailing slash is not a different server
	} {
		if _, err := workerlink.New(workerlink.Config{
			ServerURL: url, Token: "secret", WorkerID: "worker-a",
		}, sandbox.DefaultConfig(t.TempDir())); err != nil {
			t.Errorf("New(%q): %v", url, err)
		}
	}
}

func TestNewRejectsLimitsThatWouldNeedAnEnormousFrame(t *testing.T) {
	cfg := sandbox.DefaultConfig(t.TempDir())
	cfg.MaxOutputBytes = workerproto.MaxNegotiatedFrameBytes

	// The socket is sized from these numbers, so an absurd cap is an absurd
	// allocation. Caught in the process whose flags are wrong, at startup, rather
	// than as a refused handshake an operator has to go and read the server for.
	_, err := workerlink.New(workerlink.Config{
		ServerURL: "http://executor:8080", Token: "secret", WorkerID: "worker-a",
	}, cfg)
	if err == nil {
		t.Fatal("New accepted limits needing a frame over the ceiling")
	}
	if !strings.Contains(err.Error(), "ceiling") {
		t.Errorf("err = %q, want it to say the ceiling was exceeded", err)
	}
}

func TestNewAcceptsTheDefaults(t *testing.T) {
	// The checks must not reject the shipped configuration, which is the one every
	// deployment starts from.
	if _, err := workerlink.New(workerlink.Config{
		ServerURL: "http://executor:8080", Token: "secret", WorkerID: "worker-a",
	}, sandbox.DefaultConfig(t.TempDir())); err != nil {
		t.Fatalf("New rejected the defaults: %v", err)
	}
}

func TestAFileWrittenThroughTheLinkCanBeReadBack(t *testing.T) {
	h := workertest.StartOne(t)
	ctx := context.Background()

	// The round trip the whole sticky-routing design exists to protect: a write
	// and the read after it have to reach the same sandbox.
	if _, err := h.Hub.WriteFile(ctx, "agent-1", "notes.txt", "written over the wire"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	got, err := h.Hub.ReadFile(ctx, "agent-1", "notes.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if got != "written over the wire" {
		t.Errorf("content = %q", got)
	}

	// And it is a real file on the worker's disk, not something the link made up.
	onDisk, err := os.ReadFile(filepath.Join(h.Roots[0], "agents", "agent-1", "notes.txt"))
	if err != nil {
		t.Fatalf("read the worker's copy: %v", err)
	}
	if string(onDisk) != "written over the wire" {
		t.Errorf("on disk = %q", onDisk)
	}
}

func TestListingAndDeletingCrossTheLink(t *testing.T) {
	h := workertest.StartOne(t)
	ctx := context.Background()

	if _, err := h.Hub.WriteFile(ctx, "agent-1", "keep.txt", "a"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := h.Hub.WriteFile(ctx, "agent-1", "drop.txt", "b"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	files, err := h.Hub.ListDir(ctx, "agent-1", ".")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("files = %+v, want two", files)
	}

	deleted, err := h.Hub.DeleteFile(ctx, "agent-1", "drop.txt")
	if err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	if !deleted.Existed || deleted.WasDirectory {
		t.Errorf("delete = %+v, want an existing file", deleted)
	}

	files, err = h.Hub.ListDir(ctx, "agent-1", ".")
	if err != nil {
		t.Fatalf("ListDir after delete: %v", err)
	}
	if len(files) != 1 || files[0].Name != "keep.txt" {
		t.Errorf("files = %+v, want just keep.txt", files)
	}
}

func TestACommandRunsOnTheWorkerAndItsOutputComesBack(t *testing.T) {
	h := workertest.StartOne(t)

	out, err := h.Hub.Exec(context.Background(), "agent-1", workerproto.ExecRequest{
		Command: "/bin/sh",
		Args:    []string{"-c", "echo out; echo err >&2; exit 3"},
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}

	if strings.TrimSpace(out.Stdout) != "out" {
		t.Errorf("stdout = %q", out.Stdout)
	}
	if strings.TrimSpace(out.Stderr) != "err" {
		t.Errorf("stderr = %q", out.Stderr)
	}
	// The exit code has to survive the wire: an agent branches on it.
	if out.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", out.ExitCode)
	}
	if out.ExecID == "" {
		t.Error("no exec id, so nothing ties the events to this command")
	}
}

func TestOutputReachesWatchersWhileTheCommandRuns(t *testing.T) {
	h := workertest.StartOne(t)

	_, sub := h.Bus.Subscribe("agent-1", 0)
	defer h.Bus.Unsubscribe("agent-1", sub)

	go func() {
		_, _ = h.Hub.Exec(context.Background(), "agent-1", workerproto.ExecRequest{
			Command: "/bin/sh",
			Args:    []string{"-c", "echo streamed"},
		})
	}()

	// The worker holds no retention buffer: it forwards, and the server is what
	// a browser is attached to.
	var kinds []stream.EventKind
	var sawOutput bool
	deadline := time.After(waitFor)
	for {
		select {
		case event := <-sub.Events():
			kinds = append(kinds, event.Kind)
			if event.Kind == stream.EventStdout && strings.Contains(event.Data, "streamed") {
				sawOutput = true
			}
			if event.Kind == stream.EventFinished {
				if kinds[0] != stream.EventStarted {
					t.Errorf("events = %v, want started first", kinds)
				}
				if !sawOutput {
					t.Errorf("events = %v, want the output among them", kinds)
				}
				return
			}
		case <-deadline:
			t.Fatalf("no finished event; saw %v", kinds)
		}
	}
}

func TestATimeoutCrossesTheWireAsATimeout(t *testing.T) {
	h := workertest.StartOne(t)

	_, err := h.Hub.Exec(context.Background(), "agent-1", workerproto.ExecRequest{
		Command:    "/bin/sleep",
		Args:       []string{"30"},
		TimeoutSec: 1,
	})

	// An error string alone would lose the distinction, and a stopped sandbox
	// would start reporting that the command ran out of time.
	if !errors.Is(err, sandbox.ErrCommandTimeout) {
		t.Fatalf("err = %v, want ErrCommandTimeout", err)
	}
}

func TestKillStopsWhatIsRunningInTheSandbox(t *testing.T) {
	h := workertest.StartOne(t)

	running := make(chan error, 1)
	go func() {
		_, err := h.Hub.Exec(context.Background(), "agent-1", workerproto.ExecRequest{
			Command:    "/bin/sleep",
			Args:       []string{"30"},
			TimeoutSec: 60,
		})
		running <- err
	}()

	// Wait until the command has actually started before stopping it, or the
	// kill races the fork and finds nothing to do.
	waitForStart(t, h, "agent-1")

	killed, err := h.Hub.Kill(context.Background(), "agent-1", "operator", "testing")
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if killed != 1 {
		t.Errorf("killed = %d, want 1", killed)
	}

	select {
	case err := <-running:
		if !errors.Is(err, sandbox.ErrCommandStopped) {
			t.Errorf("err = %v, want ErrCommandStopped", err)
		}
	case <-time.After(waitFor):
		t.Fatal("the command outlived the kill")
	}
}

func TestKillingAnAgentWithNoWorkerIsNotAnError(t *testing.T) {
	h := workertest.Start(t, 1)

	// Blocking an idle agent still has to work: the block is what refuses its
	// next call, wherever that lands.
	killed, err := h.Hub.Kill(context.Background(), "never-seen", "operator", "blocked")
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if killed != 0 {
		t.Errorf("killed = %d, want 0", killed)
	}
}

func TestEachAgentGetsItsOwnSandbox(t *testing.T) {
	h := workertest.StartOne(t)
	ctx := context.Background()

	if _, err := h.Hub.WriteFile(ctx, "agent-1", "secret.txt", "one agent's work"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Only through the tools. A command is not confined to its sandbox — see
	// "What exec_command is not" — so an agent that runs `cat` with an absolute
	// path reaches its neighbours on the same worker. This asserts the tool
	// boundary, which is the one that exists.
	if _, err := h.Hub.ReadFile(ctx, "agent-2", "secret.txt"); err == nil {
		t.Error("agent-2 read agent-1's file")
	}

	files, err := h.Hub.ListDir(ctx, "agent-2", ".")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("agent-2 sees %+v, want an empty sandbox", files)
	}
}

func TestAPathEscapeIsRefusedAcrossTheWire(t *testing.T) {
	h := workertest.StartOne(t)
	ctx := context.Background()

	// The jail is enforced on the worker, which is the only place that has the
	// filesystem — the server cannot check a path it cannot see.
	for _, path := range []string{"../escaped.txt", "../../etc/passwd", "/etc/passwd"} {
		if _, err := h.Hub.WriteFile(ctx, "agent-1", path, "escaped"); err == nil {
			t.Errorf("WriteFile(%q) was allowed", path)
		}
		if _, err := h.Hub.ReadFile(ctx, "agent-1", path); err == nil {
			t.Errorf("ReadFile(%q) was allowed", path)
		}
	}

	if _, err := os.Stat(filepath.Join(h.Roots[0], "agents", "escaped.txt")); !os.IsNotExist(err) {
		t.Error("a file landed outside the agent's sandbox")
	}
}

// The two tests below are about blast radius rather than about file sizes.
//
// Exceeding a WebSocket read limit does not fail one message: it closes the
// connection. Many agents are multiplexed over one worker connection, so an
// oversized payload that reached the wire would disconnect the worker and orphan
// every sandbox on it — one agent writing a large CSV taking out its neighbours.

func TestAnOversizedWriteIsRefusedWithoutTakingTheWorkerDown(t *testing.T) {
	h := workertest.StartOne(t)
	ctx := context.Background()

	if _, err := h.Hub.WriteFile(ctx, "bystander", "ok.txt", "fine"); err != nil {
		t.Fatalf("bystander setup: %v", err)
	}

	// Over the negotiated frame, not merely over the file cap: at this size the
	// connection would actually close if the payload reached the wire, so the
	// bystander check below is testing containment rather than passing vacuously.
	oversized := strings.Repeat("x", negotiatedFrameBytes(t, h)+(1<<20))
	_, err := h.Hub.WriteFile(ctx, "agent-1", "big.bin", oversized)
	if err == nil {
		t.Fatal("the oversized write was accepted")
	}
	// Actionable: a configured cap an operator can raise, named as such, with the
	// file's size next to it — not a payload that merely did not fit.
	if !strings.Contains(err.Error(), "limit for one file") {
		t.Errorf("err = %q, want it to name the file limit", err)
	}

	assertWorkerSurvived(t, h)
}

func TestAnOversizedReadIsRefusedWithoutTakingTheWorkerDown(t *testing.T) {
	h := workertest.StartOne(t)
	ctx := context.Background()

	if _, err := h.Hub.WriteFile(ctx, "bystander", "ok.txt", "fine"); err != nil {
		t.Fatalf("bystander setup: %v", err)
	}

	// Planted on the worker's disk, so the oversized payload is the worker's
	// answer rather than the server's request — the other direction, which fails
	// on the server's read limit instead.
	dir := filepath.Join(h.Roots[0], "agents", "agent-1")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	big := []byte(strings.Repeat("y", negotiatedFrameBytes(t, h)+(1<<20)))
	if err := os.WriteFile(filepath.Join(dir, "big.bin"), big, 0o600); err != nil {
		t.Fatalf("plant the file: %v", err)
	}

	_, err := h.Hub.ReadFile(ctx, "agent-1", "big.bin")
	if err == nil {
		t.Fatal("the oversized read was accepted")
	}
	if !strings.Contains(err.Error(), "limit for one file") {
		t.Errorf("err = %q, want it to name the file limit", err)
	}

	assertWorkerSurvived(t, h)
}

// negotiatedFrameBytes is the frame size the worker in this harness declared,
// asked of the hub rather than recomputed, so the tests follow the defaults
// instead of pinning a copy of them.
func negotiatedFrameBytes(t *testing.T, h *workertest.Harness) int {
	t.Helper()

	workers := h.Hub.Workers()
	if len(workers) == 0 {
		t.Fatal("no worker connected")
	}
	return workers[0].Limits.FrameBytes()
}

// assertWorkerSurvived checks that an unrelated agent on the same worker is
// unaffected — which is the whole point of refusing before the write.
func assertWorkerSurvived(t *testing.T, h *workertest.Harness) {
	t.Helper()

	if workers := h.Hub.Workers(); len(workers) != 1 {
		t.Fatalf("workers = %d, want the worker still connected", len(workers))
	}

	got, err := h.Hub.ReadFile(context.Background(), "bystander", "ok.txt")
	if err != nil {
		t.Fatalf("a bystanding agent lost its sandbox: %v", err)
	}
	if got != "fine" {
		t.Errorf("bystander read %q, want its file intact", got)
	}
}

func TestStatusNamesTheWorkerHoldingTheSandbox(t *testing.T) {
	h := workertest.StartOne(t)

	status, err := h.Hub.Status(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}

	// Which pod holds an agent's files is the one thing an operator debugging a
	// scaled-out pool cannot get anywhere else.
	if status.WorkerID == "" {
		t.Error("status names no worker")
	}
	// Compared through EvalSymlinks because a macOS temp dir is reached via a
	// symlink, and the worker reports the path it resolved.
	root, err := filepath.EvalSymlinks(h.Roots[0])
	if err != nil {
		t.Fatalf("resolve the worker root: %v", err)
	}
	if want := filepath.Join(root, "agents", "agent-1"); status.RootDir != want {
		t.Errorf("root dir = %q, want %q", status.RootDir, want)
	}
}

// waitForStart blocks until a command has begun in the agent's sandbox.
func waitForStart(t *testing.T, h *workertest.Harness, agentID string) {
	t.Helper()

	deadline := time.Now().Add(waitFor)
	for time.Now().Before(deadline) {
		status, err := h.Hub.Status(context.Background(), agentID)
		if err == nil && status.RunningCommands > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no command started")
}
