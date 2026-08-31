package writepathaudit

// resolve_test.go — unit coverage for the path-resolution heuristic itself,
// independent of the live repo scan. Uses small synthetic snippets so these
// stay stable regardless of what the real codebase does.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
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
			gotPattern, gotResolved, gotDegenerate := r.resolveExpr(argExpr, defs, 0)
			if gotPattern != tt.wantPattern {
				t.Errorf("pattern = %q, want %q", gotPattern, tt.wantPattern)
			}
			if gotResolved != tt.wantResolved {
				t.Errorf("resolved = %v, want %v", gotResolved, tt.wantResolved)
			}
			gotCategory := classify(gotPattern, gotResolved, gotDegenerate)
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
	pattern, resolved, degenerate := r.resolveExpr(argExpr, defs, 0)

	if pattern != "state.json" {
		t.Errorf("pattern = %q, want %q (filepath.Join must skip the empty base, not leave a stray leading slash)", pattern, "state.json")
	}
	if !resolved {
		t.Errorf("resolved = false, want true — both the base and the literal resolved successfully")
	}
	if got := classify(pattern, resolved, degenerate); got == "elsewhere" {
		t.Errorf("classify(%q, %v, %v) = %q, want anything but \"elsewhere\" — a bare relative literal with no anchor has not positively established a destination outside .cog/", pattern, resolved, degenerate, got)
	}
}

// TestConcatEmptyBaseNeverElsewhere reproduces the CONCATENATION shape of
// the same over-claim TestJoinEmptyBaseNeverElsewhere fixes for
// filepath.Join: `base := ""` followed by `base + "/observations.jsonl"`
// reached through string concatenation (token.ADD on a *ast.BinaryExpr)
// rather than a filepath.Join call. The Join fix alone did not cover this
// producer because the "/" here is already baked into the right-hand
// literal rather than inserted as a separator between joined parts, so
// Join's empty-part-skipping logic never runs.
//
// The fix is resolveExpr's third return value, degenerateRoot: an explicit
// out-of-band bool reported alongside the (unmodified) pattern text,
// rather than an in-band sentinel folded into pattern itself. A prior
// round tried the sentinel (a NUL-delimited degenerateRootMarker prefix,
// stripped again before a Site.Pattern was ever rendered) and a second
// review round found it did not compose: filepath.Join joins already-
// resolved parts with "/", and filepath.Dir/Base wrap the inner resolution
// in "dirname(...)"/"basename(...)" — neither preserves a marker at the
// pattern's own prefix, so nesting a degenerate concat inside either
// leaked the raw marker bytes into the rendered inventory AND silently
// lost the degeneracy signal at the same time (see
// TestJoinNonFirstArgDegenerateConcatNeverLeaksOrElsewhere and
// TestDirWrappingDegenerateConcatNeverLeaksOrElsewhere below). Reporting
// degeneracy out-of-band sidesteps the composition problem entirely: there
// is no marker text for any wrapper to fail to preserve.
func TestConcatEmptyBaseNeverElsewhere(t *testing.T) {
	src := `package p
func f() {
	base := ""
	os.WriteFile(base+"/observations.jsonl", nil, 0644)
}`
	argExpr, defs, r := parseCallArg(t, src)
	pattern, resolved, degenerate := r.resolveExpr(argExpr, defs, 0)

	if pattern != "/observations.jsonl" {
		t.Errorf("pattern = %q, want %q — the pattern TEXT is never touched; degeneracy is reported out-of-band", pattern, "/observations.jsonl")
	}
	if !resolved {
		t.Errorf("resolved = false, want true — both the base and the literal resolved successfully")
	}
	if !degenerate {
		t.Errorf("degenerate = false, want true — this concatenation's root is an artifact of an empty-resolved base, not a positively-established absolute path")
	}
	if got := classify(pattern, resolved, degenerate); got == "elsewhere" {
		t.Errorf("classify(%q, %v, %v) = %q, want anything but \"elsewhere\" — the leading slash came from concatenating a literal onto an empty-resolved base, not from a genuine absolute-path literal", pattern, resolved, degenerate, got)
	}
}

// TestJoinNonFirstArgDegenerateConcatNeverLeaksOrElsewhere reproduces the
// round-3 CI review's confirmed leak: a degenerate concatenation
// (base+"/observations.jsonl" with an empty base) passed as a NON-FIRST
// argument to filepath.Join, alongside a real anchor (workspaceRoot,
// which resolves to the <WorkspaceRoot> anchor via identifierAnchors).
// With the old in-band marker, strings.Join(parts, "/") spliced the
// marker's raw NUL bytes into the middle of the resulting pattern text —
// corrupting anything that persisted Site.Pattern verbatim (the golden
// JSON/markdown) — while ALSO losing the degeneracy signal entirely,
// because the marker was no longer at the pattern's own prefix for
// isOpaqueRoot's HasPrefix check to find, so classify fell through to
// "elsewhere" even though the tail segment was never a positively
// established path fragment.
//
// With degeneracy carried out-of-band there is no marker text to leak,
// and filepath.Join's degenerateRoot is true whenever ANY kept argument
// was flagged degenerate — not only the one occupying the joined
// pattern's own first segment — so a degenerate fragment buried in the
// tail still keeps classify from confirming "elsewhere", the same
// under-claiming posture hasOpaqueSegment already applies to an ordinary
// text-opaque tail segment on a <WorkspaceRoot>-rooted pattern.
func TestJoinNonFirstArgDegenerateConcatNeverLeaksOrElsewhere(t *testing.T) {
	src := `package p
func f(workspaceRoot string) {
	base := ""
	os.WriteFile(filepath.Join(workspaceRoot, base+"/observations.jsonl"), nil, 0644)
}`
	argExpr, defs, r := parseCallArg(t, src)
	pattern, resolved, degenerate := r.resolveExpr(argExpr, defs, 0)

	if strings.ContainsRune(pattern, 0) {
		t.Errorf("pattern = %q contains a NUL byte — a marker leaked into rendered text", pattern)
	}
	if !resolved {
		t.Errorf("resolved = false, want true")
	}
	if !degenerate {
		t.Errorf("degenerate = false, want true — the second Join argument is a degenerate concat even though it isn't the pattern's own first segment")
	}
	if got := classify(pattern, resolved, degenerate); got == "elsewhere" {
		t.Errorf("classify(%q, %v, %v) = %q, want anything but \"elsewhere\" — the tail segment was never a positively established path fragment", pattern, resolved, degenerate, got)
	}
	if got := classify(pattern, resolved, degenerate); got != "unanchored" {
		t.Errorf("classify(%q, %v, %v) = %q, want %q", pattern, resolved, degenerate, got, "unanchored")
	}
}

// TestDirWrappingDegenerateConcatNeverLeaksOrElsewhere reproduces the
// round-3 CI review's other confirmed leak: filepath.Dir wrapping a
// degenerate concatenation as its ONLY argument. The old in-band marker
// ended up as "dirname(\x00degenerate-root\x00/observations.jsonl)" —
// leaking raw marker bytes into rendered text, and losing the degeneracy
// signal because the wrapped pattern no longer started with the marker.
//
// Here the concat IS the whole inner expression, so dirname(...)'s own
// root is exactly as degenerate as its inner resolution — the fix
// threads that through explicitly (see resolveCall's filepath.Dir case).
func TestDirWrappingDegenerateConcatNeverLeaksOrElsewhere(t *testing.T) {
	src := `package p
func f() {
	base := ""
	os.MkdirAll(filepath.Dir(base+"/x"), 0755)
}`
	argExpr, defs, r := parseCallArg(t, src)
	pattern, resolved, degenerate := r.resolveExpr(argExpr, defs, 0)

	if strings.ContainsRune(pattern, 0) {
		t.Errorf("pattern = %q contains a NUL byte — a marker leaked into rendered text", pattern)
	}
	if pattern != "dirname(/x)" {
		t.Errorf("pattern = %q, want %q", pattern, "dirname(/x)")
	}
	if !resolved {
		t.Errorf("resolved = false, want true")
	}
	if !degenerate {
		t.Errorf("degenerate = false, want true — filepath.Dir wrapping a degenerate concat inherits its inner expression's degeneracy")
	}
	if got := classify(pattern, resolved, degenerate); got == "elsewhere" {
		t.Errorf("classify(%q, %v, %v) = %q, want anything but \"elsewhere\" — dirname(...) wrapping a degenerate root has not positively established a destination outside .cog/", pattern, resolved, degenerate, got)
	}
	if got := classify(pattern, resolved, degenerate); got != "unanchored" {
		t.Errorf("classify(%q, %v, %v) = %q, want %q", pattern, resolved, degenerate, got, "unanchored")
	}
}

// TestConcatOfDegenerateConcatNeverElsewhere reproduces the round-4 CI
// review's confirmed leak: a SECOND string concatenation appending onto an
// already-degenerate concat's RESULT, reached through an intermediate local
// (`p := base + "/x"` then `p + ".tmp"`), rather than a compound assign.
// Before the fix, the BinaryExpr case discarded both operands' own
// degenerateRoot with `_` and recomputed the flag from scratch as
// `left == "" && strings.HasPrefix(right, "/")` — which finds nothing here,
// because by the time `p` is resolved its own text is already
// "/x" (non-empty), even though that text's root was never positively
// established. The fix threads `p`'s own degenerateRoot through as
// leftDeg instead of discarding it.
func TestConcatOfDegenerateConcatNeverElsewhere(t *testing.T) {
	src := `package p
func f() {
	base := ""
	p := base + "/x"
	os.WriteFile(p+".tmp", nil, 0644)
}`
	argExpr, defs, r := parseCallArg(t, src)
	pattern, resolved, degenerate := r.resolveExpr(argExpr, defs, 0)

	if pattern != "/x.tmp" {
		t.Errorf("pattern = %q, want %q", pattern, "/x.tmp")
	}
	if !resolved {
		t.Errorf("resolved = false, want true")
	}
	if !degenerate {
		t.Errorf("degenerate = false, want true — %q's own root was never positively established, and concatenating onto it must not launder that away", "p")
	}
	if got := classify(pattern, resolved, degenerate); got == "elsewhere" {
		t.Errorf("classify(%q, %v, %v) = %q, want anything but \"elsewhere\"", pattern, resolved, degenerate, got)
	}
}

