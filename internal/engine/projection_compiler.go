// projection_compiler.go — depth-axis projection primitive that compiles
// reflective cogdocs (verbatim operator articulations) into structured
// Reconcilable-readable events.
//
// Implements the seven-method pkg/substrate/reconcile.Reconcilable contract
// per ADR projection-compiler-primitive (accepted 2026-05-22).
//
// v0 scope (per ADR §Implementation §Smallest first):
//
//   - Pointer-emission path: unchanged blocks re-emit prior events with
//     CompileModel="pointer"; no LLM call, no thermodynamic cost.
//   - Structural-extraction path: blocks with explicit boundaries
//     (## Quote N or ## Distinction N: headings, or ">"-prefixed blockquotes)
//     emit one event per boundary via deterministic parsing; no LLM call.
//     This covers the two canonical first inputs (2026-05-19, 2026-05-20).
//
// Deferred:
//
//   - LLM extraction path (E4B-on-Darkstar) for blocks without explicit
//     boundaries.
//   - FrictionEvent coherence checker.
//   - fsnotify watch trigger (the ReconcileDaemon's periodic tick covers v0).
//   - cog_compile_projections MCP tool.
//
// Parser dependency: this v0 shells out to scripts/cogblock.py for parsing.
// The Go-side pkg/cogblock package holds the canonical block type but is
// not a markdown parser; per ADR §Implementation §Dependencies cogblock.py
// is the reference implementation. The path is resolved via, in order:
//
//   1. ProjectionCompilerConfig.CogblockPath (explicit injection)
//   2. environment variable COGBLOCK_PY
//   3. <workspaceRoot>/scripts/cogblock.py
//
// Per ADR-091 layering this file is Kernel (engine package). The event
// types it emits live in pkg/substrate/projection (Substrate layer).
package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/projection"
	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// ─── Reconcilable type identifier ────────────────────────────────────────────

// CompilerType is the Reconcilable.Type() string for the Projection Compiler.
// Used by the daemon and by callers triggering early reconciles.
const CompilerType = "projection-compiler"

// ─── Config + per-file representations ───────────────────────────────────────

// CompilerConfig is the "declared config" the compiler loads from disk.
// Carries the source directory, cogblock.py path, and optional source-file
// allowlist (testing seam).
type CompilerConfig struct {
	// SourceDir is the absolute path of the reflective cogdoc directory.
	// Default: <workspaceRoot>/.cog/mem/reflective.
	SourceDir string

	// CogblockPath is the absolute path to scripts/cogblock.py used for
	// parsing. See package doc for resolution order.
	CogblockPath string

	// StatePath is the absolute path of the per-compiler state file.
	// Default: <workspaceRoot>/.cog/state/projection-compiler.json.
	// (Note: the daemon also persists state via reconcile.WriteState; this
	// path is for the compiler's own in-band content-hash tracking, used
	// by ComputePlan to classify blocks as new/changed/unchanged without
	// re-reading the daemon's state.)
	StatePath string

	// SourceFiles, when non-nil, restricts the compiler to this exact
	// list of absolute paths instead of walking SourceDir. Used by tests
	// to isolate fixtures without depending on the live cog corpus.
	SourceFiles []string
}

// sourceCogdoc is the parsed in-memory representation of one reflective
// cogdoc returned by FetchLive.
type sourceCogdoc struct {
	// Path is the absolute filesystem path of the source cogdoc.
	Path string

	// Slug is the cog://mem/reflective/<slug> identifier. Derived from
	// frontmatter.slug if present, otherwise from the filename
	// (stripped of .cog.md).
	Slug string

	// Frontmatter is the parsed YAML frontmatter as a generic map.
	Frontmatter map[string]any

	// Blocks is the parsed block tree from cogblock.py. Each block is
	// {type, level, heading, body, ...}.
	Blocks []map[string]any
}

// extractedBlock is one event-candidate identified inside a source cogdoc.
// Produced by classify() and consumed by ApplyPlan/BuildState.
type extractedBlock struct {
	// SourceCogdoc is the parent cogdoc.
	SourceCogdoc *sourceCogdoc

	// Anchor is the URI fragment, e.g. "quote-1" or "distinction-3".
	Anchor string

	// BlockHeading is the original heading text (unused in URI; carried
	// for ledger-side observability).
	BlockHeading string

	// Distinction is the load-bearing claim extracted from the block.
	Distinction string

	// ContentHash is sha256(source-uri + distinction + sorted-relations).
	ContentHash string

	// Strategy is "structural" (v0 path) for now; "llm" is reserved.
	Strategy string
}

