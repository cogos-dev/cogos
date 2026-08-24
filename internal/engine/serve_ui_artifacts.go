// serve_ui_artifacts.go — workspace-hosted UI artifacts.
//
//	GET /ui/                      → index page listing every artifact
//	GET /ui/{name}/...            → static files from $WORKSPACE/ui/{name}/
//	GET /v1/ui/artifacts          → JSON index of discovered artifacts
//
// Motivation: the kernel already serves two embedded UIs (GET / dashboard,
// GET /canvas). Those are compiled into the binary, so every new operator
// surface — a desk mockup, a sketch variant, a one-off report — either needs a
// kernel release or gets opened as a bare file:// page with no origin, no
// same-origin access to the kernel API, and no discoverability.
//
// This route hosts *workspace* artifacts instead of embedded ones: anything
// under $WORKSPACE/ui/ is served read-only over the same origin as the
// kernel API, so an artifact can `fetch('/v1/...')` without CORS, and the set
// of artifacts is versioned with the workspace rather than with the binary.
//
// Boundaries:
//   - Read-only. There is no upload path; artifacts arrive via git.
//   - Rooted. File resolution goes through os.Root, so a symlink or a
//     "..%2f" escape cannot read outside the artifacts directory (this is the
//     same class of bug as the traversal fix in #504; os.Root enforces it in
//     the kernel rather than in string-munging).
//   - Only files under an artifact directory are reachable; the artifacts root
//     itself is enumerated, never walked as a file tree.
//   - Absent directory is not an error: a workspace with no ui/ serves
//     an empty index. The route exists unconditionally so /v1/manifest
//     advertises a surface that is always really there.
package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// uiArtifactsDirName is the workspace-relative directory hosting artifacts.
const uiArtifactsDirName = "ui"

// uiArtifactIndexNames are tried, in order, when a directory is requested.
var uiArtifactIndexNames = []string{"index.html", "index.htm"}

// UIArtifact describes one artifact directory under the artifacts root.
type UIArtifact struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	URL      string    `json:"url"`
	Title    string    `json:"title,omitempty"`
	HasIndex bool      `json:"has_index"`
	Files    int       `json:"files"`
	Bytes    int64     `json:"bytes"`
	Modified time.Time `json:"modified"`
}

// uiArtifactsRoot returns the absolute artifacts directory for this server.
func (s *Server) uiArtifactsRoot() string {
	return filepath.Join(s.WorkspaceRoot(), uiArtifactsDirName)
}

// listUIArtifacts enumerates artifact directories. A missing artifacts root
// yields an empty slice and no error — an empty desk is a valid desk.
func (s *Server) listUIArtifacts() ([]UIArtifact, error) {
	root := s.uiArtifactsRoot()
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		return []UIArtifact{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]UIArtifact, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		a := UIArtifact{
			Name: e.Name(),
			Path: filepath.Join(uiArtifactsDirName, e.Name()),
			URL:  "/ui/" + e.Name() + "/",
		}
		dir := filepath.Join(root, e.Name())
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // a partially readable artifact still lists
			}
			info, err := d.Info()
			if err != nil {
				return nil //nolint:nilerr
			}
			a.Files++
			a.Bytes += info.Size()
			if info.ModTime().After(a.Modified) {
				a.Modified = info.ModTime()
			}
			return nil
		})
		for _, name := range uiArtifactIndexNames {
			idx := filepath.Join(dir, name)
			if st, err := os.Stat(idx); err == nil && !st.IsDir() {
				a.HasIndex = true
				a.Title = uiArtifactTitle(idx)
				break
			}
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// uiArtifactTitle reads the <title> out of an HTML file, cheaply and without
// parsing: only the first 8KB is scanned. An artifact with no title is listed
// under its directory name.
//
// Case folding is done with asciiLowerInPlace rather than strings.ToLower
// because the offsets found in the folded copy are used to slice the ORIGINAL
// bytes. strings.ToLower is not length-preserving — U+0130 folds 2 bytes to 1
// and U+212A folds 3 to 1 — so a non-ASCII codepoint anywhere before the
// <title> tag shifts every subsequent offset and slices out the wrong range
// (observed: a title reading "le>REAL TI" instead of "REAL TITLE"), or panics
// near the end of the buffer. HTML tag names are ASCII, so folding only ASCII
// is both sufficient and byte-for-byte length-preserving.
func uiArtifactTitle(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 8192)
	n, _ := io.ReadFull(f, buf)
	buf = buf[:n]
	head := string(asciiLowerInPlace(append([]byte(nil), buf...)))
	i := strings.Index(head, "<title>")
	if i < 0 {
		return ""
	}
	j := strings.Index(head[i:], "</title>")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(string(buf[i+len("<title>") : i+j]))
}

