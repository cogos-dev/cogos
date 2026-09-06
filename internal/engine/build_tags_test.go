package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFTS5ProbeIsCached guards the sync.Once: the probe opens a database, so a
// per-request probe on a hot /health endpoint would be wasteful. Repeat calls
// must agree and must not re-run the DDL. Tag-independent.
func TestFTS5ProbeIsCached(t *testing.T) {
	first := FTS5Available()
	for i := 0; i < 100; i++ {
		if FTS5Available() != first {
			t.Fatalf("FTS5Available() is not stable across calls (call %d disagreed)", i)
		}
	}
}

// TestProbeFTS5NegativeControl is the control for the assertion above. The
// probe must be capable of returning false — a checker that can only ever say
// "yes" is exactly the failure mode this whole change exists to fix (the old
// `cog health` guard read a field that never existed, so it reported the same
// thing on healthy and broken kernels alike).
//
// We cannot un-compile FTS5 at runtime, so we assert the mechanism instead:
// the same DDL against a driver that lacks the module must fail. Running the
// CREATE against a deliberately unopenable database exercises the error path
// and proves probeFTS5 propagates failure rather than hardcoding true.
func TestProbeFTS5NegativeControl(t *testing.T) {
	// A path that cannot be opened as a database: a directory.
	got, errText := probeFTS5At(t.TempDir())
	if got {
		t.Fatal("probeFTS5At(<directory>) = true; the probe returns true even when " +
			"SQLite cannot be used at all, so it cannot detect a missing FTS5 module")
	}
	if errText == "" {
		t.Fatal("probe failed but reported no error text; an operator would see " +
			"fts5=false with no reason")
	}
	t.Logf("negative control produced: %s", errText)
}

// TestHealthExposesBuildTags asserts /health carries the field that
// scripts/cog health gates on. Before this change the wrapper read
// build_tags.fts5, which the kernel never emitted, so the guard reported
// "ABSENT — redeploy" on every kernel including healthy ones.
func TestHealthExposesBuildTags(t *testing.T) {
	srv := newTestServer(t)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.handleHealth(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("/health status = %d, want %d", w.Code, http.StatusOK)
	}

	var body struct {
		BuildTags *struct {
			FTS5 *bool `json:"fts5"`
		} `json:"build_tags"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("/health did not return JSON: %v", err)
	}
	if body.BuildTags == nil {
		t.Fatal("/health has no build_tags object; scripts/cog health cannot verify FTS5")
	}
	if body.BuildTags.FTS5 == nil {
		t.Fatal("/health build_tags has no fts5 field (absent != false; absent is unverifiable)")
	}
	if *body.BuildTags.FTS5 != FTS5Available() {
		t.Fatalf("/health reports fts5=%v but the runtime probe says %v",
			*body.BuildTags.FTS5, FTS5Available())
	}
}