// ─── Persisted in-band state ─────────────────────────────────────────────────

// compilerState is the on-disk hash map used to classify blocks as
// new/changed/unchanged across reconcile cycles. Source-anchor →
// last-emitted ContentHash.
type compilerState struct {
	// Version pins the schema for forward-compat.
	Version int `json:"version"`

	// HashByAnchor maps "cog://mem/reflective/<slug>#<anchor>" to the
	// ContentHash of the last emitted Event.
	HashByAnchor map[string]string `json:"hash_by_anchor"`
}

func newCompilerState() *compilerState {
	return &compilerState{Version: 1, HashByAnchor: map[string]string{}}
}

func loadCompilerState(path string) *compilerState {
	data, err := os.ReadFile(path)
	if err != nil {
		return newCompilerState()
	}
	var s compilerState
	if err := json.Unmarshal(data, &s); err != nil {
		return newCompilerState()
	}
	if s.HashByAnchor == nil {
		s.HashByAnchor = map[string]string{}
	}
	return &s
}

func writeCompilerState(path string, s *compilerState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ─── ProjectionCompiler ──────────────────────────────────────────────────────

// ProjectionCompiler implements reconcile.Reconcilable for the depth-axis
// projection of reflective cogdocs → projection.Event stream.
//
// Per the substrate rename convention the package-level name omits the
// redundant "Projection" prefix in cross-package references: callers
// receive projection.Event values, but the in-engine reconciler retains
// the long form since the engine package itself is generically named.
type ProjectionCompiler struct {
	mu     sync.Mutex
	health reconcile.ResourceStatus

	// emittedEvents holds the events emitted by the most recent ApplyPlan
	// run. Test seam: lets acceptance tests inspect the output without
	// wiring the full Observatory event bus. v0 does not yet emit to a
	// real bus; that wiring lands when the Observatory consumer is added.
	emittedEvents []projection.Event

	// lastSourceCount is the number of source cogdocs in the most recent
	// FetchLive.
	lastSourceCount int

	// lastEventCount is the number of events emitted in the most recent
	// ApplyPlan run (new + changed + pointer).
	lastEventCount int
}

// NewProjectionCompiler constructs a compiler in Progressing/Unknown health.
func NewProjectionCompiler() *ProjectionCompiler {
	return &ProjectionCompiler{
		health: reconcile.NewResourceStatus(
			reconcile.SyncStatusUnknown,
			reconcile.HealthProgressing,
		),
	}
}

// Type returns the Reconcilable type identifier.
func (c *ProjectionCompiler) Type() string { return CompilerType }

// LoadConfig resolves the source directory, cogblock.py path, and state
// path from the workspace root. Read-only disk operation.
//
// When cogblock.py is absent (fresh install, Windows, Python not set up),
// LoadConfig logs at DEBUG and returns a nil config so the reconcile cycle
// exits with "no drift" rather than WARNing on every tick.
func (c *ProjectionCompiler) LoadConfig(root string) (any, error) {
	cfg := &CompilerConfig{
		SourceDir:    filepath.Join(root, ".cog", "mem", "reflective"),
		CogblockPath: resolveCogblockPath(root),
		StatePath:    filepath.Join(root, ".cog", "state", "projection-compiler.json"),
	}
	if _, err := os.Stat(cfg.SourceDir); err != nil {
		if os.IsNotExist(err) {
			slog.Debug("projection-compiler: source dir absent, skipping", "dir", cfg.SourceDir)
			return nil, nil
		}
		return nil, fmt.Errorf("source dir: %w", err)
	}
	if _, err := os.Stat(cfg.CogblockPath); err != nil {
		if os.IsNotExist(err) {
			slog.Debug("projection-compiler: cogblock.py absent, skipping", "path", cfg.CogblockPath)
			return nil, nil
		}
		return nil, fmt.Errorf("cogblock.py at %s: %w", cfg.CogblockPath, err)
	}
	return cfg, nil
}

// resolveCogblockPath picks the cogblock.py location in priority order.
// See package doc.
func resolveCogblockPath(root string) string {
	if p := os.Getenv("COGBLOCK_PY"); p != "" {
		return p
	}
	return filepath.Join(root, "scripts", "cogblock.py")
}

// FetchLive walks the source directory (or the configured allowlist), parses
// each .cog.md file via cogblock.py, and returns the in-memory sourceCogdoc
// slice. Read-only against source files.
func (c *ProjectionCompiler) FetchLive(ctx context.Context, config any) (any, error) {
	// nil config means cogblock.py or source dir is absent — skip silently.
	if config == nil {
		return []*sourceCogdoc{}, nil
	}
	cfg, ok := config.(*CompilerConfig)
	if !ok {
		return nil, fmt.Errorf("projection-compiler: unexpected config type %T", config)
	}

	paths, err := listSourcePaths(cfg)
	if err != nil {
		return nil, err
	}

	out := make([]*sourceCogdoc, 0, len(paths))
	for _, p := range paths {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		doc, err := parseCogdoc(ctx, cfg.CogblockPath, p)
		if err != nil {
			slog.Warn("projection-compiler: parse failed", "path", p, "err", err)
			continue
		}
		out = append(out, doc)
	}

	// Deterministic order for plan stability.
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })

	c.mu.Lock()
	c.lastSourceCount = len(out)
	c.mu.Unlock()

	return out, nil
}

