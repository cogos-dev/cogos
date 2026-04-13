//go:build integration

// integration_test.go — end-to-end tests for the v3 kernel
//
// Run with: go test -tags integration -race -count=1 ./...
// Or via:   make test-integration
//
// These tests start real process and server goroutines and make HTTP calls.
// They are gated behind the "integration" build tag to keep `make test` fast.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestIntegrationFullLifecycle exercises the complete startup → event → shutdown path.
func TestIntegrationFullLifecycle(t *testing.T) {
	// Not parallel — integration tests own real goroutines.
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	cfg.ConsolidationInterval = 99999 // don't fire during test
	cfg.HeartbeatInterval = 99999

	nucleus := mustLoadNucleus(t, cfg)

	process := NewProcess(cfg, nucleus)
	server := NewServer(cfg, nucleus, process)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Start process.
	processDone := make(chan error, 1)
	go func() { processDone <- process.Run(ctx) }()

	// Start server via httptest (random port, no bind conflicts).
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	// Wait for process to enter its select loop (field update completes, start event emitted).
	// We poll /health rather than sleeping — once it returns 200 the server and process are live.
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(ts.URL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not become healthy within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}

	t.Run("health_check_200", func(t *testing.T) {
		resp := mustGET(t, ts.URL+"/health")
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d; want 200", resp.StatusCode)
		}

		var body map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode health: %v", err)
		}
		if body["status"] != "ok" {
			t.Errorf("status = %v; want ok", body["status"])
		}
		if body["identity"] != nucleus.Name {
			t.Errorf("identity = %v; want %q", body["identity"], nucleus.Name)
		}
	})

	t.Run("context_returns_json", func(t *testing.T) {
		resp := mustGET(t, ts.URL+"/v1/context")
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d; want 200", resp.StatusCode)
		}

		var body map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode context: %v", err)
		}
		if body["nucleus"] == nil {
			t.Error("nucleus field missing from /v1/context")
		}
		if body["fovea"] == nil {
			t.Error("fovea field missing from /v1/context")
		}
	})

	t.Run("chat_returns_501", func(t *testing.T) {
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
			nil)
		if err != nil {
			t.Fatalf("POST /v1/chat/completions: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("status = %d; want 501", resp.StatusCode)
		}
	})

	t.Run("external_event_transitions_state", func(t *testing.T) {
		// Push an event and wait for the Active transition.
		process.Send(&GateEvent{
			Type:      "user.message",
			Content:   "integration test message",
			Timestamp: time.Now(),
		})
		waitForState(t, process, StateActive, 2*time.Second)
	})

	t.Run("ledger_records_start_event", func(t *testing.T) {
		// Poll for the ledger file (written during process.Run startup).
		var events []EventEnvelope
		deadline := time.Now().Add(2 * time.Second)
		for {
			events = mustReadAllEvents(t, root, process.SessionID())
			if len(events) > 0 {
				break
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if len(events) == 0 {
			t.Fatal("no events in ledger")
		}
		if events[0].HashedPayload.Type != "process.start" {
			t.Errorf("first event = %q; want process.start", events[0].HashedPayload.Type)
		}
		// Each event must have a non-empty hash.
		for i, ev := range events {
			if ev.Metadata.Hash == "" {
				t.Errorf("event[%d] has empty hash", i)
			}
		}
	})

	t.Run("graceful_shutdown", func(t *testing.T) {
		cancel() // signal shutdown

		select {
		case err := <-processDone:
			if err != nil {
				t.Errorf("process.Run returned error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("process did not stop within 5s after cancel")
		}
	})
}

// TestIntegrationConcurrentRequests verifies the server handles concurrent
// requests without data races.
func TestIntegrationConcurrentRequests(t *testing.T) {
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	nucleus := makeNucleus("Test", "tester")
	process := NewProcess(cfg, nucleus)
	server := NewServer(cfg, nucleus, process)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go process.Run(ctx) //nolint:errcheck

	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	const requests = 20
	errs := make(chan error, requests)

	for i := range requests {
		go func(n int) {
			var url string
			if n%2 == 0 {
				url = ts.URL + "/health"
			} else {
				url = ts.URL + "/v1/context"
			}
			resp, err := http.Get(url) //nolint:noctx
			if err != nil {
				errs <- fmt.Errorf("request %d: %w", n, err)
				return
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("request %d: status %d", n, resp.StatusCode)
				return
			}
			errs <- nil
		}(i)
	}

	for range requests {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

// waitForState polls process.State() until it equals want or the deadline expires.
// Uses a channel-based ticker (no raw sleep) to avoid flakiness.
func waitForState(t *testing.T, p *Process, want ProcessState, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			t.Fatalf("process state: got %s after %s; want %s", p.State(), timeout, want)
		case <-tick.C:
			if p.State() == want {
				return
			}
		}
	}
}

// mustGET performs an HTTP GET and fatals on error.
func mustGET(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}
