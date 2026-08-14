// serve_queue_test.go — tests for GET /v1/queue (#556): the live snapshot of
// every kernel-owned local-backend FIFO queue.
package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHandleQueue_EmptyRegistryReturnsEmptyValidSnapshot(t *testing.T) {
	resetBackendQueuesForTest()
	t.Cleanup(resetBackendQueuesForTest)

	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/v1/queue", nil)
	rec := httptest.NewRecorder()
	s.handleQueue(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for an empty backend-queue registry, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp queueSnapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Backends == nil {
		t.Error("expected an empty (not nil-in-JSON) backends array")
	}
	if len(resp.Backends) != 0 {
		t.Errorf("expected 0 backends, got %d", len(resp.Backends))
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal raw response: %v", err)
	}
	if _, ok := raw["backends"]; !ok {
		t.Error(`expected "backends" key present in JSON even when empty`)
	}
}

// TestHandleQueue_ReportsConcurrencyInFlightAndWaitingCallers exercises the
// full documented shape: name, concurrency, in_flight, waiting,
// oldest_wait_ms, and the per-caller list (position, waiting_ms) — using a
// real backendQueue registered under a known name so the handler is reading
// actual registry state, not a stub. Also locks in the security fix (repair
// round 1, #556): a waiter's attribution (bound-identity subject) must NEVER
// appear in this response, because GET /v1/queue is grant-exempt like every
// other GET (isGrantExemptRequest) and therefore reachable with no auth.
func TestHandleQueue_ReportsConcurrencyInFlightAndWaitingCallers(t *testing.T) {
	resetBackendQueuesForTest()
	t.Cleanup(resetBackendQueuesForTest)

	q := newBackendQueue("lms-test-backend", 1)
	backendQueues.Store("lms-test-backend", q)

	// Hold the one slot, then queue a second, attributed caller behind it.
	release0, _, _, err := q.Acquire(t.Context(), "seed")
	if err != nil {
		t.Fatalf("seed acquire: %v", err)
	}
	t.Cleanup(release0)

	waiterDone := make(chan struct{})
	go func() {
		release, _, _, err := q.Acquire(t.Context(), "chazmaniandinkle")
		if err != nil {
			return
		}
		<-waiterDone
		release()
	}()
	pollUntilQueue(t, 2*time.Second, "waiter to enqueue", func() bool {
		return q.Snapshot().Waiting == 1
	})
	defer close(waiterDone)

	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/v1/queue", nil)
	rec := httptest.NewRecorder()
	s.handleQueue(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	var resp queueSnapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Backends) != 1 {
		t.Fatalf("expected exactly 1 backend in the snapshot, got %d", len(resp.Backends))
	}

	b := resp.Backends[0]
	if b.Name != "lms-test-backend" {
		t.Errorf("Name = %q, want lms-test-backend", b.Name)
	}
	if b.Concurrency != 1 {
		t.Errorf("Concurrency = %d, want 1", b.Concurrency)
	}
	if b.InFlight != 1 {
		t.Errorf("InFlight = %d, want 1 (the held seed slot)", b.InFlight)
	}
	if b.Waiting != 1 {
		t.Errorf("Waiting = %d, want 1", b.Waiting)
	}
	if len(b.Callers) != 1 {
		t.Fatalf("expected exactly 1 waiting caller, got %d", len(b.Callers))
	}
	if b.Callers[0].Position != 1 {
		t.Errorf("Callers[0].Position = %d, want 1", b.Callers[0].Position)
	}

	// Security fix verification: the caller was registered with a real,
	// identifiable attribution ("chazmaniandinkle") but the raw JSON must not
	// contain it anywhere in the response — GET /v1/queue has no auth gate.
	if strings.Contains(rec.Body.String(), "chazmaniandinkle") {
		t.Error("GET /v1/queue response leaked caller attribution; this endpoint is grant-exempt and unauthenticated")
	}
	if strings.Contains(rec.Body.String(), "attribution") {
		t.Error(`GET /v1/queue response contains an "attribution" key; identity must not be exposed on this unauthenticated surface`)
	}
}

// TestHandleQueue_MultipleBackendsSortedByName verifies allBackendQueueSnapshots'
// deterministic-ordering contract survives the HTTP layer.
func TestHandleQueue_MultipleBackendsSortedByName(t *testing.T) {
	resetBackendQueuesForTest()
	t.Cleanup(resetBackendQueuesForTest)

	backendQueues.Store("zeta-backend", newBackendQueue("zeta-backend", 2))
	backendQueues.Store("alpha-backend", newBackendQueue("alpha-backend", 3))
	backendQueues.Store("mid-backend", newBackendQueue("mid-backend", 1))

	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/v1/queue", nil)
	rec := httptest.NewRecorder()
	s.handleQueue(rec, req)

	var resp queueSnapshotResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if len(resp.Backends) != 3 {
		t.Fatalf("expected 3 backends, got %d", len(resp.Backends))
	}
	want := []string{"alpha-backend", "mid-backend", "zeta-backend"}
	for i, name := range want {
		if resp.Backends[i].Name != name {
			t.Errorf("Backends[%d].Name = %q, want %q (deterministic name order)", i, resp.Backends[i].Name, name)
		}
	}
}
