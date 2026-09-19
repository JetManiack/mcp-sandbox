// Package workerhub is the server's side of the worker link: it accepts the
// connections workers dial in on, routes each agent's operations to one of them,
// and forwards the output events they send back into the terminal stream.
//
// Workers dial the server rather than the reverse, so a worker pod needs no
// Service, no ingress and no discovery — which is what makes it something k8s can
// lock down.
package workerhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/JetManiack/mcp-sandbox/internal/stream"
	"github.com/JetManiack/mcp-sandbox/internal/workerproto"
)

// ErrNoWorker reports that no worker is available to serve an agent. It is
// deliberately distinct from a timeout: an agent that reads "no worker" knows
// waiting will not help.
var ErrNoWorker = errors.New("no execution worker is connected")

// writeTimeout bounds one frame write, so a worker whose socket has stopped
// draining cannot pin a request forever.
const writeTimeout = 10 * time.Second

// Hub tracks connected workers and which agent each one is serving.
type Hub struct {
	bus *stream.Broadcaster

	mu sync.RWMutex
	// workers is every live connection, and assignments pins an agent to one of
	// them.
	//
	// The pin is what makes a scaled-out pool usable at all: a sandbox is a
	// directory on that worker's disk, so a second call from the same agent has to
	// land on the same worker or the files written by the first are simply not
	// there.
	workers     map[string]*worker
	assignments map[string]*worker
}

// New returns a Hub publishing worker events into bus.
func New(bus *stream.Broadcaster) *Hub {
	return &Hub{
		bus:         bus,
		workers:     make(map[string]*worker),
		assignments: make(map[string]*worker),
	}
}

// WorkerInfo describes a connected worker for the operator-facing API.
type WorkerInfo struct {
	WorkerID    string    `json:"worker_id"`
	Version     string    `json:"version"`
	ConnectedAt time.Time `json:"connected_at"`
	Agents      []string  `json:"agents"`

	// Limits are what this worker declared. Reported because they are per worker
	// now, so "why was that file refused" is answerable without reading the pod's
	// flags.
	Limits workerproto.Limits `json:"limits"`
}

// Workers reports every connected worker and the agents pinned to it.
func (h *Hub) Workers() []WorkerInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()

	byWorker := make(map[string][]string, len(h.workers))
	for agentID, w := range h.assignments {
		byWorker[w.id] = append(byWorker[w.id], agentID)
	}

	infos := make([]WorkerInfo, 0, len(h.workers))
	for _, w := range h.workers {
		infos = append(infos, WorkerInfo{
			WorkerID:    w.id,
			Version:     w.version,
			ConnectedAt: w.connectedAt,
			Agents:      byWorker[w.id],
			Limits:      w.limits,
		})
	}
	return infos
}

// WorkerFor reports which worker is serving agentID, if any.
func (h *Hub) WorkerFor(agentID string) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	w, ok := h.assignments[agentID]
	if !ok {
		return "", false
	}
	return w.id, true
}

// Handler is the endpoint workers dial. token must match the one they present;
// an empty token refuses every connection rather than accepting all of them,
// because the opposite default turns a misconfiguration into an open execution
// service.
func (h *Hub) Handler(token string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if token == "" {
			http.Error(w, "worker endpoint is disabled: no --worker-token configured", http.StatusServiceUnavailable)
			return
		}
		presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || presented != token {
			w.Header().Set("WWW-Authenticate", `Bearer realm="executor-worker"`)
			http.Error(w, "invalid worker token", http.StatusUnauthorized)
			return
		}

		// No AcceptOptions: a worker is not a browser and sends no Origin header,
		// which the library already treats as acceptable. Reaching for
		// InsecureSkipVerify here would disable Origin checking for anything that
		// does send one, including a browser page that got hold of the token.
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			slog.Debug("worker websocket accept failed", "error", err)
			return
		}

		// Only enough for a hello. Until one arrives there is nothing to size the
		// connection from, and a peer that has not identified itself is the last
		// one to trust with a large allocation.
		conn.SetReadLimit(workerproto.HelloFrameBytes)

		h.serve(r.Context(), conn)
	})
}

// worker is one live connection.
type worker struct {
	id          string
	version     string
	limits      workerproto.Limits
	connectedAt time.Time

	conn *websocket.Conn

	// writeMu serializes frame writes: several agents' requests share one socket,
	// and a WebSocket connection permits one writer at a time.
	writeMu sync.Mutex

	mu       sync.Mutex
	nextID   uint64
	pending  map[uint64]chan workerproto.Frame
	closed   bool
	closeErr error
}

