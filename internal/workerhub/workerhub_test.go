package workerhub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/JetManiack/mcp-sandbox/internal/stream"
	"github.com/JetManiack/mcp-sandbox/internal/workerproto"
)

const testToken = "shared-secret"

// waitFor is how long a test waits for something that crosses the connection
// before calling it a failure. Generous, because the alternative to waiting is a
// flake on a loaded machine.
const waitFor = 5 * time.Second

// newHub returns a hub, its event bus, and the base URL workers dial.
func newHub(t *testing.T) (*Hub, *stream.Broadcaster, string) {
	t.Helper()

	bus := stream.NewBroadcaster(0)
	h := New(bus)
	srv := httptest.NewServer(h.Handler(testToken))
	t.Cleanup(srv.Close)
	return h, bus, srv.URL
}

// fakeWorker is a worker spelled out frame by frame. The hub's own client would
// be the easier thing to test against and the wrong one: these tests are about
// what the hub does with what arrives on the wire, including frames a correct
// worker never sends.
type fakeWorker struct {
	conn     *websocket.Conn
	requests chan workerproto.Frame
	cancels  chan uint64
}

// dial opens a raw connection without saying hello, for the tests whose subject
// is the handshake itself.
func dial(t *testing.T, baseURL, token string) (*websocket.Conn, *http.Response, error) {
	t.Helper()

	url := "ws" + strings.TrimPrefix(baseURL, "http") + workerproto.Path
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()

	return websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
	})
}

// connect dials, says hello, and starts reading — a worker that has registered
// but answers nothing until a test tells it to.
func connect(t *testing.T, baseURL, workerID string) *fakeWorker {
	t.Helper()

	conn, _, err := dial(t, baseURL, testToken)
	if err != nil {
		t.Fatalf("dial as %s: %v", workerID, err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	w := &fakeWorker{
		conn:     conn,
		requests: make(chan workerproto.Frame, 16),
		cancels:  make(chan uint64, 16),
	}
	w.write(t, workerproto.Frame{
		Type: workerproto.FrameHello, WorkerID: workerID, Version: "test",
		Protocol: workerproto.ProtocolVersion, Limits: &testLimits,
	})
	go w.read()
	return w
}

// testLimits are what the fake workers declare: the shipped defaults, so the
// connection the tests run over is sized the way a real one is.
var testLimits = workerproto.Limits{MaxOutputBytes: 512 << 10, MaxFileBytes: 8 << 20}

func (w *fakeWorker) read() {
	// Closed on exit so a test blocked in nextRequest learns the connection ended
	// rather than waiting out its timeout.
	defer close(w.requests)
	defer close(w.cancels)

	for {
		var frame workerproto.Frame
		if err := wsjson.Read(context.Background(), w.conn, &frame); err != nil {
			return
		}
		switch frame.Type {
		case workerproto.FrameRequest:
			w.requests <- frame
		case workerproto.FrameCancel:
			w.cancels <- frame.ID
		}
	}
}

func (w *fakeWorker) write(t *testing.T, frame workerproto.Frame) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	if err := wsjson.Write(ctx, w.conn, frame); err != nil {
		t.Fatalf("write %s frame: %v", frame.Type, err)
	}
}

// nextRequest waits for one request to arrive from the hub.
func (w *fakeWorker) nextRequest(t *testing.T) workerproto.Frame {
	t.Helper()

	select {
	case frame, ok := <-w.requests:
		if !ok {
			t.Fatal("the connection ended before a request arrived")
		}
		return frame
	case <-time.After(waitFor):
		t.Fatal("no request arrived")
		return workerproto.Frame{}
	}
}

// answerStatusWith replies to every request with a status naming workerID,
// which is how the routing tests find out where a call landed.
func (w *fakeWorker) answerStatusWith(t *testing.T, workerID string) {
	t.Helper()

	go func() {
		for frame := range w.requests {
			raw, err := workerproto.Marshal(workerproto.StatusResponse{WorkerID: workerID})
			if err != nil {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), waitFor)
			err = wsjson.Write(ctx, w.conn, workerproto.Frame{
				Type: workerproto.FrameResult, ID: frame.ID, OK: true, Payload: raw,
			})
			cancel()
			if err != nil {
				return
			}
		}
	}()
}