// TestCompoundAssignOntoDegenerateConcatNeverElsewhere reproduces the
// round-4 CI review's other confirmed leak, and the exact idiom isOpaqueRoot's
// own doc comment already cites as real (`memPath += ".md"`): a compound
// assign folds onto an EXISTING degenerate concat via applyAssign's
// synthetic `BinaryExpr{X: prev, Op: ADD, Y: rhs}`, so the outer BinaryExpr's
// own left operand is itself a BinaryExpr (not empty-string text) by the
// time resolveExpr reaches it — the identical shape
// TestConcatOfDegenerateConcatNeverElsewhere exercises via an intermediate
// local instead of a compound assign, confirming the fix covers both
// syntactic routes to the same synthetic-BinaryExpr shape.
func TestCompoundAssignOntoDegenerateConcatNeverElsewhere(t *testing.T) {
	src := `package p
func f() {
	base := ""
	p := base + "/observations.jsonl"
	p += ".md"
	os.WriteFile(p, nil, 0644)
}`
	argExpr, defs, r := parseCallArg(t, src)
	pattern, resolved, degenerate := r.resolveExpr(argExpr, defs, 0)

	if pattern != "/observations.jsonl.md" {
		t.Errorf("pattern = %q, want %q", pattern, "/observations.jsonl.md")
	}
	if !resolved {
		t.Errorf("resolved = false, want true")
	}
	if !degenerate {
		t.Errorf("degenerate = false, want true — the compound assign's synthetic BinaryExpr must not lose the inner concat's own degeneracy")
	}
	if got := classify(pattern, resolved, degenerate); got == "elsewhere" {
		t.Errorf("classify(%q, %v, %v) = %q, want anything but \"elsewhere\"", pattern, resolved, degenerate, got)
	}
}

// TestConcatOntoDegenerateDirNeverElsewhere reproduces the round-4 CI
// review's third confirmed laundering shape: a string concatenation
// appending a further literal segment onto a filepath.Dir call that itself
// wraps a degenerate concat — `filepath.Dir(base+"/observations.jsonl")` is
// degenerate (the Dir case inherits its inner expression's flag), and
// appending "/y.json" onto that result must not clear it, even though the
// Dir call's own resolved text ("dirname(/observations.jsonl)") is
// non-empty and carries no leading "/" for the BinaryExpr case's own
// left=="" test to catch.
func TestConcatOntoDegenerateDirNeverElsewhere(t *testing.T) {
	src := `package p
func f() {
	base := ""
	os.WriteFile(filepath.Dir(base+"/observations.jsonl")+"/y.json", nil, 0644)
}`
	argExpr, defs, r := parseCallArg(t, src)
	pattern, resolved, degenerate := r.resolveExpr(argExpr, defs, 0)

	if pattern != "dirname(/observations.jsonl)/y.json" {
		t.Errorf("pattern = %q, want %q", pattern, "dirname(/observations.jsonl)/y.json")
	}
	if !resolved {
		t.Errorf("resolved = false, want true")
	}
	if !degenerate {
		t.Errorf("degenerate = false, want true — dirname(...)'s own inherited degeneracy must survive a further concat appended onto its result")
	}
	if got := classify(pattern, resolved, degenerate); got == "elsewhere" {
		t.Errorf("classify(%q, %v, %v) = %q, want anything but \"elsewhere\"", pattern, resolved, degenerate, got)
	}
}

// TestConditionalPlainRebindPoisonsToUnanchored reproduces the round-5 CI
// review's confirmed finding at audit.go:587 verbatim: poisonLoopRebinds
// (now poisonConditionalAndLoopRebinds) only un-trusted a plain `=` rebind
// inside a FOR/RANGE body, so applyDirectLocalDefs's identical flat walk
// let an if-branch's plain rebind permanently overwrite the outer
// filepath.Join binding, and the write resolved on that overwritten value
// alone — turning a real, conditional .cog/ write into a false "elsewhere"
// for the branch where the rebind does NOT fire. Must land on
// "unanchored", never "elsewhere": the site cannot prove either branch
// alone from outside the `if`.
func TestConditionalPlainRebindPoisonsToUnanchored(t *testing.T) {
	src := `package p
func Save(useCog bool, workspaceRoot string) error {
	path := filepath.Join(workspaceRoot, ".cog", "settings.json")
	if !useCog {
		path = "/etc/cogos/global-settings.json"
	}
	return os.WriteFile(path, nil, 0644)
}`
	argExpr, defs, r := parseCallArg(t, src)
	pattern, resolved, degenerate := r.resolveExpr(argExpr, defs, 0)
	if got := classify(pattern, resolved, degenerate); got != "unanchored" {
		t.Errorf("classify(%q, %v, %v) = %q, want %q — the if-branch's plain rebind to an unrelated, non-.cog literal must not let the outer .cog/settings.json join stand uncontested, and must especially never read as \"elsewhere\"", pattern, resolved, degenerate, got, "unanchored")
	}
}

// TestConditionalElseBranchPlainRebindPoisons is the same shape as
// TestConditionalPlainRebindPoisonsToUnanchored, but the rebind sits in the
// ELSE arm rather than the sole `if` arm — poisonConditionalAndLoopRebinds
// must poison an else block exactly like an if body (see that function's
// IfStmt case).
func TestConditionalElseBranchPlainRebindPoisons(t *testing.T) {
	src := `package p
func f(useCog bool, workspaceRoot string) error {
	path := filepath.Join(workspaceRoot, ".cog", "settings.json")
	if useCog {
		_ = useCog
	} else {
		path = "/etc/cogos/global-settings.json"
	}
	return os.WriteFile(path, nil, 0644)
}`
	argExpr, defs, r := parseCallArg(t, src)
	pattern, resolved, degenerate := r.resolveExpr(argExpr, defs, 0)
	if got := classify(pattern, resolved, degenerate); got != "unanchored" {
		t.Errorf("classify(%q, %v, %v) = %q, want %q — a plain rebind in the ELSE arm is exactly as untrustworthy from outside the if/else as one in the IF arm", pattern, resolved, degenerate, got, "unanchored")
	}
}

// TestConditionalSwitchCasePlainRebindPoisons extends the same coverage to
// SwitchStmt case bodies — poisonConditionalAndLoopRebinds's SwitchStmt
// case must poison a case body's plain rebind the same way its IfStmt case
// does.
func TestConditionalSwitchCasePlainRebindPoisons(t *testing.T) {
	src := `package p
func f(mode int, workspaceRoot string) error {
	path := filepath.Join(workspaceRoot, ".cog", "settings.json")
	switch mode {
	case 1:
		path = "/etc/cogos/global-settings.json"
	}
	return os.WriteFile(path, nil, 0644)
}`
	argExpr, defs, r := parseCallArg(t, src)
	pattern, resolved, degenerate := r.resolveExpr(argExpr, defs, 0)
	if got := classify(pattern, resolved, degenerate); got != "unanchored" {
		t.Errorf("classify(%q, %v, %v) = %q, want %q — a switch-case's plain rebind must poison exactly like an if-branch's", pattern, resolved, degenerate, got, "unanchored")
	}
}

// TestConditionalShadowDeclarationDoesNotPoisonOuter is the required
// control for `:=`: a `:=` inside a conditional body declares a NEW name
// shadowing the outer `path`, never rebinding it, so the outer binding
// must resolve exactly as if the shadow never existed — never poisoned,
// never "unanchored".
func TestConditionalShadowDeclarationDoesNotPoisonOuter(t *testing.T) {
	src := `package p
func f(useCog bool, workspaceRoot string) error {
	path := filepath.Join(workspaceRoot, ".cog", "settings.json")
	if useCog {
		path := "/etc/cogos/global-settings.json"
		_ = path
	}
	return os.WriteFile(path, nil, 0644)
}`
	argExpr, defs, r := parseCallArg(t, src)
	pattern, resolved, degenerate := r.resolveExpr(argExpr, defs, 0)
	if got := classify(pattern, resolved, degenerate); got != "cog" {
		t.Errorf("classify(%q, %v, %v) = %q, want %q — a `:=` inside the if-block shadows `path` locally and must not clobber or poison the OUTER binding read by os.WriteFile", pattern, resolved, degenerate, got, "cog")
	}
	if !strings.Contains(pattern, ".cog/settings.json") {
		t.Errorf("pattern = %q, want it to still contain the outer join's \".cog/settings.json\" — the shadow's own value must never leak into the outer resolution either", pattern)
	}
}