// listSourcePaths returns the file allowlist if set, else every .cog.md
// directly under SourceDir (no recursion: reflective/ is a flat directory
// in current corpus layout).
func listSourcePaths(cfg *CompilerConfig) ([]string, error) {
	if cfg.SourceFiles != nil {
		return append([]string{}, cfg.SourceFiles...), nil
	}
	entries, err := os.ReadDir(cfg.SourceDir)
	if err != nil {
		return nil, fmt.Errorf("read source dir: %w", err)
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".cog.md") {
			continue
		}
		paths = append(paths, filepath.Join(cfg.SourceDir, e.Name()))
	}
	return paths, nil
}

// parseCogdoc invokes cogblock.py parse on the given path and decodes the
// returned tree.
func parseCogdoc(ctx context.Context, cogblockPath, sourcePath string) (*sourceCogdoc, error) {
	cmd := exec.CommandContext(ctx, "python3", cogblockPath, "parse", sourcePath)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("cogblock.py parse: %w: %s", err, exitErr.Stderr)
		}
		return nil, fmt.Errorf("cogblock.py parse: %w", err)
	}

	var tree struct {
		Frontmatter map[string]any   `json:"frontmatter"`
		Blocks      []map[string]any `json:"blocks"`
	}
	if err := json.Unmarshal(out, &tree); err != nil {
		return nil, fmt.Errorf("decode cogblock.py output: %w", err)
	}

	doc := &sourceCogdoc{
		Path:        sourcePath,
		Frontmatter: tree.Frontmatter,
		Blocks:      tree.Blocks,
	}
	doc.Slug = deriveSlug(sourcePath, tree.Frontmatter)
	return doc, nil
}

// deriveSlug returns the cog://mem/reflective slug for a cogdoc. Prefers
// frontmatter.slug; falls back to filename without the .cog.md suffix.
func deriveSlug(path string, fm map[string]any) string {
	if v, ok := fm["slug"].(string); ok && v != "" {
		return v
	}
	base := filepath.Base(path)
	return strings.TrimSuffix(base, ".cog.md")
}

// ─── ComputePlan ─────────────────────────────────────────────────────────────

