// mcp_kernel_status_test.go — unit tests for the cog://kernel/status resource.
//
// Coverage:
//   - resourceKernelStatus when kernel is reachable (happy path)
//   - resourceKernelStatus when kernel is unreachable (connection refused)
//   - Cache: a second call within TTL does NOT fire another probe
//   - Cache: a call after TTL expiry fires a fresh probe
//   - JSON shape: required fields present in both reachable / unreachable cases
//   - kernelStripURLFromError: verifies URL stripping from Go net error strings
//   - Round-trip: cog://kernel/status appears in ListResources over in-memory transport
package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newMCPForTestWithProber builds an MCPServer with a pre-configured prober.
// The prober's httpClient is set to ts.Client() so probes go to the fake server.
func newMCPForTestWithFakeHealth(t *testing.T, ts *httptest.Server) (*MCPServer, *Config) {
	t.Helper()
	root := makeWorkspace(t)
	port := testExtractPort(ts.URL)
	cfg := makeConfig(t, root)
	cfg.Port = port
	nucleus := makeNucleus("Cog", "tester")
	process := NewProcess(cfg, nucleus)
	srv := NewMCPServer(cfg, nucleus, process)
	// Wire the fake server's transport client so probes avoid the real network.
	srv.kernelProber.httpClient = ts.Client()
	return srv, cfg
}

// fakeHealthServer starts a fake httptest.Server returning a minimal /health JSON.
func fakeHealthServer(t *testing.T, version, identity, nodeID string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"version":  version,
			"identity": identity,
			"node_id":  nodeID,
		})
	}))
	return ts
}

// TestResourceKernelStatus_Reachable verifies the happy-path JSON shape.
func TestResourceKernelStatus_Reachable(t *testing.T) {
	t.Parallel()

	ts := fakeHealthServer(t, "0.13.0", "cog", "darkstar-1")
	defer ts.Close()

	srv, _ := newMCPForTestWithFakeHealth(t, ts)

	fakeReq := &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{URI: "cog://kernel/status"}}
	result, err := srv.resourceKernelStatus(context.Background(), fakeReq)
	if err != nil {
		t.Fatalf("resourceKernelStatus: %v", err)
	}

	if len(result.Contents) != 1 {
		t.Fatalf("len(Contents) = %d; want 1", len(result.Contents))
	}
	text := result.Contents[0].Text

	var got map[string]any
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("unmarshal response: %v\nraw: %s", err, text)
	}

	// Required fields when reachable.
	for _, key := range []string{"reachable", "endpoint", "version", "identity", "node_id", "checked_at", "latency_ms"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing field %q in response: %s", key, text)
		}
	}
	if got["reachable"] != true {
		t.Errorf("reachable = %v; want true", got["reachable"])
	}
	if got["version"] != "0.13.0" {
		t.Errorf("version = %v; want 0.13.0", got["version"])
	}
	if got["identity"] != "cog" {
		t.Errorf("identity = %v; want cog", got["identity"])
	}
	if got["node_id"] != "darkstar-1" {
		t.Errorf("node_id = %v; want darkstar-1", got["node_id"])
	}
}

// TestResourceKernelStatus_Unreachable verifies the error-path JSON shape.
func TestResourceKernelStatus_Unreachable(t *testing.T) {
	t.Parallel()

	root := makeWorkspace(t)
	// Port 19731 — no listener.
	cfg := &Config{WorkspaceRoot: root, Port: 19731}
	nucleus := makeNucleus("cog", "tester")
	process := NewProcess(cfg, nucleus)
	srv := NewMCPServer(cfg, nucleus, process)
	// Very short timeout so the test is fast.
	srv.kernelProber.httpClient = &http.Client{Timeout: 100 * time.Millisecond}

	fakeReq := &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{URI: "cog://kernel/status"}}
	result, err := srv.resourceKernelStatus(context.Background(), fakeReq)
	if err != nil {
		t.Fatalf("resourceKernelStatus returned error: %v (want graceful JSON)", err)
	}
	if len(result.Contents) == 0 {
		t.Fatal("Contents is empty")
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(result.Contents[0].Text), &got); err != nil {
		t.Fatalf("unmarshal: %v\nraw: %s", err, result.Contents[0].Text)
	}

	// Required fields when unreachable.
	for _, key := range []string{"reachable", "endpoint", "checked_at", "error"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing field %q in unreachable response: %s", key, result.Contents[0].Text)
		}
	}
	if got["reachable"] != false {
		t.Errorf("reachable = %v; want false", got["reachable"])
	}
	// "version" must NOT be present when unreachable.
	if _, ok := got["version"]; ok {
		t.Errorf("unexpected field version in unreachable response")
	}
}