// waitForWorkers blocks until want workers have registered, so no test races the
// hello it depends on.
func waitForWorkers(t *testing.T, h *Hub, want int) {
	t.Helper()

	deadline := time.Now().Add(waitFor)
	for time.Now().Before(deadline) {
		if len(h.Workers()) >= want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%d of %d workers registered", len(h.Workers()), want)
}

func TestWithoutATokenTheEndpointIsDisabledRatherThanOpen(t *testing.T) {
	srv := httptest.NewServer(New(stream.NewBroadcaster(0)).Handler(""))
	defer srv.Close()

	resp, err := http.Get(srv.URL + workerproto.Path) //nolint:noctx // a one-line status check
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// The opposite default — no token configured means accept everyone — would
	// turn a missing flag into an open execution service.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

func TestAWorkerWithTheWrongTokenIsRejected(t *testing.T) {
	_, _, url := newHub(t)

	conn, resp, err := dial(t, url, "not-the-token")
	if err == nil {
		_ = conn.CloseNow()
		t.Fatal("dial succeeded with the wrong token")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want %d", resp, http.StatusUnauthorized)
	}
}

func TestTheFirstFrameMustBeAHello(t *testing.T) {
	_, _, url := newHub(t)

	conn, _, err := dial(t, url, testToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	// A result for a request nobody made: syntactically a frame, but from
	// something that is not a worker.
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	if err := wsjson.Write(ctx, conn, workerproto.Frame{Type: workerproto.FrameResult, ID: 1}); err != nil {
		t.Fatalf("write: %v", err)
	}

	var frame workerproto.Frame
	err = wsjson.Read(ctx, conn, &frame)
	if got := websocket.CloseStatus(err); got != websocket.StatusProtocolError {
		t.Errorf("close status = %v (err %v), want %v", got, err, websocket.StatusProtocolError)
	}
}

func TestAWorkerSpeakingAnotherProtocolIsRefused(t *testing.T) {
	_, _, url := newHub(t)

	conn, _, err := dial(t, url, testToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	// Server and workers are separate deployments that an image-updating
	// controller rolls independently, so a build skew is routine. Accepting it
	// gives a worker that connects, looks healthy, and answers wrongly.
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	if err := wsjson.Write(ctx, conn, workerproto.Frame{
		Type: workerproto.FrameHello, WorkerID: "worker-from-the-future", Version: "test",
		Protocol: workerproto.ProtocolVersion + 1, Limits: &testLimits,
	}); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	var frame workerproto.Frame
	err = wsjson.Read(ctx, conn, &frame)
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Errorf("close status = %v (err %v), want %v", got, err, websocket.StatusPolicyViolation)
	}
}

func TestAHelloWithoutLimitsIsRefused(t *testing.T) {
	_, _, url := newHub(t)

	// The connection is sized from the declared limits, so a worker that declares
	// none leaves the server guessing — and guessing is the thing this replaced.
	conn, _, err := dial(t, url, testToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	if err := wsjson.Write(ctx, conn, workerproto.Frame{
		Type: workerproto.FrameHello, WorkerID: "worker-a", Version: "test",
		Protocol: workerproto.ProtocolVersion,
	}); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	var frame workerproto.Frame
	err = wsjson.Read(ctx, conn, &frame)
	if got := websocket.CloseStatus(err); got != websocket.StatusProtocolError {
		t.Errorf("close status = %v (err %v), want %v", got, err, websocket.StatusProtocolError)
	}
}

func TestAWorkerDeclaringAbsurdLimitsIsRefused(t *testing.T) {
	_, _, url := newHub(t)

	conn, _, err := dial(t, url, testToken)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	// Otherwise a worker could talk the server into sizing a buffer at whatever it
	// liked, and then fill it.
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	if err := wsjson.Write(ctx, conn, workerproto.Frame{
		Type: workerproto.FrameHello, WorkerID: "worker-a", Version: "test",
		Protocol: workerproto.ProtocolVersion,
		Limits:   &workerproto.Limits{MaxOutputBytes: 1 << 30, MaxFileBytes: 1 << 30},
	}); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	var frame workerproto.Frame
	err = wsjson.Read(ctx, conn, &frame)
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Errorf("close status = %v (err %v), want %v", got, err, websocket.StatusPolicyViolation)
	}
}

func TestTheConnectionIsResizedToWhatTheWorkerDeclared(t *testing.T) {
	h, _, url := newHub(t)

	w := connect(t, url, "worker-a")
	waitForWorkers(t, h, 1)

	// Until the hello arrives the socket only allows a few kilobytes. A result
	// larger than that proves the declared limits were applied — without the
	// resize this write would exceed the bootstrap limit and drop the connection.
	type result struct {
		content string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		content, err := h.ReadFile(context.Background(), "agent-1", "big.txt")
		done <- result{content, err}
	}()

	request := w.nextRequest(t)
	bulky := strings.Repeat("z", 4*workerproto.HelloFrameBytes)
	raw, err := workerproto.Marshal(workerproto.ReadFileResponse{Path: "big.txt", Content: bulky})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	w.write(t, workerproto.Frame{Type: workerproto.FrameResult, ID: request.ID, OK: true, Payload: raw})

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("the connection did not survive a result over the bootstrap limit: %v", got.err)
		}
		if len(got.content) != len(bulky) {
			t.Errorf("content = %d bytes, want %d", len(got.content), len(bulky))
		}
	case <-time.After(waitFor):
		t.Fatal("the result never arrived")
	}
}

// connectHolding is connect for a worker that says it already holds agents,
// which is what a redial after a server restart looks like.
func connectHolding(t *testing.T, baseURL, workerID string, agents ...string) *fakeWorker {
	t.Helper()

	conn, _, err := dial(t, baseURL, testToken)
	if err != nil {
		t.Fatalf("dial as %s: %v", workerID, err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })

	w := &fakeWorker{
		conn:     conn,
		requests: make(chan workerproto.Frame, 16),
		cancels:  make(chan uint64, 16),
	}
	w.write(t, workerproto.Frame{
		Type: workerproto.FrameHello, WorkerID: workerID, Version: "test",
		Protocol: workerproto.ProtocolVersion, Limits: &testLimits, Agents: agents,
	})
	go w.read()
	return w
}

func TestAReconnectingWorkerReclaimsTheAgentsItHolds(t *testing.T) {
	h, _, url := newHub(t)

	// The sandboxes are directories on that worker's disk. Sending the agent
	// anywhere else hands it an empty one while its files sit a pod away.
	connectHolding(t, url, "worker-a", "agent-1", "agent-2")
	waitForWorkers(t, h, 1)

	for _, agentID := range []string{"agent-1", "agent-2"} {
		got, ok := h.WorkerFor(agentID)
		if !ok || got != "worker-a" {
			t.Errorf("%s pinned to %q (%v), want worker-a", agentID, got, ok)
		}
	}
}

func TestAWorkerCannotClaimAnAgentAnotherIsAlreadyServing(t *testing.T) {
	h, _, url := newHub(t)

	a := connect(t, url, "worker-a")
	a.answerStatusWith(t, "worker-a")
	waitForWorkers(t, h, 1)

	// agent-1 is live on worker-a.
	if _, err := h.Status(context.Background(), "agent-1"); err != nil {
		t.Fatalf("status: %v", err)
	}

	// worker-b turns up claiming it too — a stale worker returning after a
	// network partition, say. Honouring that would give one agent two sandboxes
	// and make which one it reaches a matter of routing luck.
	connectHolding(t, url, "worker-b", "agent-1")
	waitForWorkers(t, h, 2)

	if got, ok := h.WorkerFor("agent-1"); !ok || got != "worker-a" {
		t.Errorf("agent-1 moved to %q (%v), want it left on worker-a", got, ok)
	}
}

func TestASecondWorkerClaimingTheSameIDIsRefused(t *testing.T) {
	h, _, url := newHub(t)

	connect(t, url, "worker-a")
	waitForWorkers(t, h, 1)

	// Two connections under one ID would make which one a call reaches a matter
	// of map iteration order.
	conn, _, err := dial(t, url, testToken)
	if err != nil {
		t.Fatalf("second dial: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	if err := wsjson.Write(ctx, conn, workerproto.Frame{
		Type: workerproto.FrameHello, WorkerID: "worker-a", Version: "test",
		Protocol: workerproto.ProtocolVersion, Limits: &testLimits,
	}); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	var frame workerproto.Frame
	err = wsjson.Read(ctx, conn, &frame)
	if got := websocket.CloseStatus(err); got != websocket.StatusPolicyViolation {
		t.Errorf("close status = %v (err %v), want %v", got, err, websocket.StatusPolicyViolation)
	}

	// The incumbent keeps running whatever it is running.
	if workers := h.Workers(); len(workers) != 1 {
		t.Errorf("workers = %d, want the first one still connected alone", len(workers))
	}
}

func TestWithNoWorkerAnAgentIsToldSoRatherThanLeftWaiting(t *testing.T) {
	h, _, _ := newHub(t)

	_, err := h.Status(context.Background(), "agent-1")
	if !errors.Is(err, ErrNoWorker) {
		t.Fatalf("err = %v, want ErrNoWorker", err)
	}
	// Naming the agent is what makes this actionable in a log full of agents.
	if !strings.Contains(err.Error(), "agent-1") {
		t.Errorf("err = %q, want it to name the agent", err)
	}
}

func TestAnAgentKeepsLandingOnTheSameWorker(t *testing.T) {
	h, _, url := newHub(t)

	a := connect(t, url, "worker-a")
	b := connect(t, url, "worker-b")
	a.answerStatusWith(t, "worker-a")
	b.answerStatusWith(t, "worker-b")
	waitForWorkers(t, h, 2)

	first, err := h.Status(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("first status: %v", err)
	}

	// A sandbox is a directory on one worker's disk: a write followed by a read
	// that landed on two pods would lose the file between adjacent calls.
	for i := range 4 {
		got, err := h.Status(context.Background(), "agent-1")
		if err != nil {
			t.Fatalf("status %d: %v", i, err)
		}
		if got.WorkerID != first.WorkerID {
			t.Fatalf("call %d landed on %s, want %s", i, got.WorkerID, first.WorkerID)
		}
	}

	if pinned, ok := h.WorkerFor("agent-1"); !ok || pinned != first.WorkerID {
		t.Errorf("WorkerFor = %q, %v; want %q, true", pinned, ok, first.WorkerID)
	}
}

func TestNewAgentsSpreadAcrossTheWorkers(t *testing.T) {
	h, _, url := newHub(t)

	a := connect(t, url, "worker-a")
	b := connect(t, url, "worker-b")
	a.answerStatusWith(t, "worker-a")
	b.answerStatusWith(t, "worker-b")
	waitForWorkers(t, h, 2)

	// Least-loaded placement is what makes adding a worker useful: without it a
	// scaled-out pool would pile every agent onto whichever worker won the map.
	first, err := h.Status(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("agent-1 status: %v", err)
	}
	second, err := h.Status(context.Background(), "agent-2")
	if err != nil {
		t.Fatalf("agent-2 status: %v", err)
	}

	if first.WorkerID == second.WorkerID {
		t.Errorf("both agents went to %s, want them spread over the two workers", first.WorkerID)
	}
}

func TestWhenAWorkerGoesAwayTheCallFailsAndWatchersAreTold(t *testing.T) {
	h, bus, url := newHub(t)

	w := connect(t, url, "worker-a")
	waitForWorkers(t, h, 1)

	_, sub := bus.Subscribe("agent-1", 0)
	defer bus.Unsubscribe("agent-1", sub)

	failed := make(chan error, 1)
	go func() {
		_, err := h.Exec(context.Background(), "agent-1", workerproto.ExecRequest{Command: "/bin/true"})
		failed <- err
	}()

	// Wait until the request is genuinely in flight, then take the worker away
	// mid-command, as a scale-down or an eviction would.
	w.nextRequest(t)
	_ = w.conn.CloseNow()

	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("exec succeeded although the worker vanished")
		}
		if !strings.Contains(err.Error(), "disconnect") {
			t.Errorf("err = %q, want it to say the worker disconnected", err)
		}
	case <-time.After(waitFor):
		t.Fatal("exec never returned after the worker vanished")
	}

	// Otherwise the terminal simply stops, which looks like a command still
	// thinking rather than a sandbox that no longer exists.
	select {
	case event, ok := <-sub.Events():
		if !ok {
			t.Fatal("the subscription ended without a killed event")
		}
		if event.Kind != stream.EventKilled {
			t.Errorf("event kind = %q, want %q", event.Kind, stream.EventKilled)
		}
		// The wording has to stop short of declaring the sandbox lost, because a
		// worker that redials reclaims what it still holds.
		if !strings.Contains(event.Reason, "stopped") {
			t.Errorf("reason = %q, want it to say what stopped", event.Reason)
		}
	case <-time.After(waitFor):
		t.Fatal("no killed event reached the watcher")
	}
}