// TestStraightLineRebindStillLastAssignmentWins is the required control at
// the opposite end: a plain `=` rebind that is NOT inside any conditional
// or loop must be completely unaffected by this fix — last-assignment-wins
// stays the rule outside a conditionally-executed subtree.
func TestStraightLineRebindStillLastAssignmentWins(t *testing.T) {
	src := `package p
func f(workspaceRoot string) error {
	path := "/tmp/placeholder"
	path = filepath.Join(workspaceRoot, ".cog", "settings.json")
	return os.WriteFile(path, nil, 0644)
}`
	argExpr, defs, r := parseCallArg(t, src)
	pattern, resolved, degenerate := r.resolveExpr(argExpr, defs, 0)
	if got := classify(pattern, resolved, degenerate); got != "cog" {
		t.Errorf("classify(%q, %v, %v) = %q, want %q — a straight-line rebind outside any conditional must still resolve on the LAST assignment, exactly as before this fix", pattern, resolved, degenerate, got, "cog")
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

// TestCallBoundaryDegenerateArgNeverElsewhere pins the substitution
// boundary in substituteAndMergeDefs: a degenerate argument must never
// cross into a callee as a positively-resolved literal and come back
// classified "elsewhere". Adversarial verification of the within-scope
// threading found four laundering shapes here; the fix declines the
// substitution, so each falls to the callee parameter's opaque
// placeholder ("unanchored"). The controls pin the two properties the
// fix must not break: a genuine absolute literal through the same callee
// still classifies "elsewhere", and degeneracy built INSIDE the callee
// from a non-degenerate argument still propagates out via chaseReturns.
// resolve_test.go's parseCallArg harness builds an empty global index and
// cannot reach this path, so these go through buildGlobalIndex +
// scanParsedFile like a real scan.
func TestCallBoundaryDegenerateArgNeverElsewhere(t *testing.T) {
	tests := []struct {
		name         string
		src          string
		wantCategory string
	}{
		{
			name: "concat-wrapping callee",
			src: `package p
func wrap(q string) string { return q + ".tmp" }
func f() {
	base := ""
	os.WriteFile(wrap(base+"/observations.jsonl"), nil, 0644)
}`,
			wantCategory: "unanchored",
		},
		{
			name: "verbatim-return callee",
			src: `package p
func ident(q string) string { return q }
func f() {
	base := ""
	os.WriteFile(ident(base+"/observations.jsonl"), nil, 0644)
}`,
			wantCategory: "unanchored",
		},
		{
			name: "join-wrapping callee",
			src: `package p
func under(q string) string { return filepath.Join(q, "y") }
func f() {
	base := ""
	os.WriteFile(under(base+"/observations.jsonl"), nil, 0644)
}`,
			wantCategory: "unanchored",
		},
		{
			name: "method callee via methodChase",
			src: `package p
type K struct{}
func (k K) Wrap(q string) string { return q + ".tmp" }
type P struct{ k K }
func (p P) f() {
	base := ""
	os.WriteFile(p.k.Wrap(base+"/observations.jsonl"), nil, 0644)
}`,
			wantCategory: "unanchored",
		},
		{
			name: "control: absolute literal through the same callee stays elsewhere",
			src: `package p
func wrap(q string) string { return q + ".tmp" }
func f() {
	os.WriteFile(wrap("/etc/cogos/config.yaml"), nil, 0644)
}`,
			wantCategory: "elsewhere",
		},
		{
			name: "control: degeneracy built inside the callee still propagates out",
			src: `package p
func mk(b string) string { return b + "/observations.jsonl" }
func f() {
	base := ""
	os.WriteFile(mk(base), nil, 0644)
}`,
			wantCategory: "unanchored",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "snippet.go", tt.src, 0)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			pf := parsedFile{relPath: "snippet.go", dir: "pkg", fset: fset, file: f, scopes: collectFuncScopes(f, fset)}
			idx := buildGlobalIndex([]parsedFile{pf})
			sites := scanParsedFile(pf, idx)
			if len(sites) == 0 {
				t.Fatalf("no write site found in snippet")
			}
			got := sites[len(sites)-1]
			if got.Category != tt.wantCategory {
				t.Errorf("category = %q (pattern %q), want %q", got.Category, got.Pattern, tt.wantCategory)
			}
			if strings.ContainsRune(got.Pattern, '\x00') {
				t.Errorf("pattern %q contains a NUL byte", got.Pattern)
			}
		})
	}
}

// ─── Round-5 gate: sibling-branch and shadow-visibility scoping ───────────
//
// The round-4 fix's `:=`-shadow guard in applyAssign is a bare EXISTENCE
// test (`if _, hadOuter := defs[ident.Name]; hadOuter { continue }`) over a
// single whole-function map. That test cannot tell "this is a genuine outer
// binding from before the conditional" apart from "this is a SIBLING
// branch's own `:=` that merely happened to be folded first by the AST walk"
// — both look identical to a bare existence check. The fix is
// localDefsFor/scopeChain: a position strictly inside a given conditional
// body resolves by folding THAT body's own level after its enclosing ones,
// regardless of what a sibling branch or the outer scope recorded for the
// same name. These tests use scanParsedFile directly (the
// real per-call-site production path, backed by localDefsFor) rather than
// parseCallArg's single-match flat collectLocalDefs, specifically because
// each case has MORE THAN ONE matched write call and the whole point is
// that each one must resolve independently.

// siteAtLine runs the same scanning path scanParsedFile/Scan use in
// production (localDefsFor's position-scoped scope-chain fold, NOT
// parseCallArg's whole-function, non-position-aware collectLocalDefs) and returns the
// matched write site at wantLine, so a snippet with more than one write
// call — one per sibling branch — can be asserted on independently.
func siteAtLine(t *testing.T, src string, wantLine int) Site {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pf := parsedFile{relPath: "snippet.go", dir: "pkg", fset: fset, file: f, scopes: collectFuncScopes(f, fset)}
	idx := buildGlobalIndex([]parsedFile{pf})
	sites := scanParsedFile(pf, idx)
	for _, s := range sites {
		if s.Line == wantLine {
			return s
		}
	}
	var lines []int
	for _, s := range sites {
		lines = append(lines, s.Line)
	}
	t.Fatalf("no matched writer call at line %d (found lines: %v)", wantLine, lines)
	return Site{}
}

// TestSiblingIfBranchesResolveOwnValuesIndependently is round-5 finding #1:
// two SIBLING `if`/`if` branches (not else-if — deliberately two separate
// IfStmts, the exact reproduction shape) each declare the same name `p` via
// `:=` with a DIFFERENT value and write it. Before this fix, the SECOND
// branch's `:=` saw the FIRST branch's `p` already sitting in the shared map
// (hadOuter=true, even though it is a SIBLING, not an enclosing scope) and
// was treated as a shadow — recorded nowhere — so the second branch's own
// write silently resolved against the FIRST branch's value.
func TestSiblingIfBranchesResolveOwnValuesIndependently(t *testing.T) {
	src := `package p
func f(a bool, root string) {
	if a {
		p := "/etc/cogos/outer.json"
		os.WriteFile(p, nil, 0644)
	}
	if !a {
		p := filepath.Join(root, ".cog", "inner.json")
		os.WriteFile(p, nil, 0644)
	}
}`
	branch1 := siteAtLine(t, src, 5)
	if branch1.Category != "elsewhere" || !strings.Contains(branch1.Pattern, "outer.json") {
		t.Errorf("branch 1 (line 5) = category %q pattern %q, want elsewhere / outer.json — its own := must resolve on ITS OWN value", branch1.Category, branch1.Pattern)
	}
	branch2 := siteAtLine(t, src, 9)
	if branch2.Category != "cog" || !strings.Contains(branch2.Pattern, ".cog/inner.json") {
		t.Errorf("branch 2 (line 9) = category %q pattern %q, want cog / .cog/inner.json — the SECOND sibling branch's own := must not silently vanish behind the FIRST branch's := the way a bare hadOuter existence test collapses it", branch2.Category, branch2.Pattern)
	}
}

// TestSwitchCaseSiblingBranchesResolveOwnValuesIndependently is the same
// shape as TestSiblingIfBranchesResolveOwnValuesIndependently, but for two
// SwitchStmt case bodies — the verifier's other named sibling shape.
func TestSwitchCaseSiblingBranchesResolveOwnValuesIndependently(t *testing.T) {
	src := `package p
func f(mode int, root string) {
	switch mode {
	case 1:
		p := "/etc/cogos/outer.json"
		os.WriteFile(p, nil, 0644)
	case 2:
		p := filepath.Join(root, ".cog", "inner.json")
		os.WriteFile(p, nil, 0644)
	}
}`
	case1 := siteAtLine(t, src, 6)
	if case1.Category != "elsewhere" || !strings.Contains(case1.Pattern, "outer.json") {
		t.Errorf("case 1 (line 6) = category %q pattern %q, want elsewhere / outer.json", case1.Category, case1.Pattern)
	}
	case2 := siteAtLine(t, src, 9)
	if case2.Category != "cog" || !strings.Contains(case2.Pattern, ".cog/inner.json") {
		t.Errorf("case 2 (line 9) = category %q pattern %q, want cog / .cog/inner.json — a switch case's own := must resolve independently of the PREVIOUS case's own :=", case2.Category, case2.Pattern)
	}
}

// TestShadowDeclarationVisibleInsideItsOwnBranch is round-5 finding #2: the
// `:=`-shadow guard correctly protects a read OUTSIDE the conditional (see
// TestConditionalShadowDeclarationDoesNotPoisonOuter), but as a side effect
// of never recording the shadow's own value anywhere, it ALSO broke a read
// INSIDE the same branch that declared the shadow — that read saw the
// OUTER value instead of the shadow's own, genuinely different, value.
func TestShadowDeclarationVisibleInsideItsOwnBranch(t *testing.T) {
	src := `package p
func f(a bool, root string) {
	p := "/etc/cogos/outer.json"
	if a {
		p := filepath.Join(root, ".cog", "inner.json")
		os.WriteFile(p, nil, 0644)
	}
	os.WriteFile(p, nil, 0644)
}`
	inside := siteAtLine(t, src, 6)
	if inside.Category != "cog" || !strings.Contains(inside.Pattern, ".cog/inner.json") {
		t.Errorf("inside the if-block (line 6) = category %q pattern %q, want cog / .cog/inner.json — a read of `p` INSIDE the very branch that shadows it must see the SHADOW's own value, not the outer one", inside.Category, inside.Pattern)
	}
	outside := siteAtLine(t, src, 8)
	if outside.Category != "elsewhere" || !strings.Contains(outside.Pattern, "outer.json") {
		t.Errorf("outside the if-block (line 8) = category %q pattern %q, want elsewhere / outer.json — the required control: the outer read must still be completely unaffected by the shadow", outside.Category, outside.Pattern)
	}
}

