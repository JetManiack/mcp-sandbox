package restapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/JetManiack/mcp-sandbox/internal/humanauth"
	"github.com/JetManiack/mcp-sandbox/internal/storage"
	"github.com/JetManiack/mcp-sandbox/internal/stream"
)

// sandboxResponse is one row of the sandbox list.
//
// Live means a worker is currently serving this agent, and Worker names it: in a
// scaled-out pool "which pod holds my sandbox" is the question an operator asks
// first. An agent with no worker can still be blocked — the block is what refuses
// its next call, wherever that lands.
type sandboxResponse struct {
	ActorID     string                `json:"actor_id"`
	DisplayName string                `json:"display_name"`
	Live        bool                  `json:"live"`
	Worker      string                `json:"worker,omitempty"`
	Watchers    int                   `json:"watchers"`
	LastSeq     uint64                `json:"last_seq"`
	Block       *storage.SandboxBlock `json:"block,omitempty"`
}

type blockRequest struct {
	Reason string `json:"reason"`
}

type blockResponse struct {
	Block           *storage.SandboxBlock `json:"block"`
	KilledProcesses int                   `json:"killed_processes"`
}

func listSandboxesHandler(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agents, err := storage.ListAgents(opts.DB)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		blocks, err := storage.ActiveSandboxBlocks(opts.DB)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		resp := make([]sandboxResponse, 0, len(agents))
		for _, agent := range agents {
			row := sandboxResponse{
				ActorID:     agent.ID,
				DisplayName: agent.DisplayName,
				Block:       blocks[agent.ID],
				LastSeq:     opts.Bus.LastSeq(agent.ID),
				Watchers:    opts.Bus.WatcherCount(agent.ID),
			}
			// Deliberately not asking every worker for a running-command count:
			// the list is polled, and one round trip per agent per poll would put
			// the pool under load proportional to how many operators have the page
			// open. The terminal shows what is running.
			if workerID, ok := opts.Hub.WorkerFor(agent.ID); ok {
				row.Live = true
				row.Worker = workerID
			}
			resp = append(resp, row)
		}

		// Blocked first, then busiest: an operator opening this list is looking
		// for something to act on, not an alphabetical directory.
		sort.SliceStable(resp, func(i, j int) bool {
			if (resp[i].Block != nil) != (resp[j].Block != nil) {
				return resp[i].Block != nil
			}
			if resp[i].Live != resp[j].Live {
				return resp[i].Live
			}
			return resp[i].DisplayName < resp[j].DisplayName
		})

		writeJSON(w, http.StatusOK, resp)
	}
}

func getSandboxHandler(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actorID := chi.URLParam(r, "id")

		agent, err := storage.GetActorByID(opts.DB, actorID)
		if errors.Is(err, storage.ErrActorNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		block, err := storage.ActiveSandboxBlock(opts.DB, actorID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		resp := sandboxResponse{
			ActorID:     agent.ID,
			DisplayName: agent.DisplayName,
			Block:       block,
			LastSeq:     opts.Bus.LastSeq(actorID),
			Watchers:    opts.Bus.WatcherCount(actorID),
		}
		if workerID, ok := opts.Hub.WorkerFor(actorID); ok {
			resp.Live = true
			resp.Worker = workerID
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// blockSandboxHandler is the emergency stop: it records the block, then kills
// every process group running in the sandbox.
//
// The block is recorded first, deliberately. Killing first would leave a window
// in which the processes are gone but nothing stops the agent from starting more
// — which, for the runaway loop this button exists to stop, means it simply
// restarts. Recording first makes the refusal effective before the teardown.
func blockSandboxHandler(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actorID := chi.URLParam(r, "id")

		admin, ok := humanauth.ActorFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errors.New("no authenticated actor"))
			return
		}

		var req blockRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errors.New("body must be a JSON object with reason"))
			return
		}
		req.Reason = strings.TrimSpace(req.Reason)

		if _, err := storage.GetActorByID(opts.DB, actorID); errors.Is(err, storage.ErrActorNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		} else if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		block, err := storage.BlockSandbox(opts.DB, actorID, admin, req.Reason)
		switch {
		case errors.Is(err, storage.ErrEmptyReason):
			writeError(w, http.StatusBadRequest, err)
			return
		case errors.Is(err, storage.ErrAlreadyBlocked):
			writeError(w, http.StatusConflict, err)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		killed, killErr := opts.Hub.Kill(r.Context(), actorID, admin.DisplayName, req.Reason)
		if killErr != nil {
			// The block is already in force, which is the part that matters, so
			// this is reported rather than rolled back: a partially-killed
			// sandbox that cannot start anything new is a safe state.
			slog.Error("kill sandbox processes", "actor_id", actorID, "error", killErr)
		}
		if killed > 0 {
			if err := storage.RecordKilledProcesses(opts.DB, block.ID, killed); err != nil {
				slog.Error("record killed process count", "block_id", block.ID, "error", err)
			}
			block.KilledProcesses = killed
		}

		opts.Bus.Publish(blockedEvent(actorID, admin.DisplayName, req.Reason))
		writeJSON(w, http.StatusCreated, blockResponse{Block: block, KilledProcesses: killed})
	}
}

func releaseSandboxHandler(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actorID := chi.URLParam(r, "id")

		admin, ok := humanauth.ActorFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errors.New("no authenticated actor"))
			return
		}

		err := storage.ReleaseSandbox(opts.DB, actorID, admin)
		if errors.Is(err, storage.ErrNotBlocked) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		opts.Bus.Publish(releasedEvent(actorID, admin.DisplayName))
		w.WriteHeader(http.StatusNoContent)
	}
}

// blockedEvent and releasedEvent put administrative state changes into the
// terminal stream itself, so a human watching sees why output stopped — or that
// it is expected to resume — instead of watching it simply go quiet.
func blockedEvent(actorID, byActor, reason string) stream.Event {
	return stream.Event{
		SandboxID: actorID,
		Kind:      stream.EventBlocked,
		At:        time.Now().UTC(),
		ByActor:   byActor,
		Reason:    reason,
	}
}

func releasedEvent(actorID, byActor string) stream.Event {
	return stream.Event{
		SandboxID: actorID,
		Kind:      stream.EventReleased,
		At:        time.Now().UTC(),
		ByActor:   byActor,
	}
}