func TestGivingUpOnACallTellsTheWorkerToStop(t *testing.T) {
	h, _, url := newHub(t)

	w := connect(t, url, "worker-a")
	waitForWorkers(t, h, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := h.Exec(ctx, "agent-1", workerproto.ExecRequest{Command: "/bin/sleep", Args: []string{"60"}})
		done <- err
	}()

	request := w.nextRequest(t)
	cancel()

	// Without the cancel the command keeps running in a pod nobody is listening
	// to, until its own timeout.
	select {
	case id := <-w.cancels:
		if id != request.ID {
			t.Errorf("cancelled request %d, want %d", id, request.ID)
		}
	case <-time.After(waitFor):
		t.Fatal("the worker was never told to cancel")
	}

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestOneAgentGivingUpDoesNotDisconnectTheWorker(t *testing.T) {
	h, _, url := newHub(t)

	w := connect(t, url, "worker-a")
	w.answerStatusWith(t, "worker-a")
	waitForWorkers(t, h, 1)

	// The cancels have to be drained or the fake worker's read loop wedges on a
	// full channel, which would look like the bug this test is about.
	go func() {
		for range w.cancels {
		}
	}()

	// The frame for a request used to be written with the calling agent's context.
	// A write interrupted by a cancelled context leaves a partial frame, so the
	// library closes the connection — and the connection is shared. So an agent
	// cancelling its own call took every other agent's sandbox on that worker with
	// it. Around a hundred cancellations were enough.
	for i := range 300 {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { _, _ = h.Exec(ctx, "agent-1", workerproto.ExecRequest{Command: "/bin/true"}) }()
		cancel()

		if len(h.Workers()) == 0 {
			t.Fatalf("the worker connection died after %d cancelled calls", i+1)
		}
	}

	// The point is the bystander: an agent that cancelled nothing must not have
	// lost anything.
	if _, err := h.Status(context.Background(), "bystander"); err != nil {
		t.Fatalf("an agent that cancelled nothing lost its worker: %v", err)
	}
}

func TestOutputEventsFromAWorkerReachWatchers(t *testing.T) {
	h, bus, url := newHub(t)

	w := connect(t, url, "worker-a")
	waitForWorkers(t, h, 1)

	_, sub := bus.Subscribe("agent-1", 0)
	defer bus.Unsubscribe("agent-1", sub)

	w.write(t, workerproto.Frame{Type: workerproto.FrameEvent, Event: &stream.Event{
		SandboxID: "agent-1",
		Kind:      stream.EventStdout,
		ExecID:    "exec-1",
		Data:      "hello from the worker",
	}})

	select {
	case event := <-sub.Events():
		if event.Data != "hello from the worker" {
			t.Errorf("data = %q", event.Data)
		}
		// Sequence numbers are the server's to assign: a worker cannot know where
		// its events fall in a sandbox's history, least of all after a reconnect.
		if event.Seq != 1 {
			t.Errorf("seq = %d, want the hub to have stamped it 1", event.Seq)
		}
	case <-time.After(waitFor):
		t.Fatal("the event never reached the watcher")
	}
}

func TestWorkersReportsWhoIsServingWhom(t *testing.T) {
	h, _, url := newHub(t)

	w := connect(t, url, "worker-a")
	w.answerStatusWith(t, "worker-a")
	waitForWorkers(t, h, 1)

	if _, err := h.Status(context.Background(), "agent-1"); err != nil {
		t.Fatalf("status: %v", err)
	}

	workers := h.Workers()
	if len(workers) != 1 {
		t.Fatalf("workers = %d, want 1", len(workers))
	}
	if workers[0].WorkerID != "worker-a" || workers[0].Version != "test" {
		t.Errorf("worker = %+v, want worker-a at version test", workers[0])
	}
	if len(workers[0].Agents) != 1 || workers[0].Agents[0] != "agent-1" {
		t.Errorf("agents = %v, want [agent-1]", workers[0].Agents)
	}
	if workers[0].ConnectedAt.IsZero() {
		t.Error("ConnectedAt is zero")
	}
}
