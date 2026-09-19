package restapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/go-chi/chi/v5"

	"github.com/JetManiack/mcp-sandbox/internal/storage"
	"github.com/JetManiack/mcp-sandbox/internal/stream"
)

// writeTimeout bounds a single frame write, not the connection. A watcher whose
// socket has stopped draining must not pin a goroutine and a subscription
// forever; it is disconnected and reconnects, which reports the gap.
const writeTimeout = 10 * time.Second

// closeCodeSlowConsumer is sent when a watcher fell behind the event stream. It
// is in the private-use range (4000-4999) reserved for application codes.
const closeCodeSlowConsumer websocket.StatusCode = 4000

// streamSandboxHandler upgrades to a WebSocket and streams one sandbox's events.
//
// The stream is strictly read-only: nothing arriving from the client is
// interpreted as a command. The read side exists only so close frames and the
// library's ping/pong keepalive are processed — without reading, a closed browser
// tab would not be noticed until the next write failed.
//
// A watcher resumes with ?after=<seq>. If the requested position has already been
// evicted from the ring buffer, the first event delivered is a gap marker: the
// terminal has to admit it cannot show a continuous picture rather than silently
// drawing one.
func streamSandboxHandler(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actorID := chi.URLParam(r, "id")

		if _, err := storage.GetActorByID(opts.DB, actorID); err != nil {
			if errors.Is(err, storage.ErrActorNotFound) {
				writeError(w, http.StatusNotFound, err)
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		after, err := parseAfter(r.URL.Query().Get("after"))
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}

		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			// The UI is served from this same origin, so same-origin is the
			// whole allowed set. Left permissive, any page on the internet
			// could open this socket using the operator's cookie and read the
			// terminal.
			OriginPatterns: nil,
		})
		if err != nil {
			// Accept has already written its own error response.
			slog.Debug("websocket accept failed", "actor_id", actorID, "error", err)
			return
		}

		ctx := conn.CloseRead(r.Context())
		replay, sub := opts.Bus.Subscribe(actorID, after)
		defer opts.Bus.Unsubscribe(actorID, sub)

		for _, event := range replay {
			if !writeEvent(ctx, conn, event) {
				return
			}
		}

		for {
			select {
			case <-ctx.Done():
				_ = conn.Close(websocket.StatusNormalClosure, "client went away")
				return
			case event, open := <-sub.Events():
				if !open {
					if sub.Lagged() {
						_ = conn.Close(closeCodeSlowConsumer, "slow consumer: reconnect with ?after=<last seq>")
						return
					}
					_ = conn.Close(websocket.StatusNormalClosure, "stream ended")
					return
				}
				if !writeEvent(ctx, conn, event) {
					return
				}
			}
		}
	}
}

// writeEvent sends one event, reporting whether the connection is still usable.
func writeEvent(ctx context.Context, conn *websocket.Conn, event stream.Event) bool {
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	if err := wsjson.Write(writeCtx, conn, event); err != nil {
		// A watcher closing its tab mid-stream is the common case, not a fault.
		slog.Debug("terminal stream write failed", "sandbox_id", event.SandboxID, "error", err)
		_ = conn.Close(websocket.StatusInternalError, "write failed")
		return false
	}
	return true
}

// parseAfter reads the resume position, treating an absent value as "from the
// start of what is retained".
func parseAfter(raw string) (uint64, error) {
	if raw == "" {
		return 0, nil
	}
	after, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, errors.New("after must be a non-negative integer sequence number")
	}
	return after, nil
}
