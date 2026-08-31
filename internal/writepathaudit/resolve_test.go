package writepathaudit

// resolve_test.go — unit coverage for the path-resolution heuristic itself,
// independent of the live repo scan. Uses small synthetic snippets so these
// stay stable regardless of what the real codebase does.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// parseCallArg parses src (a single Go source file's text) and returns the
// first matched writer call's path argument expression, the local defs for
// its enclosing function, and a *resolver scoped to that function/method —
// built with an EMPTY global index, so these cases exercise only the
// structural resolution rules, not interprocedural chasing (see
// TestFieldAndParamOriginChase for that).
func parseCallArg(t *testing.T, src string) (ast.Expr, map[string]ast.Expr, *resolver) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	scopes := collectFuncScopes(f, fset)
	idx := &globalIndex{
		funcDecls: map[funcKey]*ast.FuncDecl{},
		funcCalls: map[funcKey][]callSite{},
		fieldCand: map[string][]fieldCandidate{},
		fieldMemo: map[string]originResult{},
		paramMemo: map[string]originResult{},
	}

	var argExpr ast.Expr
	var sc funcScope
	var defs map[string]ast.Expr
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		thisScope := enclosingFuncScope(scopes, call.Pos())
		thisDefs := collectLocalDefs(thisScope.body)
		primitive, i, matched := matchWriterCall(call, thisDefs)
		if !matched {
			return true
		}
		_ = primitive
		if argExpr == nil {
			argExpr = call.Args[i]
			sc = thisScope
			defs = thisDefs
		}
		return true
	})
	if argExpr == nil {
		t.Fatalf("no matched writer call found in snippet")
	}
	return argExpr, defs, resolverContextFor(idx, "snippet", sc)
}