// TestValueSpecShadowGatedLikeDefine is round-5 finding #3: applyAssign's
// `:=` DEFINE case is gated on shadowing an outer name (record unless it
// would clobber one), but the *ast.ValueSpec (`var x = expr`) arm was
// gated on condDepth == 0 instead — unconditionally dropping EVERY `var`
// declared inside any conditional, shadow or not. This mirrors both halves
// of TestShadowDeclarationVisibleInsideItsOwnBranch and
// TestConditionalShadowDeclarationDoesNotPoisonOuter for the `var` form:
// the outer read must stay protected, AND a read inside the branch that
// declares the `var` shadow must see the shadow's own value — neither of
// which held before this fix (the outer read never SAW the shadow, since
// nothing at condDepth>0 was ever recorded — the inside read hit the same
// gap; only the OUTSIDE half happened to look correct by accident, because
// there was never anything at condDepth>0 not being poisoned in the first
// place, but that accident is exactly what made the total drop of the
// inside value invisible to a golden-diff comparison).
func TestValueSpecShadowGatedLikeDefine(t *testing.T) {
	src := `package p
func f(a bool, root string) {
	var p = "/etc/cogos/outer.json"
	if a {
		var p = filepath.Join(root, ".cog", "inner.json")
		os.WriteFile(p, nil, 0644)
	}
	os.WriteFile(p, nil, 0644)
}`
	inside := siteAtLine(t, src, 6)
	if inside.Category != "cog" || !strings.Contains(inside.Pattern, ".cog/inner.json") {
		t.Errorf("inside the if-block (line 6) = category %q pattern %q, want cog / .cog/inner.json — a `var` shadow must be visible to a read inside its own branch, exactly like a `:=` shadow", inside.Category, inside.Pattern)
	}
	outside := siteAtLine(t, src, 8)
	if outside.Category != "elsewhere" || !strings.Contains(outside.Pattern, "outer.json") {
		t.Errorf("outside the if-block (line 8) = category %q pattern %q, want elsewhere / outer.json — the outer `var` must stay protected from the branch's own `var` shadow", outside.Category, outside.Pattern)
	}
}

// TestConditionalJoinWrappedRebindPoisonsToUnanchored is round-5 finding
// #4's confirmed reproduction, restated one syntactic step from the
// round-4 gate's own literal-only case: the rebind is wrapped in
// filepath.Join over plain literals instead of spelled as one bare
// literal. staticPathText (literalPathText's round-5 replacement) must
// still catch this and poison it, exactly as the bare-literal case already
// does.
func TestConditionalJoinWrappedRebindPoisonsToUnanchored(t *testing.T) {
	src := `package p
func f(useCog bool, workspaceRoot string) error {
	path := filepath.Join(workspaceRoot, ".cog", "settings.json")
	if !useCog {
		path = filepath.Join("/etc", "cogos", "g.json")
	}
	return os.WriteFile(path, nil, 0644)
}`
	argExpr, defs, r := parseCallArg(t, src)
	pattern, resolved, degenerate := r.resolveExpr(argExpr, defs, 0)
	if got := classify(pattern, resolved, degenerate); got != "unanchored" {
		t.Errorf("classify(%q, %v, %v) = %q, want %q — a filepath.Join-wrapped conditional rebind to a non-.cog root must poison exactly like a bare-literal one", pattern, resolved, degenerate, got, "unanchored")
	}
}

// TestConditionalRebindThroughLocalChasePoisons extends the same coverage
// to a rebind whose root is reached through ONE already-known local in the
// SAME function (the manifest.go RunsRoot shape: `home := os.UserHomeDir();
// ...; root = filepath.Join(home, "workspaces")`) rather than an inline
// literal — staticPathText's Ident-through-defs chase is what closes this,
// distinct from (and in addition to) the plain filepath.Join-of-literals
// case above.
func TestConditionalRebindThroughLocalChasePoisons(t *testing.T) {
	src := `package p
func f(useEnv bool, envRoot string) (string, error) {
	root := envRoot
	if !useEnv {
		home, _ := os.UserHomeDir()
		root = filepath.Join(home, "workspaces")
	}
	return root, nil
}
func g(useEnv bool, envRoot string) error {
	root, _ := f(useEnv, envRoot)
	return os.MkdirAll(filepath.Join(root, "first-instruments-runs"), 0755)
}`
	argExpr, defs, r := parseCallArg(t, src)
	pattern, resolved, degenerate := r.resolveExpr(argExpr, defs, 0)
	got := classify(pattern, resolved, degenerate)
	if got == "home" {
		t.Errorf("classify(%q, %v, %v) = %q — a conditional rebind chased through one local (home := os.UserHomeDir()) must not let the env-unset branch alone stamp a confident \"home\"", pattern, resolved, degenerate, got)
	}
}

// TestConditionalCogBearingRebindStaysTrusted pins down the DELIBERATE
// asymmetry round-5 finding #5 flagged: a branch rebind this tool can read
// as .cog-ROOTED is trusted (never poisoned) even though the branch might
// not execute, while a branch rebind to a provably non-.cog root IS
// poisoned (see TestConditionalPlainRebindPoisonsToUnanchored and its
// siblings above). This is the SAME over-claim-toward-.cog-is-the-safe-
// direction posture classify()'s own doc comment states for every other
// ambiguous case in this package, applied here too — see
// poisonConditionalAndLoopRebinds's doc comment for the full reasoning and
// for why making this symmetric would cost real .cog/ writers (log_capture.
// go's shape) more than it would gain. This test exists so a future change
// to this balance is a conscious, gate-visible decision, never an
// accidental one.
func TestConditionalCogBearingRebindStaysTrusted(t *testing.T) {
	src := `package p
func f(useCog bool, elsewhereRoot string) error {
	path := filepath.Join(elsewhereRoot, "settings.json")
	if useCog {
		path = "/var/lib/x/.cog/settings.json"
	}
	return os.WriteFile(path, nil, 0644)
}`
	argExpr, defs, r := parseCallArg(t, src)
	pattern, resolved, degenerate := r.resolveExpr(argExpr, defs, 0)
	if got := classify(pattern, resolved, degenerate); got != "cog" {
		t.Errorf("classify(%q, %v, %v) = %q, want %q — a conditional rebind THIS tool can read as .cog-rooted is deliberately never poisoned, even though the branch might not execute at runtime (the accepted, documented asymmetry — see poisonConditionalAndLoopRebinds's doc comment)", pattern, resolved, degenerate, got, "cog")
	}
}

// ─── Round-6 gate: one fold rule for every scope level ────────────────────
//
// The round-5 fix layered each conditional body's own defs back on top of
// the whole-function map for positions inside it, but the poison pass that
// decides which rebinds a site may trust ran ONLY over the whole-function
// fold. Every overlay level was therefore free to re-introduce exactly the
// rebinds the poison had just removed, and a labeled loop dropped out of the
// chain entirely. The fix is structural: localDefsFor folds the function
// body and each level of the site's scope chain through ONE function
// (foldScopeLevel = fold-then-poison), scoped to the statements that precede
// the site. These tests pin the shapes that fix is answerable for, all
// through siteAtLine — the production path. parseCallArg cannot see any of
// them: it calls collectLocalDefs directly and is structurally blind to the
// scope chain where every one of these defects lived.

// TestNestedConditionalRebindDoesNotReachOuterSite is round-6 finding #1: a
// conditional nested INSIDE the branch that holds the write. The inner
// branch's plain rebind to a provably non-.cog root was folded into the
// outer branch's overlay and never poisoned there, so the write — which the
// inner branch may never have touched — was stamped with the inner branch's
// value. The same defect with a loop as the outer scope is the if-inside-loop
// shape below; the sibling case/comm-clause spellings are the same shape
// again, one construct over.
func TestNestedConditionalRebindDoesNotReachOuterSite(t *testing.T) {
	tests := []struct {
		name string
		src  string
		line int
	}{
		{
			name: "if inside if",
			src: `package p
func f(root string, cond, other bool) error {
	if cond {
		path := filepath.Join(root, ".cog", "x.json")
		if other {
			path = "/etc/cogos/g.json"
		}
		return os.WriteFile(path, nil, 0644)
	}
	return nil
}`,
			line: 8,
		},
		{
			name: "if inside else-if",
			src: `package p
func f(root string, a, b bool) {
	if a {
		_ = a
	} else if b {
		path := filepath.Join(root, ".cog", "x.json")
		if a {
			path = "/etc/cogos/g.json"
		}
		os.WriteFile(path, nil, 0644)
	}
}`,
			line: 10,
		},
		{
			name: "if inside switch case",
			src: `package p
func f(root string, mode int, c bool) {
	switch mode {
	case 1:
		path := filepath.Join(root, ".cog", "x.json")
		if c {
			path = "/etc/cogos/g.json"
		}
		os.WriteFile(path, nil, 0644)
	}
}`,
			line: 9,
		},
		{
			name: "if inside type-switch case",
			src: `package p
func f(root string, v interface{}, c bool) {
	switch v.(type) {
	case int:
		path := filepath.Join(root, ".cog", "x.json")
		if c {
			path = "/etc/cogos/g.json"
		}
		os.WriteFile(path, nil, 0644)
	}
}`,
			line: 9,
		},
		{
			name: "if inside select comm clause",
			src: `package p
func f(root string, ch chan int, c bool) {
	select {
	case <-ch:
		path := filepath.Join(root, ".cog", "x.json")
		if c {
			path = "/etc/cogos/g.json"
		}
		os.WriteFile(path, nil, 0644)
	}
}`,
			line: 9,
		},
		{
			name: "loop inside if",
			src: `package p
func f(root string, cond bool, args []string) {
	if cond {
		path := filepath.Join(root, ".cog", "x.json")
		for _, a := range args {
			_ = a
			path = "/etc/cogos/g.json"
		}
		os.WriteFile(path, nil, 0644)
	}
}`,
			line: 9,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			site := siteAtLine(t, tt.src, tt.line)
			if site.Category != "unanchored" {
				t.Errorf("category = %q pattern = %q, want unanchored — the nested branch's rebind is not guaranteed on this write's path and must poison, not classify, it", site.Category, site.Pattern)
			}
		})
	}
}

