package health_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/JetManiack/mcp-sandbox/internal/health"
)

func TestLivezAlwaysOK(t *testing.T) {
	rec := httptest.NewRecorder()
	health.Livez(rec, httptest.NewRequest(http.MethodGet, "/livez", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("Livez status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestReadyz(t *testing.T) {
	okPing := func(context.Context) error { return nil }

	tests := []struct {
		name        string
		checker     health.ReadyChecker
		wantStatus  int
		wantFailing []string
	}{
		{
			name:       "all healthy",
			checker:    health.ReadyChecker{Ping: okPing, MigrationsReady: true, OIDCReady: true},
			wantStatus: http.StatusOK,
		},
		{
			name: "database unreachable",
			checker: health.ReadyChecker{
				Ping:            func(context.Context) error { return errors.New("connection refused") },
				MigrationsReady: true,
				OIDCReady:       true,
			},
			wantStatus:  http.StatusServiceUnavailable,
			wantFailing: []string{"database"},
		},
		{
			// The degraded startup path builds a checker before it has any
			// database handle to ping with. A nil Ping must read as "not
			// ready", not panic the readiness probe.
			name:        "nil ping counts as a database failure",
			checker:     health.ReadyChecker{MigrationsReady: true, OIDCReady: true},
			wantStatus:  http.StatusServiceUnavailable,
			wantFailing: []string{"database"},
		},
		{
			name:        "everything failing is named individually",
			checker:     health.ReadyChecker{Ping: func(context.Context) error { return errors.New("down") }},
			wantStatus:  http.StatusServiceUnavailable,
			wantFailing: []string{"migrations", "oidc_discovery", "database"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tt.checker.Readyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			var body struct {
				Status  string   `json:"status"`
				Failing []string `json:"failing"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body %q: %v", rec.Body.String(), err)
			}

			for _, want := range tt.wantFailing {
				if !slices.Contains(body.Failing, want) {
					t.Errorf("failing = %v, want it to contain %q", body.Failing, want)
				}
			}
			if len(tt.wantFailing) == 0 && body.Status != "ready" {
				t.Errorf("status = %q, want %q", body.Status, "ready")
			}
		})
	}
}
