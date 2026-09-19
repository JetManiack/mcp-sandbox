package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/JetManiack/mcp-sandbox/internal/stream"
)

// shellProgram and shellScript spell out a shell invocation for the tests whose
// subject is streaming rather than execution policy, and which therefore need
// loops, redirection and sequencing.
//
// ExecCommand no longer interprets a shell, so asking for one explicitly is both
// the honest way to write these and a demonstration of the documented caveat: a
// shell is just another program an agent can run.
const shellProgram = "/bin/sh"

func shellScript(script string) []string { return []string{"-c", script} }

// newStreamingSandbox builds a manager-backed sandbox, which is the only kind
// with an event bus attached.
func newStreamingSandbox(t *testing.T) (*Sandbox, *stream.Broadcaster) {
	t.Helper()
	cfg := DefaultConfig(t.TempDir())
	cfg.DefaultTimeout = 10 * time.Second
	bus := stream.NewBroadcaster(0)
	// Broadcaster.Publish returns the stamped event; the sink discards it, because
	// only the server assigns sequence numbers and only tests care about them.
	mgr, err := NewManager(cfg, SinkFunc(func(e stream.Event) { bus.Publish(e) }))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	sb, err := mgr.GetSandbox("agent-1")
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	return sb, bus
}

// collectAsync starts draining sub immediately and returns a channel carrying
// everything up to and including the finished event.
//
// Draining has to run concurrently with the command: a watcher that only starts
// reading after the command exits falls behind the queue and is disconnected as a
// slow consumer, which is the broadcaster working as intended, not something for
// a test to work around.
func collectAsync(sub *stream.Subscription) <-chan []stream.Event {
	out := make(chan []stream.Event, 1)
	go func() {
		var events []stream.Event
		for e := range sub.Events() {
			events = append(events, e)
			if e.Kind == stream.EventFinished {
				break
			}
		}
		out <- events
	}()
	return out
}

// awaitEvents waits for a collector started by collectAsync.
func awaitEvents(t *testing.T, collected <-chan []stream.Event, timeout time.Duration) []stream.Event {
	t.Helper()
	select {
	case events := <-collected:
		return events
	case <-time.After(timeout):
		t.Fatal("timed out waiting for the finished event")
		return nil
	}
}

// TestOutputIsStreamedWhileTheCommandRuns is the property the whole phase exists
// for. The previous implementation buffered to completion and published one event
// at the end, so a ten-minute command showed nothing for ten minutes.
func TestOutputIsStreamedWhileTheCommandRuns(t *testing.T) {
	sb, bus := newStreamingSandbox(t)
	_, sub := bus.Subscribe(sb.ID(), 0)
	defer bus.Unsubscribe(sb.ID(), sub)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = sb.ExecCommand(context.Background(), shellProgram, shellScript("echo early; sleep 2"), 10*time.Second, "")
	}()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-sub.Events():
			if e.Kind == stream.EventStdout && strings.Contains(e.Data, "early") {
				return // output arrived before the command exited
			}
		case <-done:
			t.Fatal("the command finished before any of its output was streamed")
		case <-deadline:
			t.Fatal("timed out waiting for streamed output")
		}
	}
}