// TestIfInsideLoopPoisonsAtLoopLevel is the pre-existing twin of the shape
// above, wrong at every earlier commit in this arc: the write sits at a loop
// body's own level with a conditional rebind nested in that loop body. Before
// the round-6 fix the loop-body overlay folded the conditional's rebind in
// unpoisoned; now the loop body is folded and poisoned like any other level.
func TestIfInsideLoopPoisonsAtLoopLevel(t *testing.T) {
	src := `package p
func f(root string, args []string, cond bool) {
	for _, a := range args {
		_ = a
		p := filepath.Join(root, ".cog", "inner.json")
		if cond {
			p = "/etc/cogos/g.json"
		}
		os.WriteFile(p, nil, 0644)
	}
}`
	site := siteAtLine(t, src, 9)
	if site.Category != "unanchored" {
		t.Errorf("category = %q pattern = %q, want unanchored", site.Category, site.Pattern)
	}
}

// TestLabeledLoopBodyStaysOnTheScopeChain is round-6 finding #2, and the
// worst-shaped one: a LABELED loop was not a scope the chain walk knew about
// (nestedScope had no *ast.LabeledStmt case), so a write inside it resolved
// from the enclosing function's map alone and picked up the same-named outer
// binding its loop-local `:=` shadows — a genuine .cog/ write stamped
// "elsewhere", the one failure mode this tool declares it must never produce.
// The unlabeled twin, which was always correct, is the control: a label must
// not change a classification.
func TestLabeledLoopBodyStaysOnTheScopeChain(t *testing.T) {
	labeled := `package p
func f(root string, args []string) {
	p := "/etc/cogos/outer.json"
Loop:
	for _, a := range args {
		_ = a
		p := filepath.Join(root, ".cog", "inner.json")
		os.WriteFile(p, nil, 0644)
		break Loop
	}
	_ = p
}`
	unlabeled := `package p
func f(root string, args []string) {
	p := "/etc/cogos/outer.json"
	for _, a := range args {
		_ = a
		p := filepath.Join(root, ".cog", "inner.json")
		os.WriteFile(p, nil, 0644)
		break
	}
	_ = p
}`
	withLabel := siteAtLine(t, labeled, 8)
	if withLabel.Category != "cog" || !strings.Contains(withLabel.Pattern, ".cog/inner.json") {
		t.Errorf("labeled loop = category %q pattern %q, want cog / .cog/inner.json — a real .cog write must never be stamped from an outer binding its own loop-local := shadows", withLabel.Category, withLabel.Pattern)
	}
	noLabel := siteAtLine(t, unlabeled, 7)
	if noLabel.Category != withLabel.Category || noLabel.Pattern != withLabel.Pattern {
		t.Errorf("labeled = %q/%q but unlabeled twin = %q/%q — a label must not change a classification", withLabel.Category, withLabel.Pattern, noLabel.Category, noLabel.Pattern)
	}
}

// TestLabeledScopesOtherThanLoops covers the rest of what a label can wrap:
// the same unwrapping has to work for a labeled block and a labeled switch,
// not just the labeled `for` the gate happened to reproduce with. The
// labeled switch was broken the same way the labeled loop was; the labeled
// block happened to come out right at af01c3c for an unrelated reason (a
// `:=` directly inside a bare block is at conditional depth zero, so it
// overwrote the outer binding in the whole-function map by accident) and is
// kept here as the control that says so.
func TestLabeledScopesOtherThanLoops(t *testing.T) {
	tests := []struct {
		name string
		src  string
		line int
	}{
		{
			name: "labeled block",
			src: `package p
func f(root string) {
	p := "/etc/cogos/outer.json"
Blk:
	{
		p := filepath.Join(root, ".cog", "inner.json")
		os.WriteFile(p, nil, 0644)
		break Blk
	}
	_ = p
}`,
			line: 7,
		},
		{
			name: "labeled switch case",
			src: `package p
func f(root string, mode int) {
	p := "/etc/cogos/outer.json"
Sw:
	switch mode {
	case 1:
		p := filepath.Join(root, ".cog", "inner.json")
		os.WriteFile(p, nil, 0644)
		break Sw
	}
	_ = p
}`,
			line: 8,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			site := siteAtLine(t, tt.src, tt.line)
			if site.Category != "cog" || !strings.Contains(site.Pattern, ".cog/inner.json") {
				t.Errorf("category = %q pattern = %q, want cog / .cog/inner.json", site.Category, site.Pattern)
			}
		})
	}
}

// TestRebindAfterTheWriteDoesNotClassifyIt pins the within-level half of the
// rule: last-assignment-wins is about assignments that PRECEDE the read.
// Folding a whole level regardless of position let a rebind written strictly
// AFTER the write site classify it — including a `:=` shadow declared after
// the write, which is not even the same variable. Each of these is a genuine
// .cog write that a following statement stamped "elsewhere".
func TestRebindAfterTheWriteDoesNotClassifyIt(t *testing.T) {
	tests := []struct {
		name string
		src  string
		line int
	}{
		{
			name: "plain rebind after the write",
			src: `package p
func f(root string) {
	p := filepath.Join(root, ".cog", "inner.json")
	os.WriteFile(p, nil, 0644)
	p = "/etc/cogos/g.json"
	_ = p
}`,
			line: 4,
		},
		{
			name: "conditional rebind after the write",
			src: `package p
func f(root string, c bool) {
	p := filepath.Join(root, ".cog", "inner.json")
	os.WriteFile(p, nil, 0644)
	if c {
		p = "/etc/cogos/g.json"
	}
	_ = p
}`,
			line: 4,
		},
		{
			name: "shadow declared after the write",
			src: `package p
func f(root string, c bool) {
	p := filepath.Join(root, ".cog", "inner.json")
	if c {
		os.WriteFile(p, nil, 0644)
		p := "/etc/cogos/g.json"
		_ = p
	}
}`,
			line: 5,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			site := siteAtLine(t, tt.src, tt.line)
			if site.Category != "cog" || !strings.Contains(site.Pattern, ".cog/inner.json") {
				t.Errorf("category = %q pattern = %q, want cog / .cog/inner.json — a statement that runs after this write cannot be what the write resolved", site.Category, site.Pattern)
			}
		})
	}
}

// TestRebindAfterTheWriteInsideLoopPoisons is the required counterweight to
// the test above: inside a loop body, a rebind that follows the write DOES
// precede it on the next iteration, so ignoring it would be an over-claim in
// the other direction. It poisons instead — the site falls to unanchored,
// never to the following statement's value.
func TestRebindAfterTheWriteInsideLoopPoisons(t *testing.T) {
	src := `package p
func f(root string, args []string) {
	for _, a := range args {
		_ = a
		p := filepath.Join(root, ".cog", "inner.json")
		os.WriteFile(p, nil, 0644)
		p = "/etc/cogos/g.json"
		_ = p
	}
}`
	site := siteAtLine(t, src, 6)
	if site.Category != "unanchored" {
		t.Errorf("category = %q pattern = %q, want unanchored — a loop back edge brings this write around again after the rebind, so neither value is guaranteed", site.Category, site.Pattern)
	}
}

// TestFuncLitRebindPoisonsEnclosingSite covers the last subtree kind that is
// never on an enclosing site's scope chain: a closure's body. The fold walks
// into it (a closure's plain `=` rebinds the captured outer variable), so
// without a matching poison a closure that may never have run could classify
// a write in the enclosing function.
func TestFuncLitRebindPoisonsEnclosingSite(t *testing.T) {
	src := `package p
func f(root string, run func(func())) {
	p := filepath.Join(root, ".cog", "inner.json")
	run(func() {
		p = "/etc/cogos/g.json"
	})
	os.WriteFile(p, nil, 0644)
}`
	site := siteAtLine(t, src, 7)
	if site.Category != "unanchored" {
		t.Errorf("category = %q pattern = %q, want unanchored — whether the closure ran before this write is not something this package models", site.Category, site.Pattern)
	}
}