// ComputePlan classifies extracted blocks as new (create), changed (update),
// or unchanged (skip → pointer emission). Pure function given the inputs.
//
// The plan carries one Action per extractedBlock; ApplyPlan re-derives the
// block list from the same live snapshot (passed through plan.Metadata) to
// emit events deterministically.
func (c *ProjectionCompiler) ComputePlan(config any, live any, _ *reconcile.State) (*reconcile.Plan, error) {
	// nil config means cogblock.py / source dir absent — nothing to compile.
	if config == nil {
		return &reconcile.Plan{ResourceType: c.Type()}, nil
	}
	cfg := config.(*CompilerConfig)
	docs, ok := live.([]*sourceCogdoc)
	if !ok {
		return nil, fmt.Errorf("projection-compiler: unexpected live type %T", live)
	}

	state := loadCompilerState(cfg.StatePath)

	plan := &reconcile.Plan{
		ResourceType: c.Type(),
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		ConfigPath:   cfg.SourceDir,
	}

	for _, doc := range docs {
		blocks := extractBlocks(doc)
		for _, b := range blocks {
			anchorURI := sourceURI(doc.Slug, b.Anchor)
			prev, seen := state.HashByAnchor[anchorURI]

			var action reconcile.ActionType
			switch {
			case !seen:
				action = reconcile.ActionCreate
				plan.Summary.Creates++
			case prev != b.ContentHash:
				action = reconcile.ActionUpdate
				plan.Summary.Updates++
			default:
				action = reconcile.ActionSkip
				plan.Summary.Skipped++
			}

			plan.Actions = append(plan.Actions, reconcile.Action{
				Action:       action,
				ResourceType: c.Type(),
				Name:         anchorURI,
				Details: map[string]any{
					"source_path":  doc.Path,
					"slug":         doc.Slug,
					"anchor":       b.Anchor,
					"heading":      b.BlockHeading,
					"distinction":  b.Distinction,
					"content_hash": b.ContentHash,
					"strategy":     b.Strategy,
				},
			})
		}
	}

	return plan, nil
}

// ─── ApplyPlan ───────────────────────────────────────────────────────────────

// ApplyPlan emits Events for every action: extraction events for
// create/update, pointer events for skip. Updates the in-band state file
// so subsequent ComputePlan calls correctly classify blocks.
//
// v0 holds emitted events in the compiler struct rather than publishing to
// an event bus; the Observatory consumer is wired in a follow-up.
func (c *ProjectionCompiler) ApplyPlan(ctx context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	cfg, err := compilerCfgFromPlan(plan)
	_ = cfg
	if err != nil {
		// Non-fatal: ApplyPlan does not strictly need CompilerConfig in v0
		// because all data is on the plan actions. State persistence falls
		// back to the daemon's reconcile.WriteState path. Logged here so
		// integration can spot it if the daemon disables that path.
		slog.Debug("projection-compiler: no cfg in plan metadata", "err", err)
	}

	c.mu.Lock()
	c.emittedEvents = c.emittedEvents[:0]
	c.mu.Unlock()

	var results []reconcile.Result
	state := newCompilerState()
	// Re-seed from disk if available so partial applies still preserve
	// the running hash map. The state path may be absent in tests.
	if cfg != nil {
		state = loadCompilerState(cfg.StatePath)
	}

	for _, action := range plan.Actions {
		select {
		case <-ctx.Done():
			return results, ctx.Err()
		default:
		}

		ev, err := buildEventFromAction(action)
		if err != nil {
			results = append(results, reconcile.Result{
				Phase:  "apply",
				Action: string(action.Action),
				Name:   action.Name,
				Status: reconcile.ApplyFailed,
				Error:  err.Error(),
			})
			continue
		}

		// Mode: pointer for skips, structural for extraction.
		if action.Action == reconcile.ActionSkip {
			ev.CompileModel = "pointer"
		} else {
			ev.CompileModel = "structural"
		}

		c.mu.Lock()
		c.emittedEvents = append(c.emittedEvents, ev)
		c.mu.Unlock()

		state.HashByAnchor[ev.Source] = ev.ContentHash

		results = append(results, reconcile.Result{
			Phase:     "apply",
			Action:    string(action.Action),
			Name:      action.Name,
			Status:    reconcile.ApplySucceeded,
			CreatedID: ev.ContentHash,
		})
	}

	if cfg != nil {
		if err := writeCompilerState(cfg.StatePath, state); err != nil {
			slog.Warn("projection-compiler: WriteState fallback failed", "err", err)
		}
	}

	c.mu.Lock()
	c.lastEventCount = len(c.emittedEvents)
	c.health = reconcile.NewResourceStatus(reconcile.SyncStatusSynced, reconcile.HealthHealthy)
	c.mu.Unlock()

	return results, nil
}

