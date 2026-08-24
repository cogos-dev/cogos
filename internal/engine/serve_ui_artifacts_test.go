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

// A non-ASCII codepoint before <title> must not shift the byte offsets used to
// slice the ORIGINAL buffer. strings.ToLower is not length-preserving (U+0130
// folds 2 bytes to 1, U+212A folds 3 to 1), so folding with it and then slicing
// the unfolded bytes yields a garbled title — or an out-of-range slice near the
// end of the buffer. Regression probe for cog-review's note on #581.
func TestUIArtifacts_TitleWithNonASCIIBeforeTag(t *testing.T) {
	s, ws := newArtifactTestServer(t)
	for name, head := range map[string]string{
		// U+0130 LATIN CAPITAL LETTER I WITH DOT ABOVE: 2 bytes -> 1 when folded.
		"dotted-i": "<meta content=\"\u0130\u0130\u0130\"><title>REAL TITLE</title>",
		// U+212A KELVIN SIGN: 3 bytes -> 1 when folded.
		"kelvin": "<meta content=\"\u212a\u212a\"><title>REAL TITLE</title>",
		// Mixed, plus a codepoint that folds to the same length.
		"mixed": "<meta content=\"\u0130\u00c9\u212a caf\u00e9\"><title>REAL TITLE</title>",
	} {
		writeArtifact(t, ws, name, "index.html",
			"<!doctype html><html><head>"+head+"</head><body>x</body></html>")
	}

	rec := get(t, s.handleUIArtifactIndex, "/v1/ui/artifacts")
	var got struct {
		Artifacts []UIArtifact `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Artifacts) != 3 {
		t.Fatalf("got %d artifacts, want 3", len(got.Artifacts))
	}
	for _, a := range got.Artifacts {
		if a.Title != "REAL TITLE" {
			t.Errorf("%s: Title = %q, want %q (offset shift from non-ASCII folding)",
				a.Name, a.Title, "REAL TITLE")
		}
	}
}

// A <title> preceded by codepoints whose lowercase form is LONGER in bytes
// must not panic. Exactly two runes in Unicode grow when lowercased —
// U+023A (Ⱥ, 2 bytes -> 3) and U+023E (Ⱦ, 2 bytes -> 3) — and enough of them
// before the tag pushes the offsets found in the folded copy past the end of
// the original buffer: "slice bounds out of range [:129] with capacity 97".
// The shrinking runes (U+0130, U+212A) only garble; these crash.
func TestUIArtifacts_TitleNonASCIIDoesNotPanic(t *testing.T) {
	s, ws := newArtifactTestServer(t)
	writeArtifact(t, ws, "pathological", "index.html",
		strings.Repeat("\u023a", 40)+"<title>OK</title>")
	writeArtifact(t, ws, "pathological2", "index.html",
		strings.Repeat("\u023e", 40)+"<title>OK</title>")

	// Under the buggy implementation this panics inside listUIArtifacts,
	// taking the handler — and this test process — down.
	rec := get(t, s.handleUIArtifactIndex, "/v1/ui/artifacts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Artifacts []UIArtifact `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, a := range got.Artifacts {
		if a.Title != "OK" {
			t.Errorf("%s: Title = %q, want %q", a.Name, a.Title, "OK")
		}
	}
}

// A file whose name legitimately begins with ".." is not traversal. os.Root
// resolves it inside the artifact; a naive HasPrefix(clean, "..") guard would
// 404 it. Note the name must be at the TOP level of the URL path for the naive
// guard to bite — path.Clean("odd/..foo.txt") does not start with "..", so a
// nested file would pass even against the buggy guard and prove nothing.
func TestUIArtifacts_DotDotPrefixedFilenameIsServed(t *testing.T) {
	s, ws := newArtifactTestServer(t)
	// The artifact DIRECTORY itself is named "..odd", so the request path is
	// "/ui/..odd/index.html" and path.Clean leaves a leading "..".
	writeArtifact(t, ws, "..odd", "index.html", "<title>odd</title>legitimate content")

	rec := get(t, s.handleUIArtifacts, "/ui/..odd/index.html")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (name starts with '..' but is not traversal)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "legitimate content") {
		t.Error("file body not served")
	}
}

// A directory redirect must carry the query string through. Artifacts that
// read window.location.search would otherwise lose their state on exactly the
// shareable URL this route exists to provide. Regression probe for
// cog-review's confirmed finding on #581.
func TestUIArtifacts_RedirectPreservesQueryString(t *testing.T) {
	s, ws := newArtifactTestServer(t)
	writeArtifact(t, ws, "desk", "index.html", "<title>d</title>")

	for _, tc := range []struct{ target, wantLoc string }{
		{"/ui/desk?theme=dark", "/ui/desk/?theme=dark"},
		{"/ui/desk?a=1&b=2", "/ui/desk/?a=1&b=2"},
		{"/ui/desk", "/ui/desk/"}, // no query: no stray "?"
	} {
		rec := get(t, s.handleUIArtifacts, tc.target)
		if rec.Code != http.StatusMovedPermanently {
			t.Fatalf("%s: status = %d, want 301", tc.target, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != tc.wantLoc {
			t.Errorf("%s: Location = %q, want %q", tc.target, loc, tc.wantLoc)
		}
	}
}

// listUIArtifacts walks artifact directories on the OS filesystem rather than
// through os.Root. A symlink inside an artifact must not contribute its
// TARGET's size to the reported byte count: fs.DirEntry.Info() lstats, so the
// link's own path length is counted instead. This pins that behaviour so a
// future switch to a following-stat would fail loudly rather than silently
// reporting sizes of files outside the artifacts root.
func TestUIArtifacts_SymlinkSizeNotFollowedInIndex(t *testing.T) {
	s, ws := newArtifactTestServer(t)
	writeArtifact(t, ws, "art", "index.html", "<title>a</title>")

	big := filepath.Join(ws, "BIG.bin")
	if err := os.WriteFile(big, make([]byte, 100000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(big, filepath.Join(ws, uiArtifactsDirName, "art", "link.bin")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	rec := get(t, s.handleUIArtifactIndex, "/v1/ui/artifacts")
	var got struct {
		Artifacts []UIArtifact `json:"artifacts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Artifacts) != 1 {
		t.Fatalf("got %d artifacts, want 1", len(got.Artifacts))
	}
	if b := got.Artifacts[0].Bytes; b >= 100000 {
		t.Errorf("Bytes = %d — symlink target size leaked into the index", b)
	}
	// And the symlink is still not servable through os.Root.
	if rec := get(t, s.handleUIArtifacts, "/ui/art/link.bin"); rec.Code == http.StatusOK {
		t.Errorf("symlink served with status %d; want refusal", rec.Code)
	}
}