// asciiLowerInPlace lowercases A-Z in b and returns it. Every other byte —
// including all UTF-8 continuation bytes — is left untouched, so the result
// has exactly the same length and byte offsets as the input.
func asciiLowerInPlace(b []byte) []byte {
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return b
}

// handleUIArtifactIndex serves GET /v1/ui/artifacts as JSON.
func (s *Server) handleUIArtifactIndex(w http.ResponseWriter, r *http.Request) {
	arts, err := s.listUIArtifacts()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "artifact_index_failed", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"root":      s.uiArtifactsRoot(),
		"count":     len(arts),
		"artifacts": arts,
	})
}

// handleUIArtifacts serves GET /ui/{path...}.
//
// The bare /ui/ path renders an HTML index. Anything deeper resolves inside
// the artifacts root through os.Root, which refuses traversal and symlink
// escapes at the syscall boundary rather than by string inspection.
func (s *Server) handleUIArtifacts(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/ui")
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		s.renderUIArtifactIndexHTML(w)
		return
	}

	root, err := os.OpenRoot(s.uiArtifactsRoot())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.Error(w, "no artifacts directory in this workspace", http.StatusNotFound)
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "artifact_root_failed", err.Error())
		return
	}
	defer root.Close()

	clean := path.Clean(rel)
	// Reject only genuine traversal: "." or a leading ".." SEGMENT. A plain
	// HasPrefix(clean, "..") would also reject a legitimately named file such
	// as "..foo.txt", which os.Root resolves safely anyway.
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	st, err := root.Stat(clean)
	if err == nil && st.IsDir() {
		// Directory: redirect to a trailing slash so relative asset URLs in the
		// artifact resolve against the directory, then serve its index file.
		// The query string must survive the redirect — an artifact that reads
		// window.location.search would otherwise lose its state on exactly the
		// shareable URL this route exists to provide.
		if !strings.HasSuffix(r.URL.Path, "/") {
			target := r.URL.Path + "/"
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}
		found := false
		for _, name := range uiArtifactIndexNames {
			cand := path.Join(clean, name)
			if s2, err2 := root.Stat(cand); err2 == nil && !s2.IsDir() {
				clean, st, found = cand, s2, true
				break
			}
		}
		if !found {
			http.Error(w, "artifact has no index.html", http.StatusNotFound)
			return
		}
	} else if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	f, err := root.Open(clean)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	if ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(clean))); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	// Artifacts are edited in place during design work; never cache them.
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, path.Base(clean), st.ModTime(), f)
}

// renderUIArtifactIndexHTML renders the human-facing list at GET /ui/.
func (s *Server) renderUIArtifactIndexHTML(w http.ResponseWriter) {
	arts, err := s.listUIArtifacts()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var b strings.Builder
	b.WriteString(`<!doctype html><html><head><meta charset="utf-8"><title>CogOS — Workspace artifacts</title><style>
*{box-sizing:border-box;margin:0;padding:0}
body{font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;background:#0d1117;color:#e6edf3;padding:36px}
h1{font-size:17px;margin-bottom:4px}
.s{color:#8b949e;font-size:12px;margin-bottom:22px}
a.c{display:block;border:1px solid #30363d;background:#161b22;border-radius:10px;padding:14px 16px;margin-bottom:9px;text-decoration:none;color:inherit;max-width:720px}
a.c:hover{border-color:#58a6ff}
.t{font-weight:600;font-size:14px}
.m{color:#8b949e;font-size:11.5px;margin-top:3px}
.e{color:#8b949e}
code{background:#1c2129;padding:1px 5px;border-radius:4px;font-size:12px}
</style></head><body><h1>Workspace artifacts</h1><div class="s">served read-only from <code>`)
	b.WriteString(html.EscapeString(s.uiArtifactsRoot()))
	b.WriteString(`</code> · same origin as the kernel API</div>`)
	if len(arts) == 0 {
		b.WriteString(`<div class="e">No artifacts. Create <code>` +
			html.EscapeString(s.uiArtifactsRoot()) +
			`/&lt;name&gt;/index.html</code> and reload — no kernel restart needed.</div>`)
	}
	for _, a := range arts {
		title := a.Title
		if title == "" {
			title = a.Name
		}
		b.WriteString(fmt.Sprintf(
			`<a class="c" href="%s"><div class="t">%s</div><div class="m">%s · %d file(s) · %.1f KB%s</div></a>`,
			html.EscapeString(a.URL), html.EscapeString(title), html.EscapeString(a.Name),
			a.Files, float64(a.Bytes)/1024,
			map[bool]string{true: "", false: " · no index.html"}[a.HasIndex]))
	}
	b.WriteString(`</body></html>`)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(b.String()))
}
