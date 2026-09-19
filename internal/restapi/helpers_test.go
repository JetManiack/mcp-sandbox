package restapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/humanauth"
	"github.com/JetManiack/mcp-sandbox/internal/restapi"
	"github.com/JetManiack/mcp-sandbox/internal/storage"
	"github.com/JetManiack/mcp-sandbox/internal/workertest"
)

// roleProvider authenticates every request as a fixed identity with a chosen
// role, so role gating can be exercised without an identity provider.
type roleProvider struct {
	subject string
	name    string
	role    string
}

func (p roleProvider) Authenticate(*http.Request) (*humanauth.Identity, error) {
	return &humanauth.Identity{Subject: p.subject, DisplayName: p.name, Role: p.role}, nil
}

func viewerProvider() roleProvider {
	return roleProvider{subject: "viewer-subject", name: "Viewer", role: humanauth.RoleViewer}
}

func adminProvider() roleProvider {
	return roleProvider{subject: "admin-subject", name: "Grace", role: humanauth.RoleAdmin}
}

type testAPI struct {
	server *httptest.Server
	db     *gorm.DB
	worker *workertest.Harness
}

// newTestAPI boots the real router over HTTP, so middleware, role gating and the
// WebSocket upgrade are all exercised on the path a browser takes.
func newTestAPI(t *testing.T, provider humanauth.Provider) *testAPI {
	t.Helper()

	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}

	// A real hub with a real worker behind it: the API's stop button and its
	// sandbox list both go over the worker link now, and a fake would not exercise
	// it.
	worker := workertest.StartOne(t)

	server := httptest.NewServer(restapi.NewRouter(restapi.Options{
		DB:           db,
		Bus:          worker.Bus,
		Hub:          worker.Hub,
		AuthProvider: provider,
	}))
	t.Cleanup(server.Close)

	return &testAPI{server: server, db: db, worker: worker}
}

func (api *testAPI) do(t *testing.T, method, path string, body any) *http.Response {
	t.Helper()

	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequest(method, api.server.URL+path, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := api.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decodeBody[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

func (api *testAPI) mustAgent(t *testing.T, name string) *storage.Actor {
	t.Helper()
	agent, err := storage.CreateAgent(api.db, name)
	if err != nil {
		t.Fatalf("CreateAgent(%q): %v", name, err)
	}
	return agent
}