func TestExecPublishesStartedOutputAndFinished(t *testing.T) {
	sb, bus := newStreamingSandbox(t)
	_, sub := bus.Subscribe(sb.ID(), 0)
	defer bus.Unsubscribe(sb.ID(), sub)

	collected := collectAsync(sub)
	res, err := sb.ExecCommand(context.Background(), shellProgram, shellScript("echo out; echo err 1>&2"), 10*time.Second, "")
	if err != nil {
		t.Fatalf("ExecCommand: %v", err)
	}

	events := awaitEvents(t, collected, 5*time.Second)
	if len(events) < 3 {
		t.Fatalf("collected %d events, want at least started, output and finished", len(events))
	}

	if events[0].Kind != stream.EventStarted {
		t.Errorf("first event kind = %q, want %q", events[0].Kind, stream.EventStarted)
	}
	// The started event carries the rendered argument vector, quoted where an
	// argument contains whitespace — with no shell, a watcher has to be able to
	// see where one argument ends and the next begins.
	wantCommand := `/bin/sh -c "echo out; echo err 1>&2"`
	if events[0].Command != wantCommand {
		t.Errorf("started event command = %q, want %q", events[0].Command, wantCommand)
	}

	last := events[len(events)-1]
	if last.Kind != stream.EventFinished {
		t.Errorf("last event kind = %q, want %q", last.Kind, stream.EventFinished)
	}
	if last.ExitCode != 0 {
		t.Errorf("finished exit code = %d, want 0", last.ExitCode)
	}

	// Every event of one command shares its execution ID, which is how the UI
	// groups output under the command that produced it.
	for _, e := range events {
		if e.ExecID != res.ExecID {
			t.Errorf("event %q carries exec id %q, want %q", e.Kind, e.ExecID, res.ExecID)
		}
	}

	var stdout, stderr strings.Builder
	for _, e := range events {
		switch e.Kind {
		case stream.EventStdout:
			stdout.WriteString(e.Data)
		case stream.EventStderr:
			stderr.WriteString(e.Data)
		}
	}
	if !strings.Contains(stdout.String(), "out") {
		t.Errorf("streamed stdout = %q, want it to contain %q", stdout.String(), "out")
	}
	if !strings.Contains(stderr.String(), "err") {
		t.Errorf("streamed stderr = %q, want it to contain %q", stderr.String(), "err")
	}
}

func TestStreamedChunksReassembleToTheReturnedOutput(t *testing.T) {
	sb, bus := newStreamingSandbox(t)
	_, sub := bus.Subscribe(sb.ID(), 0)
	defer bus.Unsubscribe(sb.ID(), sub)

	// seq rather than a shell loop, and this is about the shape of the writes
	// rather than about the bytes. A shell printing lines flushes on every newline,
	// so 48KB arrived as two thousand events of about twenty bytes each — past the
	// two hundred and fifty six a watcher may fall behind by, at which point the
	// broadcaster disconnects it by design and this test was comparing a prefix.
	// A program with ordinary stdio buffering delivers the same bytes in a handful
	// of writes, and 128KB still crosses the 16KB read boundary many times.
	collected := collectAsync(sub)
	res, err := sb.ExecCommand(context.Background(), "seq", []string{"1", "20000"}, 30*time.Second, "")
	if err != nil {
		t.Fatalf("ExecCommand: %v", err)
	}

	events := awaitEvents(t, collected, 30*time.Second)
	assertKeptUp(t, sub)

	var streamed strings.Builder
	for _, e := range events {
		if e.Kind == stream.EventStdout {
			streamed.WriteString(e.Data)
		}
	}

	if streamed.String() != res.Stdout {
		t.Errorf("streamed output (%d bytes) differs from the returned output (%d bytes)", streamed.Len(), len(res.Stdout))
	}
}

