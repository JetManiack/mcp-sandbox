package restapi_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/JetManiack/mcp-sandbox/internal/stream"
	"github.com/JetManiack/mcp-sandbox/internal/workerproto"
)

// dial opens the terminal stream for actorID, optionally resuming after a
// sequence number.
func (api *testAPI) dial(t *testing.T, actorID string, after uint64) *websocket.Conn {
	t.Helper()

	url := strings.Replace(api.server.URL, "http://", "ws://", 1) + "/sandboxes/" + actorID + "/stream"
	if after > 0 {
		url += "?after=" + strconv.FormatUint(after, 10)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)

	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// readEvent reads one event, failing the test if none arrives in time.
func readEvent(t *testing.T, conn *websocket.Conn, timeout time.Duration) stream.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var event stream.Event
	if err := wsjson.Read(ctx, conn, &event); err != nil {
		t.Fatalf("read event: %v", err)
	}
	return event
}

func TestStreamDeliversLiveOutput(t *testing.T) {
	api := newTestAPI(t, viewerProvider())
	agent := api.mustAgent(t, "agent-1")

	conn := api.dial(t, agent.ID, 0)

	go func() {
		_, _ = api.worker.Hub.Exec(context.Background(), agent.ID, workerproto.ExecRequest{
			Command: "echo", Args: []string{"streamed"},
		})
	}()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		event := readEvent(t, conn, 10*time.Second)
		if event.Kind == stream.EventStdout && strings.Contains(event.Data, "streamed") {
			return
		}
	}
	t.Fatal("command output never arrived over the stream")
}

func TestStreamReplaysRetainedOutput(t *testing.T) {
	api := newTestAPI(t, viewerProvider())
	agent := api.mustAgent(t, "agent-1")

	// Output produced before anybody was watching: an operator opening the
	// terminal after an alert must see what already happened, or there is nothing
	// to base a kill-or-not decision on.
	if _, err := api.worker.Hub.Exec(context.Background(), agent.ID, workerproto.ExecRequest{
		Command: "echo", Args: []string{"earlier"},
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	conn := api.dial(t, agent.ID, 0)

	var replayed strings.Builder
	for range 3 {
		event := readEvent(t, conn, 5*time.Second)
		replayed.WriteString(event.Data)
	}
	if !strings.Contains(replayed.String(), "earlier") {
		t.Errorf("replay = %q, want it to contain the output produced before connecting", replayed.String())
	}
}

// TestStreamReportsAGapAfterEviction is the honesty requirement at the HTTP
// boundary: a resumed stream whose position was evicted must say so.
func TestStreamReportsAGapAfterEviction(t *testing.T) {
	api := newTestAPI(t, viewerProvider())
	agent := api.mustAgent(t, "agent-1")

	// Far more output than the retention budget, so sequence number 1 is long gone
	// by the time the watcher asks to resume from it.
	if _, err := api.worker.Hub.Exec(context.Background(), agent.ID, workerproto.ExecRequest{
		Command: "/bin/sh", Args: []string{"-c", "for i in $(seq 1 40000); do echo line-$i; done"}, TimeoutSec: 60,
	}); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	conn := api.dial(t, agent.ID, 1)
	event := readEvent(t, conn, 5*time.Second)

	if event.Kind != stream.EventGap {
		t.Fatalf("first event kind = %q, want %q", event.Kind, stream.EventGap)
	}
	if event.MissedEvents == 0 {
		t.Error("gap marker reports zero missed events")
	}
}

func TestStreamRejectsUnknownAgent(t *testing.T) {
	api := newTestAPI(t, viewerProvider())

	resp := api.do(t, http.MethodGet, "/sandboxes/00000000-0000-0000-0000-000000000000/stream", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestStreamRejectsMalformedResumePosition(t *testing.T) {
	api := newTestAPI(t, viewerProvider())
	agent := api.mustAgent(t, "agent-1")

	resp := api.do(t, http.MethodGet, "/sandboxes/"+agent.ID+"/stream?after=not-a-number", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
}

// TestStreamIgnoresClientMessages pins the read-only contract: the socket exists
// so a human can watch, and nothing arriving on it is executed.
func TestStreamIgnoresClientMessages(t *testing.T) {
	api := newTestAPI(t, viewerProvider())
	agent := api.mustAgent(t, "agent-1")

	conn := api.dial(t, agent.ID, 0)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Whatever this is, it must not become a command. The server calls
	// CloseRead, so the write either lands and is discarded or the socket is
	// closed under us; either is acceptable, executing it is not.
	_ = wsjson.Write(ctx, conn, map[string]string{"command": "touch /tmp/should-not-exist"})

	listing, err := api.worker.Hub.ListDir(context.Background(), agent.ID, ".")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(listing) != 0 {
		t.Errorf("sandbox contains %d entries, want none — a client message caused a side effect", len(listing))
	}
}

func TestStreamBlockedEventReachesWatchers(t *testing.T) {
	api := newTestAPI(t, adminProvider())
	agent := api.mustAgent(t, "agent-1")

	conn := api.dial(t, agent.ID, 0)

	if resp := api.do(t, http.MethodPost, "/sandboxes/"+agent.ID+"/block", map[string]string{"reason": "runaway loop"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST block: status = %d", resp.StatusCode)
	}

	// Without this event a watcher sees output stop and cannot tell a finished
	// command from an administrative stop.
	event := readEvent(t, conn, 5*time.Second)
	if event.Kind != stream.EventBlocked {
		t.Fatalf("event kind = %q, want %q", event.Kind, stream.EventBlocked)
	}
	if event.ByActor != "Grace" || event.Reason != "runaway loop" {
		t.Errorf("event = %+v, want it to name who blocked it and why", event)
	}
}
