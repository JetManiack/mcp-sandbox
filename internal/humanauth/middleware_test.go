package humanauth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/storage"
)

func openTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage.Open: %v", err)
	}
	return db
}

type fixedProvider struct {
	identity *Identity
	err      error
}

func (f fixedProvider) Authenticate(*http.Request) (*Identity, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.identity, nil
}

func TestRequireHumanAuthProvisionsActor(t *testing.T) {
	db := openTestDB(t)
	provider := fixedProvider{identity: &Identity{Subject: "sub-1", DisplayName: "Ada", Role: "admin"}}

	var gotActor *storage.Actor
	var gotRole string
	handler := RequireHumanAuth(db, provider)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotActor, _ = ActorFromContext(r.Context())
		gotRole, _ = RoleFromContext(r.Context())
	}))

	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/me", nil))

	if gotActor == nil {
		t.Fatal("no actor was injected into the request context")
	}
	if gotActor.DisplayName != "Ada" {
		t.Errorf("DisplayName = %q, want Ada", gotActor.DisplayName)
	}
	if gotRole != "admin" {
		t.Errorf("role = %q, want admin", gotRole)
	}

	// Provisioning is just-in-time, and must happen exactly once across
	// requests rather than on every one.
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/me", nil))
	var count int64
	if err := db.Model(&storage.Actor{}).Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("%d actors exist after two requests, want 1", count)
	}
}

func TestRequireHumanAuthRejectsUnauthenticated(t *testing.T) {
	db := openTestDB(t)
	provider := fixedProvider{err: errors.New("no session")}

	called := false
	handler := RequireHumanAuth(db, provider)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if called {
		t.Error("the wrapped handler ran despite failed authentication")
	}
}

func TestRequireAdmin(t *testing.T) {
	tests := []struct {
		name       string
		role       string
		wantStatus int
	}{
		{name: "admin", role: "admin", wantStatus: http.StatusOK},
		{name: "viewer", role: "viewer", wantStatus: http.StatusForbidden},
		{name: "unknown role", role: "auditor", wantStatus: http.StatusForbidden},
		{name: "empty role", role: "", wantStatus: http.StatusForbidden},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTestDB(t)
			provider := fixedProvider{identity: &Identity{Subject: "sub-" + tt.name, DisplayName: tt.name, Role: tt.role}}

			handler := RequireHumanAuth(db, provider)(RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			})))

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

// RequireAdmin must not be usable on its own: without RequireHumanAuth ahead of
// it there is no role in context, and "no role" has to mean denied.
func TestRequireAdminWithoutAuthContextDenies(t *testing.T) {
	rec := httptest.NewRecorder()
	RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("the wrapped handler ran without any auth context")
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/agents", nil))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestStubProviderIsAlwaysAdmin(t *testing.T) {
	identity, err := StubProvider{}.Authenticate(httptest.NewRequest(http.MethodGet, "/", nil))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if identity.Role != "admin" {
		t.Errorf("Role = %q, want admin", identity.Role)
	}
}