// send writes one frame, bounded by its own deadline and by nothing else.
//
// Deliberately not the caller's context, and this is load-bearing rather than
// tidy. A write interrupted by a cancelled context leaves a partial frame in the
// stream, so the library closes the connection — and several agents are
// multiplexed over this one. Handing an agent's context to the write would mean
// that agent cancelling its own call could take down every other agent's sandbox
// on that worker. Measured, not theorised: about a hundred cancelled calls killed
// the connection, and a bystander lost its worker.
//
// A caller that gives up is served by the select in call, which stops waiting and
// tells the worker to cancel. That is what cancellation is for here; aborting the
// write is not.
func (w *worker) send(frame workerproto.Frame) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()

	writeCtx, cancel := context.WithTimeout(context.Background(), writeTimeout)
	defer cancel()
	return wsjson.Write(writeCtx, w.conn, frame)
}

// serve reads a connection until it ends: the hello frame, then results and
// events.
func (h *Hub) serve(ctx context.Context, conn *websocket.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var hello workerproto.Frame
	if err := wsjson.Read(ctx, conn, &hello); err != nil {
		_ = conn.Close(websocket.StatusProtocolError, "expected a hello frame")
		return
	}
	if hello.Type != workerproto.FrameHello || hello.WorkerID == "" {
		_ = conn.Close(websocket.StatusProtocolError, "first frame must be a hello naming the worker")
		return
	}
	// Refused loudly rather than tolerated: a worker speaking a different
	// vocabulary connects, looks healthy, and answers tool calls wrongly. A pod
	// that will not start is a rollout an operator notices; a pod that answers
	// incorrectly is one nobody notices.
	if hello.Protocol != workerproto.ProtocolVersion {
		slog.Warn("refusing a worker speaking a different protocol",
			"worker_id", hello.WorkerID, "worker_protocol", hello.Protocol,
			"server_protocol", workerproto.ProtocolVersion, "worker_version", hello.Version)
		_ = conn.Close(websocket.StatusPolicyViolation, fmt.Sprintf(
			"this server speaks worker protocol %d, the worker speaks %d — update whichever is older",
			workerproto.ProtocolVersion, hello.Protocol))
		return
	}
	if hello.Limits == nil {
		_ = conn.Close(websocket.StatusProtocolError, "hello must declare the worker's limits")
		return
	}
	limits := *hello.Limits
	// Validated rather than taken: the read limit is set from these numbers, so an
	// unchecked one is a worker talking the server into an allocation.
	if err := limits.Validate(); err != nil {
		slog.Warn("refusing a worker with unusable limits", "worker_id", hello.WorkerID, "error", err)
		_ = conn.Close(websocket.StatusPolicyViolation, "unusable limits: "+err.Error())
		return
	}
	// Now the connection can be sized to what this worker actually sends, which is
	// the point of it declaring anything.
	conn.SetReadLimit(int64(limits.FrameBytes()))

	w := &worker{
		id:          hello.WorkerID,
		version:     hello.Version,
		limits:      limits,
		connectedAt: time.Now().UTC(),
		conn:        conn,
		pending:     make(map[uint64]chan workerproto.Frame),
	}

	h.mu.Lock()
	if _, exists := h.workers[w.id]; exists {
		h.mu.Unlock()
		// Two connections claiming one worker ID would make routing a matter of
		// map iteration order, so the newcomer is refused rather than silently
		// replacing whatever the first one is running.
		_ = conn.Close(websocket.StatusPolicyViolation, "a worker with that id is already connected")
		return
	}
	h.workers[w.id] = w
	// Pin back the agents this worker says it already holds. Their sandboxes are
	// on its disk, so anywhere else is an empty directory and a lost afternoon.
	//
	// An agent already served by another live worker is left alone: two workers
	// holding one agent would make which sandbox it talks to a matter of routing
	// luck, which is the thing pinning exists to prevent. The incumbent wins
	// because it is the one the agent's recent calls have been reaching.
	var restored, contested int
	for _, agentID := range hello.Agents {
		if agentID == "" {
			continue
		}
		if existing, ok := h.assignments[agentID]; ok && existing != w {
			contested++
			continue
		}
		h.assignments[agentID] = w
		restored++
	}
	h.mu.Unlock()

	slog.Info("worker connected", "worker_id", w.id, "version", w.version,
		"restored_agents", restored, "contested_agents", contested)
	if contested > 0 {
		slog.Warn("some agents this worker claims are already served elsewhere; leaving them where they are",
			"worker_id", w.id, "count", contested)
	}
	defer h.drop(w, errors.New("worker disconnected"))

	for {
		var frame workerproto.Frame
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			return
		}

		switch frame.Type {
		case workerproto.FrameResult:
			w.deliver(frame)
		case workerproto.FrameEvent:
			if frame.Event != nil {
				// Sequence numbers are assigned here, by the one process that
				// retains the stream — a worker cannot know where its events fall
				// in a sandbox's history, least of all after a reconnect.
				h.bus.Publish(*frame.Event)
			}
		default:
			slog.Warn("unexpected frame from worker", "worker_id", w.id, "type", frame.Type)
		}
	}
}

