package restapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/storage"
)

type createAgentRequest struct {
	DisplayName string `json:"display_name"`
}

type issueTokenResponse struct {
	Token      string                   `json:"token"`
	Credential *storage.AgentCredential `json:"credential"`
}

// agentResponse embeds storage.Actor and adds HasActiveToken so clients can tell
// an agent with no usable credential from one that can still authenticate —
// DELETE /agents/{id} revokes credentials rather than removing the Actor row, so
// decommissioned agents remain in this list and the blocks recorded against them
// keep a named owner.
type agentResponse struct {
	storage.Actor
	HasActiveToken bool `json:"has_active_token"`
}

func listAgentsHandler(db *gorm.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		agents, err := storage.ListAgents(db)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		active, err := storage.ActorsWithActiveToken(db)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		resp := make([]agentResponse, 0, len(agents))
		for _, agent := range agents {
			resp = append(resp, agentResponse{Actor: agent, HasActiveToken: active[agent.ID]})
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func createAgentHandler(db *gorm.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createAgentRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errors.New("body must be a JSON object with display_name"))
			return
		}

		agent, err := storage.CreateAgent(db, req.DisplayName)
		switch {
		case errors.Is(err, storage.ErrEmptyDisplayName), errors.Is(err, storage.ErrDisplayNameConflict):
			writeError(w, http.StatusBadRequest, err)
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, agent)
	}
}

// deleteAgentHandler revokes every credential belonging to the agent rather than
// deleting its Actor row: the agent can no longer authenticate, and its sandbox
// and any block against it keep a named owner instead of pointing at a vanished
// actor.
func deleteAgentHandler(db *gorm.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := storage.RevokeAllAgentCredentials(db, chi.URLParam(r, "id")); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func listAgentTokensHandler(db *gorm.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		creds, err := storage.ListAgentCredentials(db, chi.URLParam(r, "id"))
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if creds == nil {
			creds = []storage.AgentCredential{}
		}
		writeJSON(w, http.StatusOK, creds)
	}
}

// issueTokenHandler returns the raw token, which is the only time it is ever
// visible: only its hash is stored.
func issueTokenHandler(db *gorm.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, cred, err := storage.IssueAgentToken(db, chi.URLParam(r, "id"))
		if errors.Is(err, storage.ErrActorNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusCreated, issueTokenResponse{Token: token, Credential: cred})
	}
}

func revokeTokenHandler(db *gorm.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := storage.RevokeAgentToken(db, chi.URLParam(r, "id"))
		if errors.Is(err, storage.ErrCredentialNotFound) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
