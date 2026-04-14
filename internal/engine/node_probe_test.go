package engine

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestNodeProbe_EmptyManifest(t *testing.T) {
	t.Parallel()
	nh := NewNodeHealth()
	manifest := &NodeManifest{Services: map[string]ServiceDef{}}

	nh.Probe(manifest, 6931)

	snap := nh.Snapshot()
	if len(snap) != 0 {
		t.Errorf("Snapshot length = %d; want 0 for empty manifest", len(snap))
	}

	healthy, total := nh.Counts()
	if healthy != 0 || total != 0 {
		t.Errorf("Counts() = (%d, %d); want (0, 0)", healthy, total)
	}
}

func TestNodeProbe_SkipsSelfPort(t *testing.T) {
	t.Parallel()

	// Start a healthy test server.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer ts.Close()

	selfPort := portFromURL(t, ts.URL)

	manifest := &NodeManifest{
		Services: map[string]ServiceDef{
			"self-service": {Port: selfPort, Health: "/health"},
		},
	}

	nh := NewNodeHealth()
	nh.Probe(manifest, selfPort)

	snap := nh.Snapshot()
	if len(snap) != 0 {
		t.Errorf("Snapshot should be empty when only service is self; got %d entries: %v", len(snap), snap)
	}
}

func TestNodeProbe_UnreachableServiceMarkedDown(t *testing.T) {
	t.Parallel()

	// Start and immediately close a server to get a port that's not listening.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedPort := portFromURL(t, ts.URL)
	ts.Close()

	// Start a healthy server.
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer healthy.Close()
	healthyPort := portFromURL(t, healthy.URL)

	manifest := &NodeManifest{
		Services: map[string]ServiceDef{
			"alive": {Port: healthyPort, Health: "/health"},
			"dead":  {Port: closedPort, Health: "/health"},
		},
	}

	nh := NewNodeHealth()
	nh.Probe(manifest, 0) // selfPort=0 means skip nothing

	snap := nh.Snapshot()

	if s, ok := snap["dead"]; !ok {
		t.Fatal("expected 'dead' service in snapshot")
	} else if s.Status != "down" {
		t.Errorf("dead service status = %q; want \"down\"", s.Status)
	}

	if s, ok := snap["alive"]; !ok {
		t.Fatal("expected 'alive' service in snapshot")
	} else if s.Status != "healthy" {
		t.Errorf("alive service status = %q; want \"healthy\"", s.Status)
	}
}

func TestNodeProbe_CountsReturnsCorrectHealthyTotal(t *testing.T) {
	t.Parallel()

	// Closed port for "down" service.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	closedPort := portFromURL(t, ts.URL)
	ts.Close()

	// Two healthy servers.
	h1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer h1.Close()

	h2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer h2.Close()

	manifest := &NodeManifest{
		Services: map[string]ServiceDef{
			"svc-a": {Port: portFromURL(t, h1.URL), Health: "/health"},
			"svc-b": {Port: portFromURL(t, h2.URL), Health: "/health"},
			"svc-c": {Port: closedPort, Health: "/health"},
		},
	}

	nh := NewNodeHealth()
	nh.Probe(manifest, 0)

	healthy, total := nh.Counts()
	if healthy != 2 {
		t.Errorf("healthy = %d; want 2", healthy)
	}
	if total != 3 {
		t.Errorf("total = %d; want 3", total)
	}
}

// portFromURL extracts the port number from an httptest server URL.
func portFromURL(t *testing.T, rawURL string) int {
	t.Helper()
	// URL is like "http://127.0.0.1:PORT"
	parts := strings.Split(rawURL, ":")
	if len(parts) < 3 {
		t.Fatalf("unexpected URL format: %s", rawURL)
	}
	port, err := strconv.Atoi(parts[len(parts)-1])
	if err != nil {
		t.Fatalf("parse port from %q: %v", rawURL, err)
	}
	return port
}
