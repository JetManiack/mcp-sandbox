// Package workerlink is the worker's side of the link: it dials the server, keeps
// one connection, and serves the operations that arrive on it against a local
// sandbox manager.
//
// The worker holds no database, no credentials beyond its own token and no
// listening socket. Everything it knows about an agent arrives in a request.
package workerlink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/JetManiack/mcp-sandbox/internal/sandbox"
	"github.com/JetManiack/mcp-sandbox/internal/stream"
	"github.com/JetManiack/mcp-sandbox/internal/workerproto"
)

const (
	writeTimeout      = 10 * time.Second
	minReconnectDelay = time.Second
	maxReconnectDelay = 30 * time.Second
	handshakeTimeout  = 15 * time.Second
)

// Config is what a worker needs to reach its server.
type Config struct {
	// ServerURL is the server's base URL: http(s):// or ws(s)://, either accepted.
	ServerURL string

	// Token authenticates this worker to the server.
	Token string

	// WorkerID names this worker in the server's logs and its operator API.
	WorkerID string

	// Version is reported in the hello frame.
	Version string

	// MaxFileBytes caps the content of one read_file or write_file. Zero selects
	// DefaultMaxFileBytes.
	MaxFileBytes int
}

// DefaultMaxFileBytes is the largest file this worker will read or write in one
// operation.
//
// Eight megabytes because that is what the wire already allowed: doubled for
// escaping and given an envelope, it produces the same 16MB socket the link used
// when the number was hard-coded. It is a limit on a single transfer, not on what
// a sandbox may hold — a command can write a file of any size, and only moving it
// through a tool call is bounded.
const DefaultMaxFileBytes = 8 << 20

// Link runs one worker's connection to the server.
type Link struct {
	cfg     Config
	limits  workerproto.Limits
	manager *sandbox.Manager
	dialURL string

	// writeMu serializes frame writes: results and events are produced by many
	// goroutines and a WebSocket permits one writer at a time.
	writeMu sync.Mutex
	conn    *websocket.Conn

	mu       sync.Mutex
	inflight map[uint64]context.CancelFunc
}

// New validates cfg and returns a Link serving sandboxes rooted at
// sandboxCfg.RootDir.
//
// The manager is built here rather than passed in because it needs this link as
// its event sink, and the link needs the manager to serve requests — one of the
// two has to own the other, and the link is what outlives a reconnect.
func New(cfg Config, sandboxCfg sandbox.Config) (*Link, error) {
	if cfg.ServerURL == "" {
		return nil, errors.New("a server URL is required")
	}
	if cfg.Token == "" {
		return nil, errors.New("a worker token is required")
	}
	if cfg.WorkerID == "" {
		return nil, errors.New("a worker id is required")
	}

	if cfg.MaxFileBytes == 0 {
		cfg.MaxFileBytes = DefaultMaxFileBytes
	}
	// sandbox.New applies its own default when the cap is unset, and the socket has
	// to be sized for what the sandbox will actually produce rather than for the
	// zero written in the config.
	sandboxCfg = sandboxCfg.WithDefaults()

	limits := workerproto.Limits{
		MaxOutputBytes: sandboxCfg.MaxOutputBytes,
		MaxFileBytes:   cfg.MaxFileBytes,
	}
	// Refused here rather than at the handshake, so an operator reading the error
	// is looking at the process whose flags are wrong.
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	// One output chunk becomes one event frame, and events are the traffic nothing
	// else on this link bounds. It fits today because the envelope is larger than a
	// chunk — which is a coincidence rather than a rule, and coincidences between
	// limits are what this check exists to stop being silent.
	if sandbox.ChunkSize > limits.MaxPayloadBytes() {
		return nil, fmt.Errorf(
			"limits allow a %d-byte payload, under the %d bytes one output chunk needs, so streaming would break the connection",
			limits.MaxPayloadBytes(), sandbox.ChunkSize)
	}

	url := strings.TrimSuffix(cfg.ServerURL, "/")
	switch {
	case strings.HasPrefix(url, "https://"):
		url = "wss://" + strings.TrimPrefix(url, "https://")
	case strings.HasPrefix(url, "http://"):
		url = "ws://" + strings.TrimPrefix(url, "http://")
	case strings.HasPrefix(url, "wss://"), strings.HasPrefix(url, "ws://"):
	default:
		return nil, fmt.Errorf("server URL %q must start with http://, https://, ws:// or wss://", cfg.ServerURL)
	}

	link := &Link{
		cfg:      cfg,
		limits:   limits,
		dialURL:  url + workerproto.Path,
		inflight: make(map[uint64]context.CancelFunc),
	}

	// The sandbox stamps each event with the agent it belongs to, so one sink for
	// the whole manager is enough — nothing here needs per-agent state.
	manager, err := sandbox.NewManager(sandboxCfg, sandbox.SinkFunc(link.forward))
	if err != nil {
		return nil, err
	}
	link.manager = manager
	return link, nil
}

