// serve_ui_artifacts_test.go — conformance probes for the workspace artifact host.
//
// These exercise the public HTTP entrypoints (GET /ui/..., GET
// /v1/ui/artifacts) against a real temp workspace, not internal helpers, so
// the contract survives refactors of the resolution path.
package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newArtifactTestServer builds a Server with only the fields the artifact
// routes touch, plus a temp workspace, and returns it with the workspace root.
func newArtifactTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	ws := t.TempDir()
	s := &Server{cfg: &Config{WorkspaceRoot: ws}}
	if s.WorkspaceRoot() != ws {
		t.Fatalf("WorkspaceRoot() = %q, want %q", s.WorkspaceRoot(), ws)
	}
	return s, ws
}

func writeArtifact(t *testing.T, ws, name, file, body string) string {
	t.Helper()
	p := filepath.Join(ws, uiArtifactsDirName, name, file)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func get(t *testing.T, h http.HandlerFunc, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// A workspace with no artifacts directory must still answer — empty, not 500.
func TestUIArtifacts_MissingDirectoryIsEmptyNotError(t *testing.T) {
	s, _ := newArtifactTestServer(t)

	rec := get(t, s.handleUIArtifactIndex, "/v1/ui/artifacts")
	if rec.Code != http.StatusOK {
		t.Fatalf("index status = %d, want 200", rec.Code)
	}
	var got struct {
		Count     int          `json:"count"`
		Artifacts []UIArtifact `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Count != 0 || len(got.Artifacts) != 0 {
		t.Fatalf("count = %d / len = %d, want 0/0", got.Count, len(got.Artifacts))
	}

	rec = get(t, s.handleUIArtifacts, "/ui/")
	if rec.Code != http.StatusOK {
		t.Fatalf("html index status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "No artifacts") {
		t.Error("empty html index should say so")
	}
}

// An artifact directory is discovered, titled from its HTML, and served.
func TestUIArtifacts_DiscoverAndServe(t *testing.T) {
	s, ws := newArtifactTestServer(t)
	writeArtifact(t, ws, "desk-console", "index.html",
		"<!doctype html><html><head><title>Desk — Console</title></head><body>hi</body></html>")
	writeArtifact(t, ws, "desk-console", "app.js", "console.log(1)")

	rec := get(t, s.handleUIArtifactIndex, "/v1/ui/artifacts")
	var got struct {
		Count     int          `json:"count"`
		Artifacts []UIArtifact `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Count != 1 {
		t.Fatalf("count = %d, want 1", got.Count)
	}
	a := got.Artifacts[0]
	if a.Name != "desk-console" || a.URL != "/ui/desk-console/" {
		t.Errorf("name/url = %q/%q", a.Name, a.URL)
	}
	if !a.HasIndex {
		t.Error("HasIndex = false, want true")
	}
	if a.Title != "Desk — Console" {
		t.Errorf("Title = %q, want %q", a.Title, "Desk — Console")
	}
	if a.Files != 2 {
		t.Errorf("Files = %d, want 2", a.Files)
	}

	// Directory request serves the index with an HTML content type.
	rec = get(t, s.handleUIArtifacts, "/ui/desk-console/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Desk — Console") {
		t.Error("index body not served")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}

	// Nested asset is served with its own content type.
	rec = get(t, s.handleUIArtifacts, "/ui/desk-console/app.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("asset status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("asset Content-Type = %q, want javascript", ct)
	}
}

// Without a trailing slash the handler must redirect, so relative asset URLs
// inside the artifact resolve against the artifact directory.
func TestUIArtifacts_DirectoryRedirectsToTrailingSlash(t *testing.T) {
	s, ws := newArtifactTestServer(t)
	writeArtifact(t, ws, "sketch", "index.html", "<title>S</title>")

	rec := get(t, s.handleUIArtifacts, "/ui/sketch")
	if rec.Code != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want 301", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/ui/sketch/" {
		t.Errorf("Location = %q, want /ui/sketch/", loc)
	}
}

// Traversal must not escape the artifacts root, in any encoding, and a
// symlink pointing outside must not be followed.
func TestUIArtifacts_TraversalAndSymlinkAreRefused(t *testing.T) {
	s, ws := newArtifactTestServer(t)
	writeArtifact(t, ws, "ok", "index.html", "<title>ok</title>")
	secret := filepath.Join(ws, "SECRET.md")
	if err := os.WriteFile(secret, []byte("do not serve me"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink inside an artifact aimed at the workspace-level secret.
	link := filepath.Join(ws, uiArtifactsDirName, "ok", "escape.md")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	for _, target := range []string{
		"/ui/../SECRET.md",
		"/ui/ok/../../SECRET.md",
		"/ui/%2e%2e/SECRET.md",
		"/ui/ok/escape.md",
	} {
		rec := get(t, s.handleUIArtifacts, target)
		if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "do not serve me") {
			t.Errorf("%s leaked workspace content (status %d)", target, rec.Code)
		}
	}
}

// A directory with no index file is a 404, not a directory listing — the
// artifacts root is enumerated, never walked.
func TestUIArtifacts_NoIndexIsNotAListing(t *testing.T) {
	s, ws := newArtifactTestServer(t)
	writeArtifact(t, ws, "bare", "data.json", `{"a":1}`)

	rec := get(t, s.handleUIArtifacts, "/ui/bare/")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "data.json") {
		t.Error("directory contents leaked into the response")
	}
	// The file itself is still reachable by name.
	if rec = get(t, s.handleUIArtifacts, "/ui/bare/data.json"); rec.Code != http.StatusOK {
		t.Errorf("named file status = %d, want 200", rec.Code)
	}
}

// Artifacts are edited live during design work; responses must not be cached.
func TestUIArtifacts_NoCacheHeader(t *testing.T) {
	s, ws := newArtifactTestServer(t)
	writeArtifact(t, ws, "a", "index.html", "<title>a</title>")
	for _, tc := range []struct {
		h      http.HandlerFunc
		target string
	}{
		{s.handleUIArtifacts, "/ui/"},
		{s.handleUIArtifacts, "/ui/a/"},
		{s.handleUIArtifactIndex, "/v1/ui/artifacts"},
	} {
		if cc := get(t, tc.h, tc.target).Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s Cache-Control = %q, want no-cache", tc.target, cc)
		}
	}
}

// Dotfile directories are not artifacts.
func TestUIArtifacts_HiddenDirsExcluded(t *testing.T) {
	s, ws := newArtifactTestServer(t)
	writeArtifact(t, ws, ".git", "config", "x")
	writeArtifact(t, ws, "real", "index.html", "<title>r</title>")

	rec := get(t, s.handleUIArtifactIndex, "/v1/ui/artifacts")
	var got struct {
		Artifacts []UIArtifact `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0].Name != "real" {
		t.Fatalf("artifacts = %+v, want only 'real'", got.Artifacts)
	}
}
