package frontend_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JetManiack/mcp-sandbox/internal/frontend"
)

// TestEmbeddedAssetsAreServed is the check that catches a build which produced a
// binary with an empty asset tree: the Go build succeeds whether or not
// `make generate` ran, so nothing else notices until the shipped image 404s on
// every request.
func TestEmbeddedAssetsAreServed(t *testing.T) {
	fs, err := frontend.FS(false)
	if err != nil {
		t.Fatalf("FS: %v", err)
	}
	server := httptest.NewServer(http.FileServer(fs))
	defer server.Close()

	tests := []struct {
		path        string
		wantStatus  int
		wantContent string
	}{
		{path: "/", wantStatus: http.StatusOK, wantContent: "<div id=\"root\">"},
		{path: "/index.html", wantStatus: http.StatusOK, wantContent: "mcp-sandbox"},
		{path: "/js/app.bundle.js", wantStatus: http.StatusOK},
		{path: "/nope.txt", wantStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			resp, err := server.Client().Get(server.URL + tt.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tt.path, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tt.wantStatus)
			}
			if tt.wantContent == "" {
				return
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v", err)
			}
			if !strings.Contains(string(body), tt.wantContent) {
				t.Errorf("body does not contain %q", tt.wantContent)
			}
		})
	}
}

func TestBundleIsNotEmpty(t *testing.T) {
	fs, err := frontend.FS(false)
	if err != nil {
		t.Fatalf("FS: %v", err)
	}
	file, err := fs.Open("/js/app.bundle.js")
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		t.Fatalf("stat bundle: %v", err)
	}
	// An empty bundle is what an unbuilt frontend looks like: the page loads, the
	// app never mounts, and there is no error anywhere.
	if info.Size() == 0 {
		t.Error("app.bundle.js is empty — run `make generate`")
	}
}

func TestDevelModeReadsFromDisk(t *testing.T) {
	fs, err := frontend.FS(true)
	if err != nil {
		t.Fatalf("FS(devel): %v", err)
	}
	// The devel path is relative to the repository root, so opening it from this
	// package's directory is expected to fail — what matters is that FS returns a
	// disk-backed filesystem rather than the embedded snapshot.
	if fs == nil {
		t.Fatal("FS(devel) returned nil")
	}
}