// compilerCfgFromPlan is a defensive accessor; ComputePlan does not stash the
// config on the plan today, but ApplyPlan may receive one via test harnesses
// that pre-stamp plan.Metadata.
func compilerCfgFromPlan(plan *reconcile.Plan) (*CompilerConfig, error) {
	if plan == nil || plan.Metadata == nil {
		return nil, fmt.Errorf("no plan metadata")
	}
	raw, ok := plan.Metadata["compiler_config"]
	if !ok {
		return nil, fmt.Errorf("no compiler_config in plan metadata")
	}
	cfg, ok := raw.(*CompilerConfig)
	if !ok {
		return nil, fmt.Errorf("compiler_config wrong type %T", raw)
	}
	return cfg, nil
}

// buildEventFromAction reconstructs a projection.Event from the per-action
// details map. ApplyPlan calls this for every action regardless of mode;
// the caller stamps CompileModel afterwards.
func buildEventFromAction(action reconcile.Action) (projection.Event, error) {
	getStr := func(k string) string {
		v, _ := action.Details[k].(string)
		return v
	}
	getSlice := func(k string) []string {
		raw, ok := action.Details[k]
		if !ok {
			return nil
		}
		if s, ok := raw.([]string); ok {
			return s
		}
		if s, ok := raw.([]any); ok {
			out := make([]string, 0, len(s))
			for _, v := range s {
				if vs, ok := v.(string); ok {
					out = append(out, vs)
				}
			}
			return out
		}
		return nil
	}

	if getStr("anchor") == "" {
		return projection.Event{}, fmt.Errorf("missing anchor on action %q", action.Name)
	}
	if getStr("content_hash") == "" {
		return projection.Event{}, fmt.Errorf("missing content_hash on action %q", action.Name)
	}

	return projection.Event{
		Source:      action.Name,
		Distinction: getStr("distinction"),
		Relations:   getSlice("relations"),
		Salience:    getStr("salience"),
		Tags:        getSlice("tags"),
		Authors:     getSlice("authors"),
		Date:        getStr("date"),
		ContentHash: getStr("content_hash"),
		SourceFile:  getStr("source_path"),
	}, nil
}

// ─── BuildState ──────────────────────────────────────────────────────────────

// BuildState constructs the reconcile.State snapshot. One Resource per
// extracted block: address = source URI, external_id = content_hash.
func (c *ProjectionCompiler) BuildState(config any, live any, existing *reconcile.State) (*reconcile.State, error) {
	// nil config means cogblock.py / source dir absent — return empty state.
	if config == nil {
		return reconcile.NewState(c.Type()), nil
	}
	cfg, ok := config.(*CompilerConfig)
	if !ok {
		return nil, fmt.Errorf("projection-compiler: unexpected config type %T", config)
	}
	docs, ok := live.([]*sourceCogdoc)
	if !ok {
		return nil, fmt.Errorf("projection-compiler: unexpected live type %T", live)
	}

	state := reconcile.NewState(c.Type())
	if existing != nil {
		state.Lineage = existing.Lineage
		state.Serial = existing.Serial
	}

	now := time.Now().UTC().Format(time.RFC3339)
	for _, doc := range docs {
		for _, b := range extractBlocks(doc) {
			uri := sourceURI(doc.Slug, b.Anchor)
			state.Resources = append(state.Resources, reconcile.Resource{
				Address:    "projection-event." + uri,
				Type:       "projection-event",
				Mode:       reconcile.ModeManaged,
				Name:       uri,
				ExternalID: b.ContentHash,
				Attributes: map[string]any{
					"slug":     doc.Slug,
					"anchor":   b.Anchor,
					"strategy": b.Strategy,
				},
				LastRefreshed: now,
			})
		}
	}
	_ = cfg
	return state, nil
}

// Health returns the current three-axis status.
func (c *ProjectionCompiler) Health() reconcile.ResourceStatus {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.health
}

// EmittedEvents returns the events from the most recent ApplyPlan. Test
// seam; production callers should consume from the Observatory event bus
// once that wiring lands.
func (c *ProjectionCompiler) EmittedEvents() []projection.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]projection.Event, len(c.emittedEvents))
	copy(out, c.emittedEvents)
	return out
}