func (w *worker) deliver(frame workerproto.Frame) {
	w.mu.Lock()
	waiter, ok := w.pending[frame.ID]
	delete(w.pending, frame.ID)
	w.mu.Unlock()

	if !ok {
		// A result for a request that already gave up (its context expired). Not
		// an error: the caller is gone and there is nobody to tell.
		return
	}
	waiter <- frame
}

// drop removes a worker and fails everything still waiting on it.
func (h *Hub) drop(w *worker, reason error) {
	h.mu.Lock()
	delete(h.workers, w.id)
	var orphaned []string
	for agentID, assigned := range h.assignments {
		if assigned == w {
			delete(h.assignments, agentID)
			orphaned = append(orphaned, agentID)
		}
	}
	h.mu.Unlock()

	w.mu.Lock()
	w.closed = true
	w.closeErr = reason
	waiters := w.pending
	w.pending = map[uint64]chan workerproto.Frame{}
	w.mu.Unlock()

	for id, waiter := range waiters {
		waiter <- workerproto.Frame{Type: workerproto.FrameResult, ID: id, OK: false, Error: reason.Error()}
	}

	// Watchers are told, because otherwise a terminal simply stops and looks like
	// a command that is still thinking.
	//
	// The wording stops short of declaring the sandbox gone, because that is no
	// longer certain: a worker that redials reclaims the agents it still holds, so
	// the files come back if the pod survived whatever this was. What did not
	// survive is the work in flight, and that is what the message says.
	for _, agentID := range orphaned {
		h.bus.Publish(stream.Event{
			SandboxID: agentID,
			Kind:      stream.EventKilled,
			At:        time.Now().UTC(),
			ByActor:   "worker " + w.id,
			Reason: "worker disconnected; anything running has stopped, and the sandbox returns only if " +
				"that worker reconnects still holding it",
		})
	}

	slog.Info("worker disconnected", "worker_id", w.id, "orphaned_agents", len(orphaned))
}

// workerFor returns the worker serving agentID, pinning one on first use.
//
// A new agent goes to the connection currently serving the fewest, which is what
// makes adding a worker useful: the pool spreads without any coordination beyond
// this map.
func (h *Hub) workerFor(agentID string) (*worker, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if w, ok := h.assignments[agentID]; ok {
		return w, nil
	}
	if len(h.workers) == 0 {
		return nil, ErrNoWorker
	}

	load := make(map[string]int, len(h.workers))
	for _, w := range h.assignments {
		load[w.id]++
	}

	var best *worker
	for _, w := range h.workers {
		if best == nil || load[w.id] < load[best.id] ||
			(load[w.id] == load[best.id] && w.id < best.id) {
			best = w
		}
	}
	// Unreachable, given the emptiness check above — and stated anyway, because
	// every caller dereferences what this returns, and "cannot be nil unless the
	// map is empty, which we checked" is an invariant a later edit can break
	// silently. Failing as ErrNoWorker is also the honest answer if it ever did.
	if best == nil {
		return nil, ErrNoWorker
	}
	h.assignments[agentID] = best
	return best, nil
}

// call sends one request and waits for its result.
func (h *Hub) call(ctx context.Context, agentID string, op workerproto.Op, payload any, out any) error {
	w, err := h.workerFor(agentID)
	if err != nil {
		return fmt.Errorf("%w for sandbox %s", err, agentID)
	}

	raw, err := workerproto.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s request: %w", op, err)
	}
	// Refused here rather than discovered by the worker's read limit, which would
	// close the connection and take every other agent on that worker with it. The
	// bound is the one this worker declared, so it is the worker's own limit being
	// enforced rather than a number the server guessed.
	if len(raw) > w.limits.MaxPayloadBytes() {
		return workerproto.ErrPayloadTooLarge(op, len(raw), w.limits.MaxPayloadBytes())
	}

	waiter := make(chan workerproto.Frame, 1)
	w.mu.Lock()
	if w.closed {
		err := w.closeErr
		w.mu.Unlock()
		return fmt.Errorf("worker %s: %w", w.id, err)
	}
	w.nextID++
	id := w.nextID
	w.pending[id] = waiter
	w.mu.Unlock()

	if err := w.send(workerproto.Frame{
		Type:    workerproto.FrameRequest,
		ID:      id,
		AgentID: agentID,
		Op:      op,
		Payload: raw,
	}); err != nil {
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()
		return fmt.Errorf("send %s to worker %s: %w", op, w.id, err)
	}

	select {
	case frame := <-waiter:
		if !frame.OK {
			return errors.New(frame.Error)
		}
		if out == nil {
			return nil
		}
		return json.Unmarshal(frame.Payload, out)

	case <-ctx.Done():
		// Tell the worker to stop: without this the command keeps running in a pod
		// nobody is listening to until its own timeout.
		w.mu.Lock()
		delete(w.pending, id)
		w.mu.Unlock()

		if err := w.send(workerproto.Frame{Type: workerproto.FrameCancel, ID: id}); err != nil {
			slog.Warn("could not tell the worker to cancel", "worker_id", w.id, "error", err)
		}
		return ctx.Err()
	}
}
