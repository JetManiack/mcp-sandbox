package restapi_test

import (
	"net/http"
	"testing"

	"github.com/JetManiack/mcp-sandbox/internal/storage"
)

type sandboxRow struct {
	ActorID         string `json:"actor_id"`
	DisplayName     string `json:"display_name"`
	Live            bool   `json:"live"`
	RunningCommands int    `json:"running_commands"`
	Block           *struct {
		Reason        string `json:"reason"`
		BlockedByName string `json:"blocked_by_name"`
	} `json:"block"`
}

// TestViewerCannotBlockOrRelease is the gating that matters most here: watching a
// terminal is read-only, stopping a sandbox is not.
func TestViewerCannotBlockOrRelease(t *testing.T) {
	api := newTestAPI(t, viewerProvider())
	agent := api.mustAgent(t, "agent-1")

	resp := api.do(t, http.MethodPost, "/sandboxes/"+agent.ID+"/block", map[string]string{"reason": "because"})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("POST block as viewer: status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}

	resp = api.do(t, http.MethodDelete, "/sandboxes/"+agent.ID+"/block", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("DELETE block as viewer: status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}

	// The block must not have happened, not merely have been reported as
	// refused.
	block, err := storage.ActiveSandboxBlock(api.db, agent.ID)
	if err != nil {
		t.Fatalf("ActiveSandboxBlock: %v", err)
	}
	if block != nil {
		t.Error("a viewer's refused request still created a block")
	}
}

func TestViewerCanReadSandboxes(t *testing.T) {
	api := newTestAPI(t, viewerProvider())
	api.mustAgent(t, "agent-1")

	resp := api.do(t, http.MethodGet, "/sandboxes", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET sandboxes as viewer: status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	rows := decodeBody[[]sandboxRow](t, resp)
	if len(rows) != 1 {
		t.Errorf("rows = %d, want 1", len(rows))
	}
}

func TestViewerCannotManageAgents(t *testing.T) {
	api := newTestAPI(t, viewerProvider())
	agent := api.mustAgent(t, "agent-1")

	for _, tc := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/agents", nil},
		{http.MethodPost, "/agents", map[string]string{"display_name": "new"}},
		{http.MethodPost, "/agents/" + agent.ID + "/tokens", nil},
		{http.MethodDelete, "/agents/" + agent.ID, nil},
	} {
		resp := api.do(t, tc.method, tc.path, tc.body)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s as viewer: status = %d, want %d", tc.method, tc.path, resp.StatusCode, http.StatusForbidden)
		}
	}
}

func TestAdminBlockAndReleaseCycle(t *testing.T) {
	api := newTestAPI(t, adminProvider())
	agent := api.mustAgent(t, "agent-1")

	resp := api.do(t, http.MethodPost, "/sandboxes/"+agent.ID+"/block", map[string]string{"reason": "runaway loop"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST block: status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}

	stored, err := storage.ActiveSandboxBlock(api.db, agent.ID)
	if err != nil {
		t.Fatalf("ActiveSandboxBlock: %v", err)
	}
	if stored == nil {
		t.Fatal("no active block after a successful POST")
	}
	if stored.BlockedByName != "Grace" || stored.Reason != "runaway loop" {
		t.Errorf("block = %+v, want it to record who and why", stored)
	}

	// The list has to surface the block, since that is where an operator looks.
	listed := decodeBody[[]sandboxRow](t, api.do(t, http.MethodGet, "/sandboxes", nil))
	if len(listed) != 1 || listed[0].Block == nil {
		t.Fatalf("listed = %+v, want the block reported", listed)
	}
	if listed[0].Block.Reason != "runaway loop" {
		t.Errorf("listed reason = %q, want %q", listed[0].Block.Reason, "runaway loop")
	}

	resp = api.do(t, http.MethodDelete, "/sandboxes/"+agent.ID+"/block", nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE block: status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}

	released, err := storage.ActiveSandboxBlock(api.db, agent.ID)
	if err != nil {
		t.Fatalf("ActiveSandboxBlock after release: %v", err)
	}
	if released != nil {
		t.Error("block still active after release")
	}
}

func TestBlockRequiresAReason(t *testing.T) {
	api := newTestAPI(t, adminProvider())
	agent := api.mustAgent(t, "agent-1")

	for _, body := range []map[string]string{{}, {"reason": "   "}} {
		resp := api.do(t, http.MethodPost, "/sandboxes/"+agent.ID+"/block", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("POST block with body %v: status = %d, want %d", body, resp.StatusCode, http.StatusBadRequest)
		}
	}
}

func TestDoubleBlockConflicts(t *testing.T) {
	api := newTestAPI(t, adminProvider())
	agent := api.mustAgent(t, "agent-1")

	if resp := api.do(t, http.MethodPost, "/sandboxes/"+agent.ID+"/block", map[string]string{"reason": "first"}); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first block: status = %d", resp.StatusCode)
	}
	resp := api.do(t, http.MethodPost, "/sandboxes/"+agent.ID+"/block", map[string]string{"reason": "second"})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("second block: status = %d, want %d", resp.StatusCode, http.StatusConflict)
	}
}

func TestReleasingAnUnblockedSandbox(t *testing.T) {
	api := newTestAPI(t, adminProvider())
	agent := api.mustAgent(t, "agent-1")

	resp := api.do(t, http.MethodDelete, "/sandboxes/"+agent.ID+"/block", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestBlockingAnUnknownAgent(t *testing.T) {
	api := newTestAPI(t, adminProvider())

	resp := api.do(t, http.MethodPost, "/sandboxes/00000000-0000-0000-0000-000000000000/block", map[string]string{"reason": "why"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestMeReportsRole(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider roleProvider
		wantRole string
	}{
		{"viewer", viewerProvider(), "viewer"},
		{"admin", adminProvider(), "admin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newTestAPI(t, tc.provider)
			resp := api.do(t, http.MethodGet, "/me", nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
			}
			me := decodeBody[struct {
				DisplayName string `json:"display_name"`
				Role        string `json:"role"`
			}](t, resp)
			// The UI decides whether to render the stop controls from this, so a
			// wrong role here is a wrong interface.
			if me.Role != tc.wantRole {
				t.Errorf("role = %q, want %q", me.Role, tc.wantRole)
			}
		})
	}
}