// Run keeps the worker connected until ctx is cancelled, redialing with capped
// backoff.
//
// A lost connection is expected rather than exceptional — the server rolls, the
// network blips — so this reconnects instead of exiting. What does not survive is
// the sandboxes: their directories live on this pod's emptyDir, and the server
// tells watchers so.
func (l *Link) Run(ctx context.Context) error {
	delay := minReconnectDelay
	for {
		err := l.session(ctx)
		if ctx.Err() != nil {
			return nil
		}
		slog.Warn("worker link ended; reconnecting", "error", err, "in", delay)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}
		delay = min(delay*2, maxReconnectDelay)
	}
}

// session runs one connection from dial to close.
func (l *Link) session(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()

	conn, _, err := websocket.Dial(dialCtx, l.dialURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + l.cfg.Token}},
	})
	if err != nil {
		return fmt.Errorf("dial %s: %w", l.dialURL, err)
	}
	defer func() { _ = conn.CloseNow() }()

	// Sized to what this worker declares, so the two ends of the socket agree by
	// construction rather than by two constants happening to match.
	conn.SetReadLimit(int64(l.limits.FrameBytes()))

	l.writeMu.Lock()
	l.conn = conn
	l.writeMu.Unlock()

	// Which agents this worker is already holding. Empty on a first connect, and
	// on a redial after the pod restarted — the directories may still be on the
	// emptyDir, but this process no longer knows whose they are, and guessing from
	// the filesystem would resurrect agents that have since been deleted.
	held := slices.Sorted(maps.Keys(l.manager.LiveSandboxes()))

	limits := l.limits
	if err := l.send(ctx, workerproto.Frame{
		Type:     workerproto.FrameHello,
		WorkerID: l.cfg.WorkerID,
		Version:  l.cfg.Version,
		Protocol: workerproto.ProtocolVersion,
		Limits:   &limits,
		Agents:   held,
	}); err != nil {
		return fmt.Errorf("send hello: %w", err)
	}
	slog.Info("worker connected to server", "url", l.dialURL, "worker_id", l.cfg.WorkerID)

	for {
		var frame workerproto.Frame
		if err := wsjson.Read(ctx, conn, &frame); err != nil {
			return err
		}

		switch frame.Type {
		case workerproto.FrameRequest:
			// One goroutine per request: several agents share this connection, and a
			// two-minute build must not stall another agent's file read.
			go l.handle(ctx, frame)
		case workerproto.FrameCancel:
			l.cancel(frame.ID)
		default:
			slog.Warn("unexpected frame from server", "type", frame.Type)
		}
	}
}

func (l *Link) send(ctx context.Context, frame workerproto.Frame) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	if l.conn == nil {
		return errors.New("not connected")
	}

	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return wsjson.Write(writeCtx, l.conn, frame)
}

// forward sends one sandbox event to the server, which assigns its sequence
// number and fans it out — this worker holds no retention buffer and has no
// watchers of its own.
//
// context.Background rather than a request's context: an event outlives the call
// that triggered it (a finished event is published as the command returns), and
// send applies its own write deadline.
func (l *Link) forward(event stream.Event) {
	if err := l.send(context.Background(), workerproto.Frame{Type: workerproto.FrameEvent, Event: &event}); err != nil {
		// Losing an event must not fail the command that produced it:
		// observability that breaks what it observes is worse than none.
		slog.Debug("could not forward a sandbox event", "sandbox_id", event.SandboxID, "error", err)
	}
}

func (l *Link) cancel(id uint64) {
	l.mu.Lock()
	cancel, ok := l.inflight[id]
	delete(l.inflight, id)
	l.mu.Unlock()
	if ok {
		cancel()
	}
}

// handle performs one request and answers it.
func (l *Link) handle(parent context.Context, frame workerproto.Frame) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	l.mu.Lock()
	l.inflight[frame.ID] = cancel
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.inflight, frame.ID)
		l.mu.Unlock()
	}()

	payload, err := l.dispatch(ctx, frame)
	reply := workerproto.Frame{Type: workerproto.FrameResult, ID: frame.ID}
	if err != nil {
		reply.Error = err.Error()
	} else {
		raw, marshalErr := workerproto.Marshal(payload)
		switch {
		case marshalErr != nil:
			reply.Error = fmt.Sprintf("encode %s result: %v", frame.Op, marshalErr)
		case len(raw) > l.limits.MaxPayloadBytes():
			// An answer too big to send — a read_file of something enormous. Sent
			// anyway it would trip the server's read limit, closing the connection
			// and orphaning every other agent this worker is serving.
			reply.Error = workerproto.ErrPayloadTooLarge(frame.Op, len(raw), l.limits.MaxPayloadBytes()).Error()
		default:
			reply.OK = true
			reply.Payload = raw
		}
	}

	if err := l.send(parent, reply); err != nil {
		slog.Warn("could not answer a request", "op", frame.Op, "id", frame.ID, "error", err)
	}
}
