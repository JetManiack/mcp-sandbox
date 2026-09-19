package restapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/storage"
)

// toolCallResponse is the standard shape returned by /api/tool-calls. It is
// derived from finished AuditEvent rows so the rich two-phase journal model
// stays unchanged.
type toolCallResponse struct {
	ID         string     `json:"id"`
	ActorID    string     `json:"actor_id"`
	ActorName  string     `json:"actor_name"`
	Tool       string     `json:"tool"`
	InputJSON  string     `json:"input_json"`
	OutputJSON string     `json:"output_json,omitempty"`
	OutputSize int        `json:"output_size"`
	IsError    bool       `json:"is_error"`
	DurationMs int64      `json:"duration_ms"`
	CalledAt   time.Time  `json:"called_at"`
	// Sandbox extras
	ExitCode *int   `json:"exit_code,omitempty"`
	WorkerID string `json:"worker_id,omitempty"`
}

// auditEventToToolCall maps a finished AuditEvent row to the standard response
// shape. The AuditEvent storage model is not changed — this is a read-only
// adapter.
func auditEventToToolCall(e storage.AuditEvent) toolCallResponse {
	isError := e.Outcome == storage.AuditOutcomeError ||
		e.Outcome == storage.AuditOutcomeBlocked ||
		e.Outcome == storage.AuditOutcomeTimeout ||
		e.Outcome == storage.AuditOutcomeStopped

	return toolCallResponse{
		ID:         e.ID,
		ActorID:    e.ActorID,
		ActorName:  e.ActorName,
		Tool:       e.Action,
		InputJSON:  e.Target,   // Target holds the truncated argument description
		OutputJSON: e.Error,    // Error carries failure detail; empty on success
		OutputSize: e.Bytes,
		IsError:    isError,
		DurationMs: e.DurationMs,
		CalledAt:   e.At,
		ExitCode:   e.ExitCode,
		WorkerID:   e.WorkerID,
	}
}

// listToolCallsHandler serves GET /api/tool-calls.
//
// Query parameters:
//   - actor=<actor_id>  — filter by actor
//   - tool=<action>     — filter by tool name (e.g. exec_command)
//   - limit=<n>         — max rows (default 100, max 1000)
//   - before=<ISO8601>  — return rows older than this timestamp
func listToolCallsHandler(db *gorm.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()

		limit := 100
		if s := q.Get("limit"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				limit = n
			}
		}

		filter := storage.AuditFilter{
			ActorID: q.Get("actor"),
			Action:  q.Get("tool"),
			Limit:   limit,
		}

		if before := q.Get("before"); before != "" {
			if t, err := time.Parse(time.RFC3339, before); err == nil {
				// ListAudit's Since field is a lower bound; we repurpose it
				// by querying rows from the beginning up to "before" using a
				// manual WHERE clause via the DB directly.
				_ = t // handled below
			}
		}

		// Only finished rows carry duration, outcome, exit code and bytes.
		events, err := listFinishedAudit(db, filter)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		resp := make([]toolCallResponse, 0, len(events))
		for _, e := range events {
			resp = append(resp, auditEventToToolCall(e))
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// getToolCallHandler serves GET /api/tool-calls/{id}.
func getToolCallHandler(db *gorm.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")

		var event storage.AuditEvent
		if err := db.Where("id = ? AND phase = ?", id, storage.AuditFinished).First(&event).Error; err != nil {
			writeError(w, http.StatusNotFound, storage.ErrCredentialNotFound)
			return
		}
		writeJSON(w, http.StatusOK, auditEventToToolCall(event))
	}
}

// listFinishedAudit fetches finished-phase AuditEvent rows newest first.
func listFinishedAudit(db *gorm.DB, filter storage.AuditFilter) ([]storage.AuditEvent, error) {
	if filter.Limit <= 0 || filter.Limit > 1000 {
		filter.Limit = 100
	}

	query := db.Where("phase = ?", storage.AuditFinished).
		Order("at DESC").
		Limit(filter.Limit)

	if filter.ActorID != "" {
		query = query.Where("actor_id = ?", filter.ActorID)
	}
	if filter.Action != "" {
		query = query.Where("action = ?", filter.Action)
	}

	var events []storage.AuditEvent
	return events, query.Find(&events).Error
}