// TestKernelStatusProber_Cache verifies that a second call within TTL does
// not fire a second HTTP probe.
func TestKernelStatusProber_Cache(t *testing.T) {
	t.Parallel()

	var probeCount atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCount.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "version": "1.0.0", "identity": "cog", "node_id": "x",
		})
	}))
	defer ts.Close()

	prober := &kernelStatusProber{httpClient: ts.Client()}
	endpoint := ts.URL

	// First call — should probe.
	if _, err := prober.probe(endpoint); err != nil {
		t.Fatalf("first probe: %v", err)
	}
	// Second call within TTL — should NOT probe.
	if _, err := prober.probe(endpoint); err != nil {
		t.Fatalf("second probe: %v", err)
	}

	if n := probeCount.Load(); n != 1 {
		t.Errorf("probe fired %d times; want exactly 1 (cache miss then hit)", n)
	}
}

// TestKernelStatusProber_CacheExpiry verifies that an expired cache entry is
// replaced by a fresh probe.
func TestKernelStatusProber_CacheExpiry(t *testing.T) {
	t.Parallel()

	var probeCount atomic.Int64
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeCount.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "version": "2.0.0", "identity": "cog2", "node_id": "y",
		})
	}))
	defer ts.Close()

	// Seed the prober with an already-expired entry.
	prober := &kernelStatusProber{
		httpClient: ts.Client(),
		entry: &kernelStatusEntry{
			payload:   []byte(`{"reachable":false,"error":"stale"}`),
			expiresAt: time.Now().Add(-1 * time.Second),
		},
	}

	payload, err := prober.probe(ts.URL)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Must be the fresh probe's data, not the stale entry.
	if got["version"] != "2.0.0" {
		t.Errorf("version = %v; want 2.0.0 (stale cache was not refreshed)", got["version"])
	}
	if n := probeCount.Load(); n != 1 {
		t.Errorf("probe fired %d times; want 1", n)
	}
}

// TestKernelStripURLFromError checks URL-stripping of Go net/http error strings.
func TestKernelStripURLFromError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		input string
		want  string
	}{
		{
			input: `Get "http://localhost:6931/health": dial tcp 127.0.0.1:6931: connect: connection refused`,
			want:  `dial tcp 127.0.0.1:6931: connect: connection refused`,
		},
		{
			input: `connection refused`,
			want:  `connection refused`,
		},
		{
			input: `Get "http://localhost:6931/health": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`,
			want:  `context deadline exceeded (Client.Timeout exceeded while awaiting headers)`,
		},
		{
			input: ``,
			want:  ``,
		},
	}
	for _, c := range cases {
		got := kernelStripURLFromError(c.input)
		if got != c.want {
			t.Errorf("kernelStripURLFromError(%q) = %q; want %q", c.input, got, c.want)
		}
	}
}

// TestResourceKernelStatus_ConcurrentSafe verifies that many goroutines
// calling resourceKernelStatus concurrently do not panic or data-race.
// Run with: go test -race ./internal/engine/...
func TestResourceKernelStatus_ConcurrentSafe(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "version": "1.0.0", "identity": "cog", "node_id": "z",
		})
	}))
	defer ts.Close()

	srv, _ := newMCPForTestWithFakeHealth(t, ts)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{URI: "cog://kernel/status"}}
			_, _ = srv.resourceKernelStatus(context.Background(), req)
		}()
	}
	wg.Wait()
}

// TestMCPServer_KernelStatusInListResources checks that the resource is
// advertised in the server's resource list over the in-memory transport.
func TestMCPServer_KernelStatusInListResources(t *testing.T) {
	t.Parallel()

	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	nucleus := makeNucleus("Cog", "tester")
	process := NewProcess(cfg, nucleus)
	server := NewMCPServer(cfg, nucleus, process)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = runServerOnTransport(ctx, server, serverTransport)
	}()
	defer func() {
		cancel()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("server goroutine did not exit in 5 s")
		}
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	defer session.Close()

	resources, err := session.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}

	var found bool
	for _, r := range resources.Resources {
		if r.URI == "cog://kernel/status" {
			found = true
			break
		}
	}
	if !found {
		uris := make([]string, 0, len(resources.Resources))
		for _, r := range resources.Resources {
			uris = append(uris, r.URI)
		}
		t.Errorf("cog://kernel/status not found in ListResources; got: %s",
			strings.Join(uris, ", "))
	}
}

// testExtractPort pulls the port number from an "http://127.0.0.1:PORT" URL.
// Uses the package-level extractPort helper (provider_mlx_supervised.go).
func testExtractPort(rawURL string) int {
	return extractPort(rawURL, 6931)
}