// TestMultiByteOutputSurvivesChunkBoundaries guards the reason pump holds back an
// incomplete trailing rune: output is read in fixed-size chunks, so a multi-byte
// character routinely straddles two reads, and splitting one would put a
// replacement character at a moving position in the terminal.
func TestMultiByteOutputSurvivesChunkBoundaries(t *testing.T) {
	sb, bus := newStreamingSandbox(t)
	_, sub := bus.Subscribe(sb.ID(), 0)
	defer bus.Unsubscribe(sb.ID(), sub)

	// Three-byte characters, repeated well past chunkSize so boundaries land
	// mid-character.
	const repeats = 20000
	collected := collectAsync(sub)
	// One printf writing the lot, so the reads that split runes are the sandbox's
	// 16KB reads rather than twenty thousand three-byte writes. 16384 is not a
	// multiple of three, so boundaries still land mid-character.
	res, err := sb.ExecCommand(context.Background(), shellProgram,
		shellScript(`printf '☃%.0s' $(seq 1 `+itoa(repeats)+`)`), 60*time.Second, "")
	if err != nil {
		t.Fatalf("ExecCommand: %v", err)
	}

	events := awaitEvents(t, collected, 60*time.Second)
	assertKeptUp(t, sub)

	var streamed strings.Builder
	for _, e := range events {
		if e.Kind == stream.EventStdout {
			if strings.Contains(e.Data, "�") {
				t.Fatalf("chunk contains a replacement character, so a rune was split across events")
			}
			streamed.WriteString(e.Data)
		}
	}

	if got := strings.Count(streamed.String(), "☃"); got != repeats {
		t.Errorf("streamed %d snowmen, want %d", got, repeats)
	}
	if got := strings.Count(res.Stdout, "☃"); got != repeats {
		t.Errorf("returned %d snowmen, want %d", got, repeats)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestExitCodeIsReportedNotTreatedAsFailure(t *testing.T) {
	sb, _ := newStreamingSandbox(t)

	// A non-zero exit is a successful tool call reporting a failed command.
	res, err := sb.ExecCommand(context.Background(), shellProgram, shellScript("exit 3"), 10*time.Second, "")
	if err != nil {
		t.Fatalf("ExecCommand returned an error for a non-zero exit: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
}

func TestTruncationBoundsTheReturnedOutputOnly(t *testing.T) {
	cfg := DefaultConfig(t.TempDir())
	cfg.MaxOutputBytes = 1024
	bus := stream.NewBroadcaster(0)
	// Broadcaster.Publish returns the stamped event; the sink discards it, because
	// only the server assigns sequence numbers and only tests care about them.
	mgr, err := NewManager(cfg, SinkFunc(func(e stream.Event) { bus.Publish(e) }))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	sb, err := mgr.GetSandbox("agent-1")
	if err != nil {
		t.Fatalf("GetSandbox: %v", err)
	}
	_, sub := bus.Subscribe(sb.ID(), 0)
	defer bus.Unsubscribe(sb.ID(), sub)

	collected := collectAsync(sub)
	res, err := sb.ExecCommand(context.Background(), shellProgram, shellScript("for i in $(seq 1 2000); do echo line-$i; done"), 30*time.Second, "")
	if err != nil {
		t.Fatalf("ExecCommand: %v", err)
	}

	if !res.Truncated {
		t.Error("result is not marked truncated although output exceeded the cap")
	}
	if len(res.Stdout) > cfg.MaxOutputBytes {
		t.Errorf("returned %d bytes, want at most the %d-byte cap", len(res.Stdout), cfg.MaxOutputBytes)
	}

	// The live terminal is meant to keep showing what is happening even after the
	// tool result has been capped.
	events := awaitEvents(t, collected, 30*time.Second)
	var streamed int
	for _, e := range events {
		if e.Kind == stream.EventStdout {
			streamed += len(e.Data)
		}
	}
	if streamed <= cfg.MaxOutputBytes {
		t.Errorf("streamed %d bytes, want more than the %d-byte result cap", streamed, cfg.MaxOutputBytes)
	}
}

// assertKeptUp fails if the watcher was disconnected for falling behind.
//
// Without it, a lagging subscription and genuinely lost output look identical:
// collectAsync stops when the channel closes, whichever reason closed it, and the
// comparison that follows then reports "streamed output differs" about a prefix.
// That sent a CI failure looking at the streaming code when the cause was a test
// publishing thirteen thousand events through a queue of two hundred and fifty six.
func assertKeptUp(t *testing.T, sub *stream.Subscription) {
	t.Helper()

	if sub.Lagged() {
		t.Fatal("the watcher was disconnected for falling behind, so what it collected is a prefix " +
			"rather than the whole stream — this test cannot tell that apart from lost output, " +
			"so it says so instead of comparing")
	}
}