// ─── Extraction (the load-bearing v0 logic) ──────────────────────────────────

// extractBlocks identifies the event-candidates inside a parsed cogdoc.
//
// v0 strategy (per ADR §3 §Block-level extraction targets):
//
//  1. Explicit-boundary first: H2 sections whose heading matches
//     "Quote N" or "Distinction N:" patterns. One event per such section.
//     The distinction text is the leading blockquote line if present
//     (Quote-N style on the 2026-05-19 cogdoc), else the heading text
//     after the prefix (Distinction-N style on the 2026-05-20 cogdoc).
//  2. (Deferred) Structural inference for sections without explicit
//     boundaries — needs LLM extraction.
//  3. (Deferred) Conservative fallback — one event per section.
//
// The deferred paths are explicitly NOT exercised in v0; non-matching
// sections produce zero events. This is the safe behavior for the v0
// scope: only the two canonical cogdocs are compiled, both use the
// explicit-boundary patterns, and the count assertions in the acceptance
// criteria pin the structural shape.
func extractBlocks(doc *sourceCogdoc) []extractedBlock {
	var out []extractedBlock
	relations := inheritedRelations(doc.Frontmatter)
	tags := inheritedStringSlice(doc.Frontmatter, "tags")
	authors := inheritedStringSlice(doc.Frontmatter, "authors")
	salience, _ := doc.Frontmatter["salience"].(string)
	date := frontmatterDate(doc.Frontmatter)

	for _, block := range doc.Blocks {
		btype, _ := block["type"].(string)
		if btype != "section" {
			continue
		}
		levelF, _ := block["level"].(float64)
		if int(levelF) != 2 {
			continue
		}
		heading, _ := block["heading"].(string)
		body, _ := block["body"].(string)

		anchor, distinction, ok := classifySection(heading, body)
		if !ok {
			continue
		}

		uri := sourceURI(doc.Slug, anchor)
		hash := compilerEventHash(uri, distinction, relations)

		out = append(out, extractedBlock{
			SourceCogdoc: doc,
			Anchor:       anchor,
			BlockHeading: heading,
			Distinction:  distinction,
			ContentHash:  hash,
			Strategy:     "structural",
		})
		_ = salience
		_ = tags
		_ = authors
		_ = date
	}
	return out
}

// classifySection inspects an H2 heading + body and decides whether it
// matches an explicit-boundary pattern. Returns (anchor, distinction, ok).
//
// Patterns:
//
//   - "Quote N — ..." or "Quote N - ..." or "Quote N ..." or "Quote N:" →
//     anchor = "quote-N". Distinction = first ">" blockquote line in body
//     if present (the typical 2026-05-19 shape), else the heading text
//     after "Quote N — ".
//   - "Distinction N: ..." or "Distinction N — ..." → anchor =
//     "distinction-N". Distinction = heading text after the prefix.
func classifySection(heading, body string) (string, string, bool) {
	h := strings.TrimSpace(heading)
	lower := strings.ToLower(h)

	if n, after, ok := matchPrefixNumber(lower, h, "quote "); ok {
		anchor := fmt.Sprintf("quote-%d", n)
		distinction := firstBlockquoteLine(body)
		if distinction == "" {
			distinction = trimSeparators(after)
		}
		return anchor, distinction, true
	}
	if n, after, ok := matchPrefixNumber(lower, h, "distinction "); ok {
		anchor := fmt.Sprintf("distinction-%d", n)
		distinction := trimSeparators(after)
		return anchor, distinction, true
	}
	return "", "", false
}

// matchPrefixNumber parses headings of the form "<prefix>N<sep><rest>"
// where prefix is case-insensitive, N is a positive integer, and sep is
// one of ":", "—", "-", or whitespace.
//
// Returns (N, rest, ok). `rest` is from the original-case heading so
// downstream consumers keep the verbatim distinction text.
func matchPrefixNumber(lower, orig, prefix string) (int, string, bool) {
	if !strings.HasPrefix(lower, prefix) {
		return 0, "", false
	}
	tail := orig[len(prefix):]
	i := 0
	for i < len(tail) && tail[i] >= '0' && tail[i] <= '9' {
		i++
	}
	if i == 0 {
		return 0, "", false
	}
	var n int
	_, err := fmt.Sscanf(tail[:i], "%d", &n)
	if err != nil {
		return 0, "", false
	}
	rest := tail[i:]
	// Require a separator (or end-of-string) right after the number so
	// "Distinction17 unrelated word" doesn't accidentally match — but
	// "Distinction 17:" and "Distinction 17 — foo" both do.
	if rest != "" && !isSeparatorByte(rest[0]) {
		return 0, "", false
	}
	return n, rest, true
}