func TestResolveExpr_Cases(t *testing.T) {
	tests := []struct {
		name         string
		src          string
		wantPattern  string
		wantResolved bool
		wantCategory string
	}{
		{
			name: "string literal",
			src: `package p
func f() {
	os.WriteFile("/etc/cogos/config.yaml", nil, 0644)
}`,
			wantPattern:  "/etc/cogos/config.yaml",
			wantResolved: true,
			wantCategory: "elsewhere",
		},
		{
			name: "filepath.Join with CogDir field",
			src: `package p
func f(cfg *Config) {
	os.MkdirAll(filepath.Join(cfg.CogDir, "ledger", sessionID), 0755)
}`,
			wantPattern:  "<CogDir>/ledger/{sessionID}",
			wantResolved: true,
			wantCategory: "cog",
		},
		{
			name: "workspaceRoot param plus literal .cog segment",
			src: `package p
func AppendEvent(workspaceRoot, sessionID string) {
	path := filepath.Join(workspaceRoot, ".cog", "ledger", sessionID, "events.jsonl")
	os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
}`,
			wantPattern:  "<WorkspaceRoot>/.cog/ledger/{sessionID}/events.jsonl",
			wantResolved: true,
			wantCategory: "cog",
		},
		{
			name: "os.UserHomeDir anchor",
			src: `package p
func f() {
	home, _ := os.UserHomeDir()
	os.WriteFile(filepath.Join(home, ".cogos", "state.json"), nil, 0644)
}`,
			wantPattern:  "<Home>/.cogos/state.json",
			wantResolved: true,
			wantCategory: "home",
		},
		{
			name: "partial resolution still classifies as cog when an anchor is visible",
			src: `package p
func AppendEvent(workspaceRoot, sessionID string) {
	dir := filepath.Join(workspaceRoot, ".cog", "ledger", pathsafe.SanitizeComponent(sessionID))
	os.MkdirAll(dir, 0755)
}`,
			wantPattern:  "<WorkspaceRoot>/.cog/ledger/<call:pathsafe.SanitizeComponent>",
			wantResolved: false,
			wantCategory: "cog",
		},
		{
			name: "opaque method call is DYNAMIC",
			src: `package p
func (m *Manager) f() {
	os.WriteFile(m.RegistryPath(), data, 0644)
}`,
			wantPattern:  "<call:m.RegistryPath>",
			wantResolved: false,
			wantCategory: "dynamic",
		},
		{
			name: "read-only OpenFile is not matched at all",
			src: `package p
func f() {
	os.OpenFile(path, os.O_RDONLY, 0)
}`,
			wantPattern:  "",
			wantResolved: false,
			wantCategory: "",
		},
		{
			// The load-bearing regression: an unresolvable struct field
			// root with NO chaseable composite literal anywhere (no
			// global index consulted here) must land in UNANCHORED, never
			// "elsewhere" — over-claiming "not under .cog/" for a
			// destination this tool has no information about at all was
			// the tool's worst failure mode (see classify's doc comment).
			name: "opaque receiver field with no chase available is UNANCHORED, not elsewhere",
			src: `package p
func (bs *BlobStore) f() {
	os.MkdirAll(bs.root, 0755)
}`,
			wantPattern:  "{bs.root}",
			wantResolved: true,
			wantCategory: "unanchored",
		},
		{
			// Same failure mode via a different field/primitive shape
			// (append-mode OpenFile, serve_attention.go's real pattern) —
			// still must not be laundered into "elsewhere".
			name: "opaque field on an append-mode OpenFile is UNANCHORED",
			src: `package p
func (l *attentionLog) f() {
	os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
}`,
			wantPattern:  "{l.path}",
			wantResolved: true,
			wantCategory: "unanchored",
		},
		{
			name: "os.CreateTemp dir argument is a recognized write site",
			src: `package p
func f(dir string) {
	os.CreateTemp(dir, "blob-put-*")
}`,
			wantPattern:  "{dir}",
			wantResolved: true,
			wantCategory: "unanchored",
		},
		{
			name: "sql.Open with sqlite3 driver treats the DSN as the write target",
			src: `package p
func f(workspaceRoot string) {
	dbPath := filepath.Join(workspaceRoot, ".cog", ".state", "constellation.db")
	sql.Open("sqlite3", dbPath+"?_journal_mode=WAL")
}`,
			wantPattern:  "<WorkspaceRoot>/.cog/.state/constellation.db?_journal_mode=WAL",
			wantResolved: true,
			wantCategory: "cog",
		},
		{
			name: "sql.Open with a non-sqlite driver is not matched",
			src: `package p
func f(dsn string) {
	sql.Open("postgres", dsn)
}`,
			wantPattern:  "",
			wantResolved: false,
			wantCategory: "",
		},
		{
			name: "io.Copy into an untraceable destination is not matched at all",
			src: `package p
func f(w io.Writer, r io.Reader) {
	io.Copy(w, r)
}`,
			wantPattern:  "",
			wantResolved: false,
			wantCategory: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			switch tt.name {
			case "read-only OpenFile is not matched at all",
				"sql.Open with a non-sqlite driver is not matched",
				"io.Copy into an untraceable destination is not matched at all":
				fset := token.NewFileSet()
				f, err := parser.ParseFile(fset, "snippet.go", tt.src, 0)
				if err != nil {
					t.Fatalf("parse: %v", err)
				}
				matched := false
				ast.Inspect(f, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					if _, _, ok := matchWriterCall(call, map[string]ast.Expr{}); ok {
						matched = true
					}
					return true
				})
				if matched {
					t.Fatalf("expected no write-primitive match in this snippet")
				}
				return
			}

			argExpr, defs, r := parseCallArg(t, tt.src)
			gotPattern, gotResolved := r.resolveExpr(argExpr, defs, 0)
			if gotPattern != tt.wantPattern {
				t.Errorf("pattern = %q, want %q", gotPattern, tt.wantPattern)
			}
			if gotResolved != tt.wantResolved {
				t.Errorf("resolved = %v, want %v", gotResolved, tt.wantResolved)
			}
			gotCategory := classify(gotPattern, gotResolved)
			if gotCategory != tt.wantCategory {
				t.Errorf("category = %q, want %q", gotCategory, tt.wantCategory)
			}
		})
	}
}

// TestJoinEmptyBaseNeverElsewhere reproduces the exact scenario from the
// CI review's confirmed finding against audit.go's filepath.Join handling:
// a base that resolves to the literal empty string, joined with a real
// filename. The buggy implementation joined resolved parts with a literal
// strings.Join(parts, "/"), which does not replicate real filepath.Join's
// empty-component-skipping — Join("", "state.json") is "state.json" at
// runtime, never "/state.json" — so the tool's own resolution disagreed
// with the runtime behavior it claims to model, produced a spurious
// leading-slash artifact, and isOpaqueRoot didn't recognize that artifact
// as degenerate either, letting classify stamp it "elsewhere": a positive
// claim that the write lands outside .cog/ that neither the true runtime
// path nor the tool's own resolution logic ever actually established.
//
// Fixed in two places: the Join handler now skips empty-resolved parts
// (matching real filepath.Join), and isBareUnanchoredLiteral in classify
// additionally refuses "elsewhere" for the bare, anchor-less single
// segment ("state.json") that a correctly-fixed join now produces here.
func TestJoinEmptyBaseNeverElsewhere(t *testing.T) {
	src := `package p
func f() {
	base := ""
	os.WriteFile(filepath.Join(base, "state.json"), nil, 0644)
}`
	argExpr, defs, r := parseCallArg(t, src)
	pattern, resolved := r.resolveExpr(argExpr, defs, 0)

	if pattern != "state.json" {
		t.Errorf("pattern = %q, want %q (filepath.Join must skip the empty base, not leave a stray leading slash)", pattern, "state.json")
	}
	if !resolved {
		t.Errorf("resolved = false, want true — both the base and the literal resolved successfully")
	}
	if got := classify(pattern, resolved); got == "elsewhere" {
		t.Errorf("classify(%q, %v) = %q, want anything but \"elsewhere\" — a bare relative literal with no anchor has not positively established a destination outside .cog/", pattern, resolved, got)
	}
}

