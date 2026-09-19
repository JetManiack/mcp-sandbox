// Package workertest starts a real hub with real workers connected to it, for
// tests of anything above the worker link.
//
// It exists because the alternative — a fake executor — would pass while the wire
// contract was broken. Execution now crosses a process boundary, so the tests that
// used to run commands in-process are only worth as much as the link they run
// over.
package workertest

import (
	"context"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/JetManiack/mcp-sandbox/internal/sandbox"
	"github.com/JetManiack/mcp-sandbox/internal/stream"
	"github.com/JetManiack/mcp-sandbox/internal/workerhub"
	"github.com/JetManiack/mcp-sandbox/internal/workerlink"
)

// Token is the shared secret the harness's workers present.
const Token = "test-worker-token"

// Harness is a hub, the HTTP endpoint workers dial, and the workers themselves.
type Harness struct {
	Bus    *stream.Broadcaster
	Hub    *workerhub.Hub
	Server *httptest.Server

	// Roots are the sandbox roots of the started workers, in the order they were
	// started, for tests that need to look at what a command wrote.
	Roots []string
}

// Start brings up a hub and workers workers connected to it, and waits until they
// have all registered.
func Start(t *testing.T, workers int) *Harness {
	t.Helper()
	if workers < 1 {
		t.Fatalf("workers = %d, want at least 1", workers)
	}

	bus := stream.NewBroadcaster(0)
	hub := workerhub.New(bus)
	server := httptest.NewServer(hub.Handler(Token))
	t.Cleanup(server.Close)

	h := &Harness{Bus: bus, Hub: hub, Server: server}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	for i := range workers {
		root := t.TempDir()
		h.Roots = append(h.Roots, root)

		cfg := sandbox.DefaultConfig(root)
		link, err := workerlink.New(workerlink.Config{
			ServerURL: server.URL,
			Token:     Token,
			WorkerID:  workerID(i),
			Version:   "test",
		}, cfg)
		if err != nil {
			t.Fatalf("workerlink.New: %v", err)
		}
		go func() { _ = link.Run(ctx) }()
	}

	h.waitForWorkers(t, workers)
	return h
}

// StartOne is Start with a single worker, which is what most tests want.
func StartOne(t *testing.T) *Harness { return Start(t, 1) }

// workerID names the i-th worker. Numbered rather than lettered: 'a'+i stops
// being a letter after 26 and the harness takes a count, not a promise.
func workerID(i int) string {
	return "worker-" + strconv.Itoa(i)
}

// waitForWorkers blocks until want workers have registered, so a test never races
// the dial it depends on.
func (h *Harness) waitForWorkers(t *testing.T, want int) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.Hub.Workers()) >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only %d of %d workers connected", len(h.Hub.Workers()), want)
}
