package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/myrgic/cogos/internal/providers/vitalsretention"
)

// withVitalsWorkspace points the vitalsretention package's global
// workspace-root/node-key seams at a fresh temp dir + fixed node key,
// restoring prior state afterward — mirrors
// vitalsretention_test.go's withWorkspace helper, duplicated here because
// that helper is unexported in its own package.
func withVitalsWorkspace(t *testing.T, nodeKey string) string {
	t.Helper()
	root := t.TempDir()
	vitalsretention.SetWorkspaceRoot(root)
	vitalsretention.SetNodeKeySource(vitalsretention.NodeKeyFunc(func() string { return nodeKey }))
	vitalsretention.ReloadConfig(root)
	t.Cleanup(func() {
		vitalsretention.SetWorkspaceRoot("")
		vitalsretention.SetNodeKeySource(nil)
		vitalsretention.ReloadConfig(root)
	})
	return root
}

func TestHandleVitals_MissingRequiredParams(t *testing.T) {
	withVitalsWorkspace(t, "node-a")
	s := &Server{}

	cases := []string{
		"/v1/vitals?since=24h&resolution=raw",              // missing metric
		"/v1/vitals?metric=disk_free_bytes&resolution=raw", // missing since
		"/v1/vitals?metric=disk_free_bytes&since=24h",      // missing resolution
	}
	for _, url := range cases {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		rec := httptest.NewRecorder()
		s.handleVitals(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", url, rec.Code)
		}
	}
}

func TestHandleVitals_UnknownMetricIs400(t *testing.T) {
	withVitalsWorkspace(t, "node-a")
	s := &Server{}

	req := httptest.NewRequest(http.MethodGet, "/v1/vitals?metric=not_a_real_metric&since=24h&resolution=raw", nil)
	rec := httptest.NewRecorder()
	s.handleVitals(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an unknown metric (caller's fault), got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleVitals_PathologicalSinceIs400NotAHang is the HTTP-layer
// regression for cog-review's finding on PR #493 (cb26afa): a syntactically
// valid RFC3339 timestamp far in the past (parseTimeOrDuration accepts it
// without complaint) must be rejected by Window()'s maxWindowSpan bound
// rather than driving hundreds of thousands of day-file opens.
func TestHandleVitals_PathologicalSinceIs400NotAHang(t *testing.T) {
	withVitalsWorkspace(t, "node-a")
	s := &Server{}

	req := httptest.NewRequest(http.MethodGet, "/v1/vitals?metric=disk_free_bytes&since=0001-01-01T00:00:00Z&resolution=raw", nil)
	rec := httptest.NewRecorder()
	s.handleVitals(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a since exceeding maxWindowSpan, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleVitals_EmptyHistoryReturns200WithEmptyPoints(t *testing.T) {
	withVitalsWorkspace(t, "node-a")
	s := &Server{}

	req := httptest.NewRequest(http.MethodGet, "/v1/vitals?metric=disk_free_bytes&since=24h&resolution=raw", nil)
	rec := httptest.NewRecorder()
	s.handleVitals(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 for a metric/window with no recorded data, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp vitalsWindowResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Points == nil {
		t.Error("expected an empty (not nil-in-JSON) points array")
	}
	if len(resp.Points) != 0 {
		t.Errorf("expected 0 points, got %d", len(resp.Points))
	}
}

// TestHandleVitals_StorageErrorIs500NotCallerFault is the regression for
// the cog-review finding on PR #493 (fb9a291): a genuine server-side
// storage failure (here: a metric's raw directory replaced by a plain
// file, so the day-file open fails with ENOTDIR rather than ENOENT) must
// come back as 500, not 400 — the caller sent a perfectly valid request and
// has nothing to fix.
func TestHandleVitals_StorageErrorIs500NotCallerFault(t *testing.T) {
	root := withVitalsWorkspace(t, "node-a")

	blockedDir := filepath.Join(root, ".cog", "observatory", "vitals", "node-a", "raw", "disk_free_bytes")
	if err := os.MkdirAll(filepath.Dir(blockedDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blockedDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/v1/vitals?metric=disk_free_bytes&since=24h&resolution=raw", nil)
	rec := httptest.NewRecorder()
	s.handleVitals(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500 for a storage-layer failure, got %d: %s", rec.Code, rec.Body.String())
	}
}