// TestConcatEmptyBaseNeverElsewhere reproduces the CONCATENATION shape of
// the same over-claim TestJoinEmptyBaseNeverElsewhere fixes for
// filepath.Join: `base := ""` followed by `base + "/observations.jsonl"`
// reached through string concatenation (token.ADD on a *ast.BinaryExpr)
// rather than a filepath.Join call. The Join fix alone did not cover this
// producer — the round-2 CI review's confirmed finding against it — because
// the "/" here is already baked into the right-hand literal rather than
// inserted as a separator between joined parts, so Join's empty-part-
// skipping logic never runs. The fix instead tags the concatenation result
// with degenerateRootMarker whenever the left operand resolves to the
// literal empty string and the right operand starts with "/", and
// isOpaqueRoot treats that marker as an opaque root — the same "we have not
// positively established a destination outside .cog/" honesty margin the
// Join case lands in, not a confirmed absolute-path literal like
// "/etc/cogos/config.yaml".
func TestConcatEmptyBaseNeverElsewhere(t *testing.T) {
	src := `package p
func f() {
	base := ""
	os.WriteFile(base+"/observations.jsonl", nil, 0644)
}`
	argExpr, defs, r := parseCallArg(t, src)
	pattern, resolved := r.resolveExpr(argExpr, defs, 0)

	if !resolved {
		t.Errorf("resolved = false, want true — both the base and the literal resolved successfully")
	}
	if got := classify(pattern, resolved); got == "elsewhere" {
		t.Errorf("classify(%q, %v) = %q, want anything but \"elsewhere\" — the leading slash came from concatenating a literal onto an empty-resolved base, not from a genuine absolute-path literal", pattern, resolved, got)
	}

	// stripDegenerateRootMarker is what a Site.Pattern actually carries —
	// the marker itself must never reach the rendered inventory.
	if displayed := stripDegenerateRootMarker(pattern); displayed != "/observations.jsonl" {
		t.Errorf("stripDegenerateRootMarker(%q) = %q, want %q", pattern, displayed, "/observations.jsonl")
	}
}