// TestLocalDefsForCacheIsKeyedByPosition pins the cache contract the round-6
// gate found broken. The previous key was the innermost *ast.BlockStmt in the
// chain, and a switch/type-switch/select case body has no *ast.BlockStmt of
// its own — nestedScope synthesizes a fresh one per call — so every lookup
// for a site inside a case body missed and left a dead entry behind. The key
// is now the position itself, which is also the only correct key now that a
// level folds only up to the site: two sites in one case body legitimately
// get two different maps, and asking the cache for one position must never
// return the other's.
func TestLocalDefsForCacheIsKeyedByPosition(t *testing.T) {
	src := `package p
func f(root string, mode int) {
	switch mode {
	case 1:
		p := filepath.Join(root, ".cog", "first.json")
		os.WriteFile(p, nil, 0644)
		p = filepath.Join(root, ".cog", "second.json")
		os.WriteFile(p, nil, 0644)
	}
}`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "snippet.go", src, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	scopes := collectFuncScopes(file, fset)
	if len(scopes) == 0 {
		t.Fatal("no function scopes")
	}
	body := scopes[0].body

	var writes []*ast.CallExpr
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if _, _, matched := matchWriterCall(call, map[string]ast.Expr{}); matched {
			writes = append(writes, call)
		}
		return true
	})
	if len(writes) != 2 {
		t.Fatalf("expected 2 matched writer calls, got %d", len(writes))
	}

	cache := map[token.Pos]map[string]ast.Expr{}
	first := localDefsFor(cache, body, writes[0].Pos())
	second := localDefsFor(cache, body, writes[1].Pos())
	if len(cache) != 2 {
		t.Errorf("cache holds %d entries for 2 distinct positions, want 2 — a key that never matches (or one that matches too eagerly) is how the previous version both missed every lookup and claimed sharing it did not have", len(cache))
	}
	if again := localDefsFor(cache, body, writes[0].Pos()); len(cache) != 2 {
		t.Errorf("re-asking for the first position grew the cache to %d entries — the key does not match itself", len(cache))
		_ = again
	}
	firstText := exprString(first["p"])
	secondText := exprString(second["p"])
	if firstText == secondText {
		t.Errorf("both sites resolved p to %q — each write must see only the assignments that precede it", firstText)
	}
	if !strings.Contains(firstText, "first.json") {
		t.Errorf("first site's p = %q, want the first.json join", firstText)
	}
	if !strings.Contains(secondText, "second.json") {
		t.Errorf("second site's p = %q, want the second.json join", secondText)
	}
}

// ─── Round-7 gate: category agreement for rebinds staticPathText can't read ──
//
// The round-4/5/6 poison rule decided, BEFORE any resolution, whether a
// conditional plain `=` rebind disagreed with the outer binding — using
// staticPathText, which deliberately never leaves the current function body.
// Every rebind it could not read was therefore left in defs by
// last-assignment-wins and then resolved by the FULL interprocedural
// resolver, so the site classified from ONE branch's value with the outer
// binding gone. Five shapes reproduced it through the production siteAtLine
// path, each a one-substitution paraphrase of the round-4 finding's literal
// rebind: a declared function's return value, a package-level const, a
// filepath.Join over a declared-function call, a local bound to such a call,
// and the same shape one call-frame down inside a chased callee.
//
// The fix is resolveCondRebindPair: an unreadable conditional rebind no
// longer replaces the outer binding at all — the two are recorded as a pair
// and resolved together, with the FULL resolver, accepted only when they
// agree on classify() category. This is chaseReturns's idiom, applied to
// branches within one function rather than to a callee's return statements.
// These tests all go through siteAtLine (the production path); parseCallArg
// cannot see the difference, since the defect was never in the resolver's
// reach but in what the def map handed it.

// TestUnreadableConditionalRebindFallsToUnanchored is the five reproductions,
// pinned. Each has the SAME outer binding — a plainly .cog-rooted join — and
// a rebind that staticPathText cannot read but the interprocedural resolver
// can read as non-.cog. Before the fix every one of them stamped the site
// "elsewhere" with the sibling branch's path: the single failure this package
// declares never acceptable.
func TestUnreadableConditionalRebindFallsToUnanchored(t *testing.T) {
	tests := []struct {
		name string
		src  string
		line int
	}{
		{
			// h1: the round-4 finding's own shape with ONE substitution —
			// string literal replaced by a call to a declared function. This
			// is the shape internal/engine/log_capture.go:121-123 uses.
			name: "rebind to a declared function's return value",
			src: `package p
func Save(useCog bool, workspaceRoot string) error {
	path := filepath.Join(workspaceRoot, ".cog", "settings.json")
	if !useCog {
		path = globalSettingsPath()
	}
	return os.WriteFile(path, nil, 0644)
}
func globalSettingsPath() string { return "/etc/cogos/global-settings.json" }`,
			line: 7,
		},
		{
			// h2: a package-level const — resolveExpr's constDecl lookup
			// reads it, staticPathText (by design) does not.
			name: "rebind to a package-level const",
			src: `package p
const globalSettings = "/etc/cogos/global-settings.json"
func Save(useCog bool, workspaceRoot string) error {
	path := filepath.Join(workspaceRoot, ".cog", "settings.json")
	if !useCog {
		path = globalSettings
	}
	return os.WriteFile(path, nil, 0644)
}`,
			line: 8,
		},
		{
			// h5: the unreadable call wrapped in a filepath.Join, so the
			// round-5 widening (which DID teach staticPathText Join) still
			// cannot read it — one of its arguments is out of reach.
			name: "rebind to filepath.Join over a declared-function call",
			src: `package p
func Save(useCog bool, workspaceRoot string) error {
	path := filepath.Join(workspaceRoot, ".cog", "settings.json")
	if !useCog {
		path = filepath.Join(globalRoot(), "g.json")
	}
	return os.WriteFile(path, nil, 0644)
}
func globalRoot() string { return "/etc/cogos" }`,
			line: 7,
		},
		{
			// h13: one more hop — the rebind names a local that is itself
			// bound to the unreadable call.
			name: "rebind to a local bound to a declared-function call",
			src: `package p
func Save(useCog bool, workspaceRoot string) error {
	path := filepath.Join(workspaceRoot, ".cog", "settings.json")
	if !useCog {
		alt := globalSettingsPath()
		path = alt
	}
	return os.WriteFile(path, nil, 0644)
}
func globalSettingsPath() string { return "/etc/cogos/global-settings.json" }`,
			line: 8,
		},
		{
			// h15: the same shape inside a CALLEE whose return value is
			// chased. collectLocalDefs (the whole-function fold callChase
			// substitutes into) has to build the same pair, or the defect
			// simply moves one frame down.
			name: "the same shape inside a chased callee",
			src: `package p
func Save(useCog bool, workspaceRoot string) error {
	return os.WriteFile(pick(useCog, workspaceRoot), nil, 0644)
}
func pick(useCog bool, workspaceRoot string) string {
	path := filepath.Join(workspaceRoot, ".cog", "settings.json")
	if !useCog {
		path = globalSettingsPath()
	}
	return path
}
func globalSettingsPath() string { return "/etc/cogos/global-settings.json" }`,
			line: 3,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := siteAtLine(t, tc.src, tc.line)
			if s.Category == "elsewhere" || s.Category == "home" {
				t.Fatalf("category = %q pattern = %q — a genuine .cog/ write stamped with a sibling branch's non-.cog value is the one failure this package treats as never acceptable", s.Category, s.Pattern)
			}
			if s.Category != "unanchored" {
				t.Errorf("category = %q pattern = %q, want unanchored — the outer binding is .cog-rooted and the rebind is not, so neither may decide the site", s.Category, s.Pattern)
			}
		})
	}
}

// TestAgreeingConditionalRebindKeepsItsCategory is the required control on
// the other side of the same rule: when the outer binding and an unreadable
// conditional rebind resolve to DIFFERENT paths that land in the SAME
// category, the binding stands and the site keeps that category. Without
// this, "poison every unreadable conditional rebind" would look identical to
// the real fix on the five shapes above while silently discarding every
// two-branch .cog/ writer in the repo.
func TestAgreeingConditionalRebindKeepsItsCategory(t *testing.T) {
	src := `package p
func Save(useCog bool, workspaceRoot string) error {
	path := filepath.Join(workspaceRoot, ".cog", "settings.json")
	if !useCog {
		path = altCogPath(workspaceRoot)
	}
	return os.WriteFile(path, nil, 0644)
}
func altCogPath(workspaceRoot string) string { return filepath.Join(workspaceRoot, ".cog", "alt.json") }`
	s := siteAtLine(t, src, 7)
	if s.Category != "cog" {
		t.Errorf("category = %q pattern = %q, want cog — both branches write under .cog/, so which one runs cannot change the site's category and the binding must stand", s.Category, s.Pattern)
	}
}

// TestDisagreeingConditionalRebindFallsToUnanchored is the disagreement
// control stated in the categories that make the rule's shape clearest — one
// branch <Home>-rooted, the other a plain absolute literal — so the test does
// not read as a special case about ".cog" text.
func TestDisagreeingConditionalRebindFallsToUnanchored(t *testing.T) {
	src := `package p
func Save(useHome bool) error {
	path := homeConfigPath()
	if !useHome {
		path = etcConfigPath()
	}
	return os.WriteFile(path, nil, 0644)
}
func homeConfigPath() string { home, _ := os.UserHomeDir(); return filepath.Join(home, "cogos.json") }
func etcConfigPath() string  { return "/etc/cogos/cogos.json" }`
	s := siteAtLine(t, src, 7)
	if s.Category != "unanchored" {
		t.Errorf("category = %q pattern = %q, want unanchored — a home-rooted branch and an /etc-rooted branch are a real disagreement, not something to resolve by picking the later assignment", s.Category, s.Pattern)
	}
}

// TestUnreadableConditionalRebindWithNoOuterBindingIsUnchanged pins the
// boundary of the new rule: it only ever engages where last-assignment-wins
// actually DESTROYED something. A conditional rebind with no displaced outer
// binding (the name's first and only binding at this level) has no sibling
// value to disagree with, so it resolves exactly as it did before.
func TestUnreadableConditionalRebindWithNoOuterBindingIsUnchanged(t *testing.T) {
	src := `package p
func Save(useCog bool, workspaceRoot string) error {
	var path string
	if useCog {
		path = cogSettingsPath(workspaceRoot)
	}
	return os.WriteFile(path, nil, 0644)
}
func cogSettingsPath(workspaceRoot string) string { return filepath.Join(workspaceRoot, ".cog", "settings.json") }`
	s := siteAtLine(t, src, 7)
	if s.Category != "cog" {
		t.Errorf("category = %q pattern = %q, want cog — with nothing displaced there is no pair to disagree, and the pre-existing single-binding behavior must be untouched", s.Category, s.Pattern)
	}
}