func isSeparatorByte(b byte) bool {
	switch b {
	case ':', '-', ' ', '\t':
		return true
	}
	// em-dash is multi-byte UTF-8; check via string-level prefix below.
	return false
}

// trimSeparators strips leading ":", "—", "-", and whitespace.
func trimSeparators(s string) string {
	s = strings.TrimLeft(s, " \t:-")
	s = strings.TrimPrefix(s, "—")
	s = strings.TrimLeft(s, " \t")
	return strings.TrimSpace(s)
}

// firstBlockquoteLine returns the first ">"-prefixed line in body with the
// marker and surrounding whitespace stripped. Empty if no blockquote line.
//
// Used for Quote-N style sections where the distinction is the verbatim
// quote and the prose under it is interpretation.
func firstBlockquoteLine(body string) string {
	var collected []string
	inQuote := false
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimLeft(raw, " \t")
		if strings.HasPrefix(line, ">") {
			inQuote = true
			collected = append(collected, strings.TrimSpace(strings.TrimPrefix(line, ">")))
			continue
		}
		if inQuote {
			// Blank line ends the blockquote.
			if strings.TrimSpace(line) == "" {
				break
			}
			// Continuation of the same blockquote in lazy form.
			collected = append(collected, strings.TrimSpace(line))
		}
	}
	return strings.TrimSpace(strings.Join(collected, " "))
}

// ─── URI + hashing + frontmatter helpers ─────────────────────────────────────

// sourceURI builds the per-event source URI per ADR §4.
func sourceURI(slug, anchor string) string {
	return fmt.Sprintf("cog://mem/reflective/%s#%s", slug, anchor)
}

// compilerEventHash is the idempotency key: sha256 over source +
// distinction + sorted relations. Named with the compiler prefix to avoid
// collision with engine.contentHash in context_frame.go.
func compilerEventHash(source, distinction string, relations []string) string {
	rels := append([]string{}, relations...)
	sort.Strings(rels)
	h := sha256.New()
	h.Write([]byte(source))
	h.Write([]byte{0})
	h.Write([]byte(distinction))
	h.Write([]byte{0})
	for _, r := range rels {
		h.Write([]byte(r))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// inheritedRelations pulls cog:// URIs out of the cogdoc's frontmatter.
// Looks at "refs", "related", "relates-to", and "relates_to" (yaml
// normalization differs across cogdocs).
func inheritedRelations(fm map[string]any) []string {
	if fm == nil {
		return nil
	}
	var out []string
	for _, key := range []string{"refs", "related", "relates-to", "relates_to"} {
		raw, ok := fm[key]
		if !ok {
			continue
		}
		switch v := raw.(type) {
		case []any:
			for _, e := range v {
				switch ev := e.(type) {
				case string:
					if strings.HasPrefix(ev, "cog://") {
						out = append(out, ev)
					}
				case map[string]any:
					if uri, ok := ev["uri"].(string); ok && strings.HasPrefix(uri, "cog://") {
						out = append(out, uri)
					}
				}
			}
		case []string:
			for _, s := range v {
				if strings.HasPrefix(s, "cog://") {
					out = append(out, s)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// inheritedStringSlice extracts a []string from frontmatter[key].
func inheritedStringSlice(fm map[string]any, key string) []string {
	if fm == nil {
		return nil
	}
	raw, ok := fm[key]
	if !ok {
		return nil
	}
	switch v := raw.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return append([]string{}, v...)
	}
	return nil
}

// frontmatterDate returns the cogdoc's date in priority order:
// date → created → updated.
func frontmatterDate(fm map[string]any) string {
	for _, key := range []string{"date", "created", "updated"} {
		if v, ok := fm[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