// TestFieldAndParamOriginChase exercises the callable-origin index directly
// — the mechanism that chases a struct field or a bare function parameter
// back to where it was actually constructed, closing the exact defect class
// the round-3 adversarial gate found (blobstore's bs.root, bep_provider's
// p.watchDir, and friends — see scan_test.go's named regression fixtures
// for the real-repo versions of these).
func TestFieldAndParamOriginChase(t *testing.T) {
	tests := []struct {
		name         string
		src          string
		wantPattern  string
		wantResolved bool
		wantCategory string
	}{
		{
			// blobstore.go's exact shape: field set from a filepath.Join
			// inside the constructor, where the constructor's own
			// parameter is already anchor-named — no call-site chase
			// needed, just the composite-literal lookup itself.
			name: "field origin resolved directly from constructor's own anchor-named param",
			src: `package p
type BlobStore struct { root string }
func NewBlobStore(workspaceRoot string) *BlobStore {
	return &BlobStore{root: filepath.Join(workspaceRoot, ".cog", "blobs")}
}
func (bs *BlobStore) ensureDir() {
	os.MkdirAll(bs.root, 0o755)
}`,
			wantPattern:  "<WorkspaceRoot>/.cog/blobs",
			wantResolved: true,
			wantCategory: "cog",
		},
		{
			// bep_provider.go's exact shape: the constructor's own param
			// is NOT anchor-named ("root"), so the field origin dead-ends
			// one level further — into the CALL SITE of the constructor.
			name: "field origin chased through a non-anchor-named constructor param to its call site",
			src: `package p
type BEPProvider struct { watchDir string }
func NewBEPProvider(root string) *BEPProvider {
	return &BEPProvider{watchDir: filepath.Join(root, ".cog", "bin", "agents", "definitions")}
}
func useIt(cfg *Config) {
	provider := NewBEPProvider(cfg.WorkspaceRoot)
	_ = provider
}
func (p *BEPProvider) start() {
	os.MkdirAll(p.watchDir, 0755)
}`,
			wantPattern:  "<WorkspaceRoot>/.cog/bin/agents/definitions",
			wantResolved: true,
			wantCategory: "cog",
		},
		{
			// rotating_writer.go's exact shape: a pure passthrough field
			// (path: path) with NO filepath.Join inside the constructor at
			// all — the entire value comes from the call site.
			name: "pure passthrough field chased entirely from the call site",
			src: `package p
type rotatingWriter struct { path string }
func newRotatingWriter(path string) *rotatingWriter {
	return &rotatingWriter{path: path}
}
func caller(workspaceRoot string) {
	newRotatingWriter(filepath.Join(workspaceRoot, ".cog", "run", "kernel.log.jsonl"))
}
func (w *rotatingWriter) rotate() {
	os.Rename(w.path, w.path+".1")
}`,
			wantPattern:  "<WorkspaceRoot>/.cog/run/kernel.log.jsonl.1",
			wantResolved: true,
			wantCategory: "cog",
		},
		{
			// Two DISAGREEING call sites must dead-end, never guess: the
			// param chase for newLogger's own "path" parameter refuses to
			// pick between "/etc/a.log" and "/etc/b.log", so the
			// composite-literal field resolves only as far as that
			// parameter's own opaque placeholder — never silently
			// resolving to EITHER caller's literal, and never landing in
			// "elsewhere" for a destination this tool actually could not
			// pin down.
			name: "conflicting call sites never guess — dead ends to opaque/unanchored",
			src: `package p
type Logger struct { path string }
func newLogger(path string) *Logger {
	return &Logger{path: path}
}
func callerA() { newLogger("/etc/a.log") }
func callerB() { newLogger("/etc/b.log") }
func (l *Logger) write() {
	os.WriteFile(l.path, nil, 0644)
}`,
			wantPattern:  "{path}",
			wantResolved: true,
			wantCategory: "unanchored",
		},
		{
			// CertDir()-shaped case: two return branches with DIFFERENT
			// literal roots that nonetheless agree on category — accepted,
			// using the first resolvable branch's pattern.
			name: "multi-branch return agreeing on category is accepted",
			src: `package p
func CertDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".cog", "etc")
	}
	return filepath.Join(home, ".cog", "etc")
}
func f() {
	os.MkdirAll(CertDir(), 0700)
}`,
			wantPattern:  "./.cog/etc",
			wantResolved: true,
			wantCategory: "cog",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "snippet.go", tt.src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			pf := parsedFile{
				relPath: "snippet.go",
				dir:     "pkg",
				fset:    fset,
				file:    f,
				scopes:  collectFuncScopes(f, fset),
			}
			idx := buildGlobalIndex([]parsedFile{pf})
			sites := scanParsedFile(pf, idx)
			if len(sites) == 0 {
				t.Fatalf("no write site found in snippet")
			}
			// The write site under test is always the LAST one declared
			// (constructors/helpers above it are not writers themselves).
			got := sites[len(sites)-1]
			if got.Pattern != tt.wantPattern {
				t.Errorf("pattern = %q, want %q", got.Pattern, tt.wantPattern)
			}
			if got.Resolved != tt.wantResolved {
				t.Errorf("resolved = %v, want %v", got.Resolved, tt.wantResolved)
			}
			if got.Category != tt.wantCategory {
				t.Errorf("category = %q, want %q", got.Category, tt.wantCategory)
			}
		})
	}
}