// TestEveryBranchMustAgree pins the left-fold: THREE branches, two agreeing
// and one not. A pair built only from the outer binding and the LAST rebind
// would miss the middle branch entirely.
func TestEveryBranchMustAgree(t *testing.T) {
	src := `package p
func Save(mode int, workspaceRoot string) error {
	path := filepath.Join(workspaceRoot, ".cog", "a.json")
	switch mode {
	case 1:
		path = globalSettingsPath()
	case 2:
		path = cogSettingsPath(workspaceRoot)
	}
	return os.WriteFile(path, nil, 0644)
}
func cogSettingsPath(workspaceRoot string) string { return filepath.Join(workspaceRoot, ".cog", "settings.json") }
func globalSettingsPath() string                  { return "/etc/cogos/global-settings.json" }`
	s := siteAtLine(t, src, 10)
	if s.Category != "unanchored" {
		t.Errorf("category = %q pattern = %q, want unanchored — the case-1 branch disagrees with the other two, and a fold that only compared the outer binding against the LAST rebind would never see it", s.Category, s.Pattern)
	}
}

// TestConditionalRebindInsideItsOwnBranchStillSeesItsOwnValue is the
// scope-chain control the new rule must not disturb: a write INSIDE the
// branch that performs the rebind is on that branch's own execution path, so
// it resolves from the rebind alone — no pair, no disagreement. This is the
// shape internal/providers/selfupdate/spawn_unix.go:39 has (the MkdirAll sits
// inside the `if root != ""` branch, while the OpenFile at :43 sits after it
// and is genuinely two-destination).
func TestConditionalRebindInsideItsOwnBranchStillSeesItsOwnValue(t *testing.T) {
	src := `package p
func Save(root string) error {
	logPath := filepath.Join(os.TempDir(), "x.log")
	if root != "" {
		logPath = cogRunPath(root)
		if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
			return err
		}
	}
	return os.WriteFile(logPath, nil, 0644)
}
func cogRunPath(root string) string { return filepath.Join(root, ".cog", "run", "x.log") }`
	inside := siteAtLine(t, src, 6)
	if inside.Category != "cog" {
		t.Errorf("inside the branch (line 6) = category %q pattern %q, want cog — a site on the rebind's own path sees the rebind, and pairing must not reach it", inside.Category, inside.Pattern)
	}
	after := siteAtLine(t, src, 10)
	if after.Category != "unanchored" {
		t.Errorf("after the branch (line 10) = category %q pattern %q, want unanchored — a TempDir default and a .cog/run rebind are a real disagreement for a site outside the branch", after.Category, after.Pattern)
	}
}

// ─── Round-8 gate: the pair must compose upward, not just resolve at top ─────
//
// Round 7's pair reported a disagreement as reserved MARKER TEXT with
// degenerateRoot=false, and relied on isOpaqueRoot finding that text at the
// pattern's ROOT. Every round-7 test resolved the pair at the top level — the
// pair WAS the whole path argument — so the marker was always at the root and
// always found. Composed into a larger expression under a positively-known
// non-.cog root, the marker lands in the TAIL, where nothing looks for it, and
// the site laundered straight back into a confident "elsewhere": the exact
// leak commit 86bea26 removed for the degenerate-concat sentinel, reintroduced
// one commit later in a different guise.
//
// The fix unifies the two: a disagreement now returns the variable's ordinary
// opaque placeholder with degenerateRoot=TRUE, and degeneracy is the signal
// that composes — through filepath.Join in any argument position, string
// concatenation, filepath.Dir/Base, and across a call boundary (where
// substituteAndMergeDefs declines to substitute a degenerate argument at all).
// These tests compose the pair in every one of those directions.

// TestComposedDisagreementNeverLaundersToElsewhere is the primary round-8
// finding, in all four shapes the verifier reproduced. The disagreeing name
// carries a .cog-relative value on one branch and an opaque parameter on the
// other, and is then joined/concatenated under a plainly non-.cog absolute
// root. Round 7 answered "elsewhere" with the marker sitting unnoticed in the
// pattern's tail — a genuine .cog/ write stamped with a confident non-.cog
// category, which is the one failure this package declares never acceptable.
func TestComposedDisagreementNeverLaundersToElsewhere(t *testing.T) {
	tests := []struct {
		name string
		src  string
		line int
		want string // the composed pattern the site must report
	}{
		{
			name: "pair joined under a non-.cog literal root",
			src: `package p
func Save(useCog bool, opaque string) error {
	name := opaque
	if useCog {
		name = cogRel()
	}
	return os.WriteFile(filepath.Join("/var/lib/myapp", name), nil, 0644)
}
func cogRel() string { return ".cog/state.json" }`,
			line: 7,
			want: "/var/lib/myapp/{name}",
		},
		{
			name: "pair concatenated onto a non-.cog literal root",
			src: `package p
func Save(useCog bool, opaque string) error {
	name := opaque
	if useCog {
		name = cogRel()
	}
	return os.WriteFile("/var/lib/myapp/"+name, nil, 0644)
}
func cogRel() string { return ".cog/state.json" }`,
			line: 7,
			want: "/var/lib/myapp/{name}",
		},
		{
			name: "pair wrapped in filepath.Base inside a join",
			src: `package p
func Save(useCog bool, opaque string) error {
	name := opaque
	if useCog {
		name = cogRel()
	}
	return os.WriteFile(filepath.Join("/var/lib/myapp", filepath.Base(name)), nil, 0644)
}
func cogRel() string { return ".cog/state.json" }`,
			line: 7,
			want: "/var/lib/myapp/basename({name})",
		},
		{
			name: "pair composed one call-frame down, through a chased callee",
			src: `package p
func Save(useCog bool, opaque string) error {
	return os.WriteFile(filepath.Join("/var/lib/myapp", pick(useCog, opaque)), nil, 0644)
}
func pick(useCog bool, opaque string) string {
	name := opaque
	if useCog {
		name = cogRel()
	}
	return name
}
func cogRel() string { return ".cog/state.json" }`,
			line: 3,
			want: "/var/lib/myapp/{name}",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := siteAtLine(t, tc.src, tc.line)
			if s.Category == "elsewhere" || s.Category == "home" {
				t.Fatalf("category = %q pattern = %q — a disagreement composed under a positively-known non-.cog root must not produce a confident category; the .cog branch is a real possible destination", s.Category, s.Pattern)
			}
			if s.Category != "unanchored" {
				t.Errorf("category = %q pattern = %q, want unanchored", s.Category, s.Pattern)
			}
			if s.Pattern != tc.want {
				t.Errorf("pattern = %q, want %q — the disagreement is carried out-of-band, so the text is the variable's ordinary opaque placeholder and nothing else", s.Pattern, tc.want)
			}
		})
	}
}

// TestComposedDisagreementUnderInvariantCogTailStaysCog is the OTHER
// direction, and it is a pinned decision rather than an accident: a .cog
// segment that sits OUTSIDE the disagreement is invariant across every branch,
// so the site writes under .cog/ whichever branch runs and classify's
// unconditional cog-first test is allowed to say so. This is the shape
// internal/engine/cli_install_unix.go's cogBinDir has — only `home`
// disagrees (os.UserHomeDir vs os.Getenv("HOME")), while ".cog", "bin" are
// literals — and it is why the five cli_selfupdate_unix.go rows keep "cog"
// with the honest pattern "{home}/.cog/bin/cogos" instead of a marker.
//
// Changing this to "unanchored" would be a deliberate tightening, not a bug
// fix; this test exists so that change has to be made on purpose.
func TestComposedDisagreementUnderInvariantCogTailStaysCog(t *testing.T) {
	src := `package p
func Save() error {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return os.WriteFile(filepath.Join(home, ".cog", "bin", "cogos"), nil, 0644)
}`
	s := siteAtLine(t, src, 7)
	if s.Category != "cog" {
		t.Errorf("category = %q pattern = %q, want cog — only the root disagrees; the .cog/bin tail is invariant across both branches, so the write is under .cog/ either way", s.Category, s.Pattern)
	}
	if s.Pattern != "{home}/.cog/bin/cogos" {
		t.Errorf("pattern = %q, want %q — no marker text may reach a shipping pattern", s.Pattern, "{home}/.cog/bin/cogos")
	}
}

// TestComposedDisagreementUnderHomeRootIsNotHome is the home-side counterpart
// of the elsewhere test above, and the reason classify gates its <Home> test
// on degenerateRoot. "home" is a CONFIDENT category exactly like "elsewhere":
// the invariant forbids assigning it from a value only one branch justifies.
// Only "cog" is exempt, and only because over-claiming toward .cog is this
// tool's acceptable failure direction.
func TestComposedDisagreementUnderHomeRootIsNotHome(t *testing.T) {
	src := `package p
func Save(useCog bool, opaque string) error {
	home, _ := os.UserHomeDir()
	name := opaque
	if useCog {
		name = cogRel()
	}
	return os.WriteFile(filepath.Join(home, name), nil, 0644)
}
func cogRel() string { return ".cog/state.json" }`
	s := siteAtLine(t, src, 8)
	if s.Category == "home" || s.Category == "elsewhere" {
		t.Fatalf("category = %q pattern = %q — a disagreeing tail under <Home> has not established a home write any more than it established an elsewhere one", s.Category, s.Pattern)
	}
	if s.Category != "unanchored" {
		t.Errorf("category = %q pattern = %q, want unanchored", s.Category, s.Pattern)
	}
}

// TestIfInitAssignmentIsNotConditional is the round-8 latent-correctness
// finding. An assignment in an IfStmt's (or SwitchStmt's) Init ALWAYS executes
// on the way into the statement, so the binding it displaces is DEAD — it can
// never reach any site below. applyDirectLocalDefs counted the whole IfStmt
// node as the conditional region, header included, so the Init's `=` recorded
// the dead pre-init value in displaced[]; a later unreadable body rebind then
// paired against that dead value, and two dead-but-agreeing values handed the
// site a confident category built from a path that cannot run.
//
// Here "/etc/base.json" (dead) and other() (live) both classify "elsewhere"
// and agree, so round 7 reported "/etc/base.json" — while the LIVE candidates,
// cogPath() and other(), disagree and one of them is .cog-rooted. The fix
// splits the one depth counter in two: scopeDepth (the whole statement, for
// `:=` shadow protection) and condDepth (the bodies only, for execution
// certainty). poisonConditionalAndLoopRebinds already walked only Body/Else;
// this is the matching half.
//
// No site in the repo has this shape today, so the golden is unchanged by it —
// but it is squarely inside the class this repair claims to close.
func TestIfInitAssignmentIsNotConditional(t *testing.T) {
	src := `package p
func Save(a bool) error {
	path := "/etc/base.json"
	if path = cogPath(); a {
		path = other()
	}
	return os.WriteFile(path, nil, 0644)
}
func cogPath() string { return "/srv/.cog/live.json" }
func other() string   { return "/var/other.json" }`
	s := siteAtLine(t, src, 7)
	if s.Category == "elsewhere" || s.Category == "home" {
		t.Fatalf("category = %q pattern = %q — the pre-init binding is dead; the live candidates are cogPath() and other(), which disagree", s.Category, s.Pattern)
	}
	if s.Category != "unanchored" {
		t.Errorf("category = %q pattern = %q, want unanchored", s.Category, s.Pattern)
	}
	if strings.Contains(s.Pattern, "/etc/base.json") {
		t.Errorf("pattern = %q — reports a value the Init assignment overwrote before the branch was ever reached", s.Pattern)
	}
}

// TestIfInitDefineStillShadows is the control for the split above: the shadow
// half of the old single counter must be untouched. A `:=` in an if HEADER is
// scoped to that if statement, so it must not clobber the outer binding for a
// site AFTER the statement — which is exactly why scopeDepth still counts the
// whole statement node while condDepth counts only its bodies.
func TestIfInitDefineStillShadows(t *testing.T) {
	src := `package p
func Save(a bool, root string) error {
	path := filepath.Join(root, ".cog", "outer.json")
	if path := "/etc/shadow.json"; a {
		_ = path
	}
	return os.WriteFile(path, nil, 0644)
}`
	s := siteAtLine(t, src, 7)
	if s.Category != "cog" {
		t.Errorf("category = %q pattern = %q, want cog — the header `:=` declares a name scoped to the if statement and must resolve exactly as if the shadow never existed", s.Category, s.Pattern)
	}
}

// ─── Round-9 gate: closure bodies and forward `goto` ──────────────────────
//
// Two false-"elsewhere" mechanisms that sat OUTSIDE the four gaps the
// resolution-semantics invariant paragraph enumerated, both predating the
// branch-pair mechanism (they reproduce identically at 82e8cb3 and d941abe)
// and both latent — no shipping site had either shape.
//
// First: a FUNC-LITERAL body was absent from applyDirectLocalDefs's
// condRegion, so a closure's plain `=` rebind ran at condDepth 0. applyAssign
// therefore never recorded what it displaced, and the pairing loop's
// `if !hadOuter { continue }` left the closure's own RHS alone in defs for
// the full interprocedural resolver — which reads shapes staticPathText
// cannot. The closure was neither poisoned NOR paired: exactly the hole the
// round-7 pair was built to close for conditionals, still open one subtree
// kind over. TestFuncLitRebindPoisonsEnclosingSite could not see it because
// its rebind is a bare string literal, which staticPathText reads and
// poisons outright on the readable-and-non-.cog path.
//
// Second: containsGoto modeled only the BACKWARD jump (statements after the
// site can precede it on a second pass). A FORWARD `goto` jumping OVER a
// rebind leaves that rebind folded as if it always executes.

// TestFuncLitUnreadableRebindPairsAgainstDisplacedBinding is the first
// mechanism, in all four shapes a closure reaches an enclosing local through:
// called via a parameter, deferred, spawned with `go`, and bound to a local.
// The RHS is a declared function's return value — resolvable by the full
// resolver, invisible to staticPathText — so the rebind lands in the
// unreadable bucket, and the site must fall to unanchored rather than take
// either branch's value.
func TestFuncLitUnreadableRebindPairsAgainstDisplacedBinding(t *testing.T) {
	const prelude = `package p
func globalSettingsPath() string { return "/etc/cogos/global-settings.json" }
`
	cases := []struct {
		name string
		body string
		line int
	}{
		{"called-through-parameter", `func f(root string, run func(func())) {
	path := filepath.Join(root, ".cog", "settings.json")
	run(func() {
		path = globalSettingsPath()
	})
	os.WriteFile(path, nil, 0644)
}`, 8},
		{"defer", `func f(root string) {
	path := filepath.Join(root, ".cog", "settings.json")
	defer func() {
		path = globalSettingsPath()
	}()
	os.WriteFile(path, nil, 0644)
}`, 8},
		{"go", `func f(root string) {
	path := filepath.Join(root, ".cog", "settings.json")
	go func() {
		path = globalSettingsPath()
	}()
	os.WriteFile(path, nil, 0644)
}`, 8},
		{"bound-to-local", `func f(root string) {
	path := filepath.Join(root, ".cog", "settings.json")
	fn := func() {
		path = globalSettingsPath()
	}
	_ = fn
	os.WriteFile(path, nil, 0644)
}`, 9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := siteAtLine(t, prelude+tc.body, tc.line)
			if s.Category == "elsewhere" || s.Category == "home" {
				t.Fatalf("category = %q pattern = %q — real Go sends this write to {root}/.cog/settings.json whenever the closure has not run; a confident NON-cog category here is the failure this package treats as never acceptable", s.Category, s.Pattern)
			}
			if s.Category != "unanchored" {
				t.Errorf("category = %q pattern = %q, want unanchored — whether the closure ran before this write is not something this package models, so the two candidate values must be paired and disagree", s.Category, s.Pattern)
			}
		})
	}
}

// TestFuncLitCogBearingRebindStaysTrusted is the control for the deliberate
// cog-favoring asymmetry, at a closure instead of a conditional: a rebind
// staticPathText CAN read as .cog-rooted is still neither poisoned nor
// paired, exactly as TestConditionalCogBearingRebindStaysTrusted pins for an
// `if` body. Closing the unreadable case above must not have made this
// symmetric by accident.
func TestFuncLitCogBearingRebindStaysTrusted(t *testing.T) {
	src := `package p
func f(elsewhereRoot string, run func(func())) {
	path := filepath.Join(elsewhereRoot, "settings.json")
	run(func() {
		path = "/var/lib/x/.cog/settings.json"
	})
	os.WriteFile(path, nil, 0644)
}`
	s := siteAtLine(t, src, 7)
	if s.Category != "cog" {
		t.Errorf("category = %q pattern = %q, want cog — a closure rebind THIS tool can read as .cog-rooted is deliberately never poisoned, the same documented asymmetry conditionals get", s.Category, s.Pattern)
	}
}

// TestFuncLitShadowDeclarationLeavesOuterBindingIntact is the closure twin of
// the `:=`-shadow control: a `:=` inside a closure declares a variable scoped
// to the closure and must not clobber the enclosing function's binding of the
// same name. Before the condRegion/isScope fix this was a THIRD false
// "elsewhere" of the same family — the closure's dead local resolved the
// enclosing function's genuine .cog write.
func TestFuncLitShadowDeclarationLeavesOuterBindingIntact(t *testing.T) {
	src := `package p
func f(root string) {
	path := filepath.Join(root, ".cog", "settings.json")
	defer func() {
		path := "/etc/cogos/g.json"
		_ = path
	}()
	os.WriteFile(path, nil, 0644)
}`
	s := siteAtLine(t, src, 8)
	if s.Category != "cog" {
		t.Errorf("category = %q pattern = %q, want cog — the closure's `:=` is its own local and must resolve exactly as if it never existed", s.Category, s.Pattern)
	}
}

// TestForwardGotoOverRebindPoisonsSite is the second mechanism. `goto done`
// jumps over the `path = "/etc/a.json"` rebind, so when a is true real Go
// writes {root}/.cog/settings.json — but the fold sees only a straight-line
// rebind preceding the site and, before this fix, stamped it "elsewhere"
// with the skipped branch's literal.
func TestForwardGotoOverRebindPoisonsSite(t *testing.T) {
	src := `package p
func f(root string, a bool) {
	path := filepath.Join(root, ".cog", "settings.json")
	if a {
		goto done
	}
	path = "/etc/a.json"
done:
	os.WriteFile(path, nil, 0644)
}`
	s := siteAtLine(t, src, 9)
	if s.Category == "elsewhere" || s.Category == "home" {
		t.Fatalf("category = %q pattern = %q — the `goto` skips the rebind when a is true, so real Go sends this write into .cog/", s.Category, s.Pattern)
	}
	if s.Category != "unanchored" {
		t.Errorf("category = %q pattern = %q, want unanchored", s.Category, s.Pattern)
	}
}

// TestBackwardGotoStillPoisonsAfterSiteRebinds is the control for the
// direction containsGoto already modeled: a backward jump brings the site
// around again after the rebind that follows it. Behavior here is unchanged —
// the site was, and stays, unanchored.
func TestBackwardGotoStillPoisonsAfterSiteRebinds(t *testing.T) {
	src := `package p
func f(root string, a bool) {
	path := filepath.Join(root, ".cog", "settings.json")
again:
	os.WriteFile(path, nil, 0644)
	path = "/etc/a.json"
	if a {
		goto again
	}
}`
	s := siteAtLine(t, src, 5)
	if s.Category != "unanchored" {
		t.Errorf("category = %q pattern = %q, want unanchored — the back edge re-runs this write after the rebind", s.Category, s.Pattern)
	}
}