// TestIOCopyIntoCreatedFile exercises the "where visible" io.Copy gating
// (see matchWriterCall's package doc): serve_blocks.go's exact shape —
// stream a request body into an os.CreateTemp'd file, then rename it into
// place. Both the CreateTemp and the io.Copy must appear as their own
// sites, at their own line, both resolving to the same destination.
func TestIOCopyIntoCreatedFile(t *testing.T) {
	src := `package p
func handlePut(dir string) {
	tmpFile, err := os.CreateTemp(dir, "blob-put-*")
	if err != nil {
		return
	}
	io.Copy(tmpFile, body)
}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pf := parsedFile{relPath: "snippet.go", dir: "pkg", fset: fset, file: f, scopes: collectFuncScopes(f, fset)}
	idx := buildGlobalIndex([]parsedFile{pf})
	sites := scanParsedFile(pf, idx)

	var sawCreateTemp, sawCopy bool
	for _, s := range sites {
		switch s.Primitive {
		case "os.CreateTemp":
			sawCreateTemp = true
			if s.Pattern != "{dir}" || s.Category != "unanchored" {
				t.Errorf("os.CreateTemp site: pattern=%q category=%q, want {dir}/unanchored", s.Pattern, s.Category)
			}
		case "io.Copy":
			sawCopy = true
			if s.Pattern != "{dir}" || s.Category != "unanchored" {
				t.Errorf("io.Copy site: pattern=%q category=%q, want {dir}/unanchored", s.Pattern, s.Category)
			}
		}
	}
	if !sawCreateTemp {
		t.Errorf("expected an os.CreateTemp site")
	}
	if !sawCopy {
		t.Errorf("expected an io.Copy site chased through to the CreateTemp target")
	}
}

// TestFieldOriginMemo_DoesNotPoisonAcrossDepths reproduces the CI review's
// unverified note against fieldOrigin/paramOrigin: both memoize by
// (type, field) / (dir, func, paramIdx) alone, with no dependency on the
// depth budget the FIRST caller to reach that key happened to have left.
// A deeply-nested call site (here, objA.path buried under many layers of
// filepath.Join) can genuinely exhaust maxChaseDepth resolving Shared's
// field candidate, while a second, SHALLOW call site referencing the exact
// same field (objB.path, one hop from a bare receiver) has ample budget to
// resolve it fully. Before the fix, whichever site's resolution ran FIRST
// (here, the deep one, textually first in the file and so visited first
// by the single ast.Inspect walk) decided the answer for BOTH — the
// shallow site inherited the deep site's exhausted-budget failure and was
// wrongly downgraded from "home" to "unanchored".
func TestFieldOriginMemo_DoesNotPoisonAcrossDepths(t *testing.T) {
	joinWrap := func(n int, inner string) string {
		s := inner
		for i := 0; i < n; i++ {
			s = "filepath.Join(" + s + ")"
		}
		return s
	}
	// 5 wrapping Joins around the field's own composite-literal value is
	// comfortably resolvable from a fresh (depth-0) call, but pushes a
	// call that starts 11 Joins deep past maxChaseDepth (=16).
	src := `package p

type Shared struct{ path string }

func makeShared(homeDir string) *Shared {
	return &Shared{path: ` + joinWrap(5, "homeDir") + `}
}

func (objA *Shared) deepSite() {
	os.WriteFile(` + joinWrap(11, "objA.path") + `, nil, 0644)
}

func (objB *Shared) shallowSite() {
	os.WriteFile(objB.path, nil, 0644)
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pf := parsedFile{relPath: "snippet.go", dir: "pkg", fset: fset, file: f, scopes: collectFuncScopes(f, fset)}
	idx := buildGlobalIndex([]parsedFile{pf})
	sites := scanParsedFile(pf, idx)

	var sawShallow bool
	for _, s := range sites {
		if s.Func != "(*Shared).shallowSite" {
			continue
		}
		sawShallow = true
		// The deep site's exhausted budget must never leak into this
		// site's answer: with its own full budget, objB.path resolves
		// all the way to the <Home> anchor, category "home" — never the
		// deep site's opaque "{objA.path}"/"unanchored" leftover.
		if s.Pattern != "<Home>" || s.Category != "home" {
			t.Errorf("shallowSite: pattern=%q category=%q, want <Home>/home — poisoned by an unrelated deep call site's exhausted chase-depth budget", s.Pattern, s.Category)
		}
	}
	if !sawShallow {
		t.Fatalf("expected a shallowSite write site")
	}
}

func TestSubsystemOf(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"internal/providers/discord/discord_provider.go", "provider:discord"},
		{"internal/engine/ledger.go", "internal:engine"},
		{"pkg/cogblock/ledger.go", "pkg:cogblock"},
		{"sdk/cogos.go", "sdk"},
		{"sdk/internal/fs/fs.go", "sdk"},
		{"cmd/cogos/main.go", "cmd"},
		{"harness/config.go", "harness"},
		{"envspec/spec.go", "other"},
	}
	for _, tt := range tests {
		if got := subsystemOf(tt.path); got != tt.want {
			t.Errorf("subsystemOf(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}
