// Package pin implements the inter-workspace pin Reconcilable for CogOS.
//
// Pin records are source-side relationship facts declared at
// <workspace>/.cog/pins/<target>.yaml. A source workspace pins a target
// workspace at a specific git ref and optional content digest. Multiple
// source workspaces can pin the same target independently — each pin record
// carries its own ref, sync policy, and branch context.
//
// The Reconcilable lifecycle:
//   - LoadConfig  — reads all .cog/pins/*.yaml files from the source workspace
//   - FetchLive   — resolves the live HEAD ref for each target workspace
//   - ComputePlan — diffs pinned ref vs live HEAD; emits drift actions
//   - ApplyPlan   — bumps pin ref (after explicit user action); no-op for read-only
//   - BuildState  — projects pin status into queryable Reconcile State
//   - Health      — green/yellow/red aggregate across all pins
//   - Reconcile   — full cycle (LoadConfig → FetchLive → ComputePlan → ApplyPlan)
//
// URIRegistry dependency: FetchLive resolves target workspace names via a
// WorkspaceLocator (backed by engine.URIRegistry, wired at startup). When no
// locator is injected the provider falls back to local git checkout inspection.
// The locator is injected via Provider.SetWorkspaceLocator after construction.
package pin

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"errors"

	"gopkg.in/yaml.v3"

	"github.com/myrgic/cogos/pkg/reconcile"
)

// ─── WorkspaceLocator ────────────────────────────────────────────────────────

// WorkspaceLocator resolves a workspace name (e.g. "myrgic/cogos") to its
// absolute filesystem root path. In production it is backed by engine.URIRegistry
// via an adapter wired at daemon startup. Tests inject a stub.
type WorkspaceLocator interface {
	// LocateWorkspace returns the filesystem root for the named workspace.
	// Returns ErrWorkspaceNotFound when the workspace is not registered.
	LocateWorkspace(ctx context.Context, name string) (path string, err error)
}

// ErrWorkspaceNotFound is returned by WorkspaceLocator when the target workspace
// is not registered in the global workspace registry.
var ErrWorkspaceNotFound = errors.New("pin: workspace not found in registry")

// ─── Schema types ────────────────────────────────────────────────────────────

// SyncPolicy controls whether the reconciler can write back to the pin record.
type SyncPolicy string

const (
	// SyncReadOnly means the reconciler observes drift but does not apply bumps.
	SyncReadOnly SyncPolicy = "read-only"
	// SyncReadWrite means the reconciler can auto-bump the pin to track live HEAD.
	SyncReadWrite SyncPolicy = "read-write"
	// SyncMirror means the target is kept as an exact local mirror of the source.
	SyncMirror SyncPolicy = "mirror"
)

// PinRef holds the pinned position within a target workspace.
type PinRef struct {
	// Ref is a git tag or commit SHA. Required.
	Ref string `yaml:"ref"`
	// Digest is an optional content-addressed pin in "sha256:<hex>" format.
	// When present, reconcile verifies the resolved commit matches this digest.
	Digest string `yaml:"digest,omitempty"`
	// Node is an optional node hint — which node hosts the pinned version.
	// Informational only in v0; resolution uses the git ref.
	Node string `yaml:"node,omitempty"`
}

// PinRecord is the on-disk schema for a single pin relationship.
// Lives at <workspace>/.cog/pins/<sanitized-target-name>.yaml.
type PinRecord struct {
	// Target is the workspace name as registered in global.yaml (e.g. "myrgic/cogos").
	// Must resolve via global workspace registry (URIRegistry in #167).
	Target string `yaml:"target"`
	// Pin holds the pinned position.
	Pin PinRef `yaml:"pin"`
	// Branch is the default branch context for this relationship (e.g. "main").
	Branch string `yaml:"branch,omitempty"`
	// Sync controls write-back behaviour. Defaults to "read-only".
	Sync SyncPolicy `yaml:"sync,omitempty"`
	// Updated is the timestamp of the last explicit pin bump.
	Updated time.Time `yaml:"updated,omitempty"`

	// filePath is the absolute path from which this record was loaded.
	// Not serialized; set by loadPinRecords.
	filePath string
}

// LiveRef is the resolved current HEAD of a target workspace.
type LiveRef struct {
	// Target is the workspace name (mirrors PinRecord.Target).
	Target string
	// Ref is the current HEAD ref (commit SHA or tag).
	Ref string
	// Digest is the content digest of the resolved ref (if computable).
	Digest string
	// Reachable is false when the target workspace cannot be reached.
	Reachable bool
	// Error holds the resolution error when Reachable is false.
	Error error
}

// ─── Config bundle ───────────────────────────────────────────────────────────

// pinConfig is the LoadConfig output. It bundles all pin records for a
// source workspace so the rest of the lifecycle can operate without re-reading
// the filesystem.
type pinConfig struct {
	Root    string
	Records []*PinRecord
	PinsDir string
}

// ─── Live bundle ─────────────────────────────────────────────────────────────

// pinLive is the FetchLive output. For each declared pin, it holds the
// resolved live HEAD.
type pinLive struct {
	Refs map[string]*LiveRef // target → live ref
}

// ─── Provider ────────────────────────────────────────────────────────────────

// Provider implements reconcile.Reconcilable for the pin resource type.
//
// Construct via New; inject a GitHeadResolver for FetchLive. Tests supply
// a stubResolver; production defaults to localGitHeadResolver.
// Optionally inject a WorkspaceLocator via SetWorkspaceLocator after
// construction so FetchLive can consult the global workspace registry before
// falling back to local directory scanning.
//
// Thread safety: mu protects all mutable fields. LoadConfig, FetchLive, and
// the Health probe are the only callers that mutate state; they are sequenced
// by the reconcile harness, but the foveated context block calls Health()
// concurrently with the reconcile loop — hence the lock.
type Provider struct {
	mu sync.Mutex

	resolver GitHeadResolver
	locator  WorkspaceLocator // optional; nil = skip registry lookup

	// State updated through the lifecycle and surfaced by Health().
	root      string
	lastPlan  *reconcile.Plan
	pinStates map[string]*pinState // target → last-known state
	operation reconcile.OperationPhase
}

// pinState tracks per-pin health derived from the last reconcile pass.
type pinState struct {
	Target    string
	PinnedRef string
	LiveRef   string
	Sync      reconcile.SyncStatus
	Health    reconcile.HealthStatus
	Message   string
}

// New constructs a Provider. Pass nil for resolver to use the default
// local-checkout git resolver. Tests inject a stubResolver.
func New(resolver GitHeadResolver) *Provider {
	if resolver == nil {
		resolver = &localGitHeadResolver{}
	}
	return &Provider{
		resolver:  resolver,
		pinStates: make(map[string]*pinState),
		operation: reconcile.OperationIdle,
	}
}

// SetWorkspaceLocator injects a WorkspaceLocator so FetchLive can consult the
// global workspace registry before falling back to local directory scanning.
// Call this at daemon startup after the registry is initialised. Safe to call
// multiple times; the last value wins. Thread-safe.
func (p *Provider) SetWorkspaceLocator(loc WorkspaceLocator) {
	p.mu.Lock()
	p.locator = loc
	// Propagate to the underlying localGitHeadResolver if that is what we hold.
	if lgr, ok := p.resolver.(*localGitHeadResolver); ok {
		lgr.locator = loc
	}
	p.mu.Unlock()
}

// Type returns the provider identifier. Used by the reconcile registry and
// the "cog reconcile" dispatch.
func (p *Provider) Type() string { return "pin" }

// ─── GitHeadResolver ─────────────────────────────────────────────────────────

// GitHeadResolver resolves the live HEAD ref for a target workspace.
// The interface isolates FetchLive from filesystem and network I/O so tests
// can inject deterministic stubs.
type GitHeadResolver interface {
	// ResolveHead returns the live HEAD commit SHA for the target workspace
	// identified by name. workspaceRoot is the source workspace root (used for
	// relative-path fallback when target is a sibling checkout).
	//
	// TODO(#167): when URIRegistry lands, implementations should consult it for
	// canonical target path resolution before falling back to local checkout.
	ResolveHead(ctx context.Context, target, workspaceRoot string) (ref, digest string, err error)
}

// localGitHeadResolver is the production GitHeadResolver.
// It resolves target workspace paths using a two-step chain:
//  1. WorkspaceLocator (backed by URIRegistry) — canonical registry lookup.
//  2. Sibling-directory scan — local fallback when no locator is wired or the
//     registry does not know the workspace yet.
type localGitHeadResolver struct {
	locator WorkspaceLocator // injected via Provider.SetWorkspaceLocator
}

func (r *localGitHeadResolver) ResolveHead(ctx context.Context, target, workspaceRoot string) (string, string, error) {
	targetPath, locErr := r.locateTarget(ctx, target, workspaceRoot)
	if locErr != nil {
		return "", "", fmt.Errorf("pin: locating target %q: %w", target, locErr)
	}
	if targetPath == "" {
		return "", "", fmt.Errorf("%w: %q", ErrWorkspaceNotFound, target)
	}

	// git rev-parse HEAD in the target directory.
	cmd := exec.CommandContext(ctx, "git", "-C", targetPath, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("pin: resolving HEAD for %q at %s: %w", target, targetPath, err)
	}
	ref := strings.TrimSpace(string(out))
	return ref, "", nil // digest computation deferred to v1
}

// locateTarget finds the filesystem root for the target workspace.
//
// Lookup chain:
//  1. WorkspaceLocator.LocateWorkspace — consults the global workspace registry
//     (URIRegistry backed by ~/.cog/node/global.yaml). Returns immediately on
//     success or on any error that is NOT ErrWorkspaceNotFound.
//  2. Sibling-directory scan — local fallback for workspaces not yet in the
//     registry (e.g. during bootstrapping or federation setup).
//
// Returns ("", nil) when the workspace is simply not found anywhere.
// Returns ("", err) when the locator signals a hard registry failure.
func (r *localGitHeadResolver) locateTarget(ctx context.Context, target, workspaceRoot string) (string, error) {
	// Step 1: consult WorkspaceLocator when available.
	if r.locator != nil {
		path, err := r.locator.LocateWorkspace(ctx, target)
		if err == nil && path != "" {
			return path, nil
		}
		if err != nil && !errors.Is(err, ErrWorkspaceNotFound) {
			// Non-not-found error (I/O failure, parse error, registry
			// unavailable): propagate immediately so the registry failure
			// is visible to the caller rather than silently masked by a
			// stale sibling-directory scan.
			return "", err
		}
		// ErrWorkspaceNotFound → fall through to local sibling-directory scan.
	}

	if workspaceRoot == "" {
		return "", nil
	}

	// Normalise target: convert "/" to OS separator.
	// candidate 1: sibling of workspaceRoot with same name as last segment.
	// candidate 2: parent-level path matching full target.
	parent := filepath.Dir(workspaceRoot)
	grandParent := filepath.Dir(parent)

	// "myrgic/cogos" → last two components in sibling tree.
	parts := strings.SplitN(target, "/", 2)
	switch len(parts) {
	case 1:
		// single-component target (e.g., "cogos") — look as a sibling dir.
		candidate := filepath.Join(parent, parts[0])
		if isGitRepo(candidate) {
			return candidate, nil
		}
	case 2:
		// two-component target (e.g., "myrgic/cogos") — look one level up.
		candidate := filepath.Join(grandParent, parts[0], parts[1])
		if isGitRepo(candidate) {
			return candidate, nil
		}
		// Also try: sibling with last-segment name.
		candidateFlat := filepath.Join(parent, parts[1])
		if isGitRepo(candidateFlat) {
			return candidateFlat, nil
		}
	}
	return "", nil
}

// isGitRepo returns true when path is a directory containing a .git entry.
func isGitRepo(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	_, err = os.Stat(filepath.Join(path, ".git"))
	return err == nil
}

// ─── LoadConfig ──────────────────────────────────────────────────────────────

// LoadConfig reads all pin records from <root>/.cog/pins/*.yaml.
// Returns a *pinConfig as any; other methods type-assert it back.
// An empty pins directory is not an error — the provider is healthy with zero pins.
func (p *Provider) LoadConfig(root string) (any, error) {
	p.mu.Lock()
	p.root = root
	p.mu.Unlock()

	records, err := loadPinRecords(root)
	if err != nil {
		return nil, fmt.Errorf("pin provider: LoadConfig: %w", err)
	}

	return &pinConfig{
		Root:    root,
		Records: records,
		PinsDir: pinsDir(root),
	}, nil
}

// loadPinRecords reads all *.yaml files from <root>/.cog/pins/ and parses them
// as PinRecord. Files that fail to parse are skipped with a warning logged to
// stderr (not a hard error — a single malformed pin should not brick the whole
// reconcile cycle).
func loadPinRecords(root string) ([]*PinRecord, error) {
	dir := pinsDir(root)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil // no pins declared — healthy
	}
	if err != nil {
		return nil, fmt.Errorf("reading pins directory %s: %w", dir, err)
	}

	var records []*PinRecord
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		rec, err := parsePinFile(path)
		if err != nil {
			// Log and skip — one bad file should not prevent other pins from reconciling.
			fmt.Fprintf(os.Stderr, "pin provider: skipping %s: %v\n", path, err)
			continue
		}
		rec.filePath = path
		records = append(records, rec)
	}
	return records, nil
}

// parsePinFile decodes a single pin YAML file.
func parsePinFile(path string) (*PinRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var rec PinRecord
	if err := yaml.Unmarshal(data, &rec); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if rec.Target == "" {
		return nil, fmt.Errorf("pin record %s: missing required field 'target'", path)
	}
	if rec.Pin.Ref == "" {
		return nil, fmt.Errorf("pin record %s: missing required field 'pin.ref'", path)
	}
	if rec.Sync == "" {
		rec.Sync = SyncReadOnly
	}
	return &rec, nil
}

// pinsDir returns the absolute path of the pins directory for a workspace root.
func pinsDir(root string) string {
	return filepath.Join(root, ".cog", "pins")
}

// ─── FetchLive ───────────────────────────────────────────────────────────────

// FetchLive resolves the live HEAD for each declared target workspace.
// Config must be *pinConfig. Returns *pinLive as any.
// Resolution failures are captured per-target (Reachable=false) rather than
// failing the whole fetch — one unreachable target should not mask the rest.
func (p *Provider) FetchLive(ctx context.Context, config any) (any, error) {
	cfg, ok := config.(*pinConfig)
	if !ok {
		return nil, fmt.Errorf("pin: FetchLive expected *pinConfig, got %T", config)
	}

	live := &pinLive{Refs: make(map[string]*LiveRef, len(cfg.Records))}
	for _, rec := range cfg.Records {
		ref, digest, err := p.resolver.ResolveHead(ctx, rec.Target, cfg.Root)
		lr := &LiveRef{
			Target:    rec.Target,
			Reachable: err == nil,
		}
		if err != nil {
			lr.Error = err
		} else {
			lr.Ref = ref
			lr.Digest = digest
		}
		live.Refs[rec.Target] = lr
	}
	return live, nil
}

// ─── ComputePlan ─────────────────────────────────────────────────────────────

// ComputePlan diffs each pinned ref against the live HEAD.
// Emits one Action per pin:
//   - skip    if pinned ref matches live HEAD (and digest matches when declared)
//   - update  if pinned ref is behind live HEAD
//   - skip    with Health=Degraded if target unreachable
//   - skip    with Health=Degraded if digest declared but mismatched
func (p *Provider) ComputePlan(config any, live any, state *reconcile.State) (*reconcile.Plan, error) {
	cfg, ok := config.(*pinConfig)
	if !ok {
		return nil, fmt.Errorf("pin: ComputePlan expected *pinConfig, got %T", config)
	}
	lv, ok := live.(*pinLive)
	if !ok {
		return nil, fmt.Errorf("pin: ComputePlan expected *pinLive, got %T", live)
	}

	plan := &reconcile.Plan{
		ResourceType: "pin",
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		ConfigPath:   cfg.PinsDir,
	}

	newStates := make(map[string]*pinState, len(cfg.Records))

	for _, rec := range cfg.Records {
		lr := lv.Refs[rec.Target]
		ps := &pinState{
			Target:    rec.Target,
			PinnedRef: rec.Pin.Ref,
		}

		if lr == nil || !lr.Reachable {
			errMsg := "unreachable"
			if lr != nil && lr.Error != nil {
				errMsg = lr.Error.Error()
			}
			ps.Sync = reconcile.SyncStatusUnknown
			ps.Health = reconcile.HealthMissing
			ps.Message = fmt.Sprintf("target %q unreachable: %s", rec.Target, errMsg)
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action:       reconcile.ActionSkip,
				ResourceType: "pin",
				Name:         rec.Target,
				Details: map[string]any{
					"reason":     "target_unreachable",
					"pinned_ref": rec.Pin.Ref,
					"error":      errMsg,
				},
			})
			plan.Summary.Skipped++
			newStates[rec.Target] = ps
			continue
		}

		ps.LiveRef = lr.Ref

		// Digest verification (fail-closed per ADR-067 amendment).
		//
		// The local resolver (localGitHeadResolver) does not compute a content
		// digest — it returns empty string for digest. This is expected: git SHA
		// resolution does not produce a sha256 content hash without reading
		// and hashing the tree, which is deferred to v1 (TODO #167 / URIRegistry).
		//
		// However, per the ADR-067 fail-closed amendment: if the pin record
		// DECLARES a digest, the reconciler MUST treat an unverifiable live
		// digest as a RED condition rather than silently ignoring the integrity
		// constraint. Three cases:
		//   1. pinned == "" → no digest check; fall through to ref comparison.
		//   2. pinned != "" and live == "" → RED: "digest unverifiable".
		//   3. pinned != "" and live != "" and mismatch → RED: "digest mismatch".
		//   4. pinned != "" and live != "" and match → proceed to ref comparison.
		if rec.Pin.Digest != "" {
			pinnedDigest := normaliseDigest(rec.Pin.Digest)
			if lr.Digest == "" {
				// Case 2: declared digest, resolver returned nothing — fail closed.
				ps.Sync = reconcile.SyncStatusOutOfSync
				ps.Health = reconcile.HealthDegraded
				ps.Message = fmt.Sprintf("digest unverifiable: pin declares %s but resolver returned no digest (fail-closed per ADR-067)", pinnedDigest)
				plan.Actions = append(plan.Actions, reconcile.Action{
					Action:       reconcile.ActionSkip,
					ResourceType: "pin",
					Name:         rec.Target,
					Details: map[string]any{
						"reason":        "digest_unverifiable",
						"pinned_ref":    rec.Pin.Ref,
						"pinned_digest": rec.Pin.Digest,
						"live_ref":      lr.Ref,
					},
				})
				plan.Summary.Skipped++
				newStates[rec.Target] = ps
				continue
			}
			// Case 3: both present, compare.
			liveDigest := normaliseDigest(lr.Digest)
			if pinnedDigest != liveDigest {
				ps.Sync = reconcile.SyncStatusOutOfSync
				ps.Health = reconcile.HealthDegraded
				ps.Message = fmt.Sprintf("digest mismatch: want %s, got %s", pinnedDigest, liveDigest)
				plan.Actions = append(plan.Actions, reconcile.Action{
					Action:       reconcile.ActionSkip,
					ResourceType: "pin",
					Name:         rec.Target,
					Details: map[string]any{
						"reason":        "digest_mismatch",
						"pinned_ref":    rec.Pin.Ref,
						"pinned_digest": rec.Pin.Digest,
						"live_ref":      lr.Ref,
						"live_digest":   lr.Digest,
					},
				})
				plan.Summary.Skipped++
				newStates[rec.Target] = ps
				continue
			}
			// Case 4: match — fall through to ref comparison.
		}

		// Ref comparison.
		if refsMatch(rec.Pin.Ref, lr.Ref) {
			ps.Sync = reconcile.SyncStatusSynced
			ps.Health = reconcile.HealthHealthy
			ps.Message = fmt.Sprintf("pinned at %s, live HEAD matches", shortRef(rec.Pin.Ref))
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action:       reconcile.ActionSkip,
				ResourceType: "pin",
				Name:         rec.Target,
				Details: map[string]any{
					"reason":     "in_sync",
					"pinned_ref": rec.Pin.Ref,
					"live_ref":   lr.Ref,
				},
			})
			plan.Summary.Skipped++
		} else {
			ps.Sync = reconcile.SyncStatusOutOfSync
			ps.Health = reconcile.HealthDegraded
			ps.Message = fmt.Sprintf("drift: pinned %s, live HEAD %s", shortRef(rec.Pin.Ref), shortRef(lr.Ref))
			ps.LiveRef = lr.Ref
			plan.Actions = append(plan.Actions, reconcile.Action{
				Action:       reconcile.ActionUpdate,
				ResourceType: "pin",
				Name:         rec.Target,
				Details: map[string]any{
					"pinned_ref":  rec.Pin.Ref,
					"live_ref":    lr.Ref,
					"sync_policy": string(rec.Sync),
					"branch":      rec.Branch,
					"file":        rec.filePath,
				},
			})
			plan.Summary.Updates++
		}
		newStates[rec.Target] = ps
	}

	p.mu.Lock()
	p.lastPlan = plan
	p.pinStates = newStates
	p.mu.Unlock()

	return plan, nil
}

// refsMatch returns true if two git refs refer to the same commit.
// Handles: exact match, short SHA prefix match (7-char minimum).
func refsMatch(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == b {
		return true
	}
	// SHA prefix: if one is a prefix of the other and at least 7 chars long.
	shorter, longer := a, b
	if len(a) > len(b) {
		shorter, longer = b, a
	}
	return len(shorter) >= 7 && strings.HasPrefix(longer, shorter)
}

// shortRef truncates a full SHA to 12 chars for display.
func shortRef(ref string) string {
	if len(ref) > 12 && isHexString(ref) {
		return ref[:12]
	}
	return ref
}

// isHexString returns true if s looks like a hex string.
func isHexString(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

// normaliseDigest strips the "sha256:" prefix and lowercases the hex portion
// for comparison.
func normaliseDigest(d string) string {
	d = strings.ToLower(strings.TrimSpace(d))
	if after, ok := strings.CutPrefix(d, "sha256:"); ok {
		return "sha256:" + after
	}
	return d
}

// ─── ApplyPlan ───────────────────────────────────────────────────────────────

// ApplyPlan executes update actions from the plan.
// For sync:read-only pins, update actions are skipped with ApplySkipped.
// For sync:read-write pins, the pin YAML file is updated with the new ref.
// For sync:mirror — not yet implemented in v0; treated as read-write.
func (p *Provider) ApplyPlan(ctx context.Context, plan *reconcile.Plan) ([]reconcile.Result, error) {
	results := make([]reconcile.Result, 0, len(plan.Actions))

	for _, action := range plan.Actions {
		if action.Action != reconcile.ActionUpdate {
			continue
		}
		target := action.Name
		syncPolicy, _ := action.Details["sync_policy"].(string)
		filePath, _ := action.Details["file"].(string)
		newRef, _ := action.Details["live_ref"].(string)

		// read-only: refuse the write-back.
		if SyncPolicy(syncPolicy) == SyncReadOnly || syncPolicy == "" {
			results = append(results, reconcile.Result{
				Phase:  "apply",
				Action: string(reconcile.ActionUpdate),
				Name:   target,
				Status: reconcile.ApplySkipped,
				Error:  "sync: read-only — bump requires explicit `cog pin bump`",
			})
			continue
		}

		if filePath == "" {
			results = append(results, reconcile.Result{
				Phase:  "apply",
				Action: string(reconcile.ActionUpdate),
				Name:   target,
				Status: reconcile.ApplyFailed,
				Error:  "pin file path not set in plan details",
			})
			continue
		}

		if err := bumpPinFile(filePath, newRef); err != nil {
			results = append(results, reconcile.Result{
				Phase:  "apply",
				Action: string(reconcile.ActionUpdate),
				Name:   target,
				Status: reconcile.ApplyFailed,
				Error:  err.Error(),
			})
		} else {
			results = append(results, reconcile.Result{
				Phase:  "apply",
				Action: string(reconcile.ActionUpdate),
				Name:   target,
				Status: reconcile.ApplySucceeded,
			})
		}
	}
	return results, nil
}

// bumpPinFile rewrites the pin.ref field in the YAML file to newRef and
// updates the updated timestamp. It round-trips through yaml.v3 to preserve
// comments and formatting as much as possible.
func bumpPinFile(path, newRef string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("bump: reading %s: %w", path, err)
	}
	var rec PinRecord
	if err := yaml.Unmarshal(data, &rec); err != nil {
		return fmt.Errorf("bump: parsing %s: %w", path, err)
	}
	rec.Pin.Ref = newRef
	rec.Updated = time.Now().UTC()

	out, err := yaml.Marshal(&rec)
	if err != nil {
		return fmt.Errorf("bump: marshalling %s: %w", path, err)
	}
	if err := os.WriteFile(path, out, 0o644); err != nil {
		return fmt.Errorf("bump: writing %s: %w", path, err)
	}
	return nil
}

// ─── BuildState ──────────────────────────────────────────────────────────────

// BuildState constructs a reconcile.State snapshot from the live pin data.
// Used for `cog state pin` and import operations.
func (p *Provider) BuildState(config any, live any, existing *reconcile.State) (*reconcile.State, error) {
	cfg, ok := config.(*pinConfig)
	if !ok {
		return nil, fmt.Errorf("pin: BuildState expected *pinConfig, got %T", config)
	}
	lv, ok := live.(*pinLive)
	if !ok {
		return nil, fmt.Errorf("pin: BuildState expected *pinLive, got %T", live)
	}

	lineage := "pin"
	serial := 1
	if existing != nil {
		lineage = existing.Lineage
		serial = existing.Serial + 1
	}

	state := &reconcile.State{
		Version:      1,
		Lineage:      lineage,
		Serial:       serial,
		ResourceType: "pin",
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
	}

	for _, rec := range cfg.Records {
		lr := lv.Refs[rec.Target]
		attrs := map[string]any{
			"target":     rec.Target,
			"pinned_ref": rec.Pin.Ref,
			"sync":       string(rec.Sync),
			"branch":     rec.Branch,
		}
		if rec.Pin.Digest != "" {
			attrs["pinned_digest"] = rec.Pin.Digest
		}
		if lr != nil && lr.Reachable {
			attrs["live_ref"] = lr.Ref
			attrs["reachable"] = true
		} else {
			attrs["reachable"] = false
		}

		state.Resources = append(state.Resources, reconcile.Resource{
			Address:       "pin." + sanitiseName(rec.Target),
			Type:          "pin",
			Mode:          reconcile.ModeManaged,
			ExternalID:    rec.Target,
			Name:          rec.Target,
			Attributes:    attrs,
			LastRefreshed: time.Now().UTC().Format(time.RFC3339),
		})
	}
	return state, nil
}

// sanitiseName converts "myrgic/cogos" → "myrgic_cogos" for use as a
// Reconcile State address component.
func sanitiseName(name string) string {
	return strings.ReplaceAll(name, "/", "_")
}

// ─── Health ───────────────────────────────────────────────────────────────────

// Health returns the aggregate three-axis status across all pins.
//
//   - No pins declared → Healthy (nothing to drift on)
//   - All pins in sync → Healthy, Synced
//   - Any pin unreachable → Degraded, OutOfSync (can't verify)
//   - Any digest mismatch → Degraded, OutOfSync (integrity violation)
//   - Any ref drift (behind live) → Degraded, OutOfSync
//   - workspace root not yet configured → Missing, Unknown
func (p *Provider) Health() reconcile.ResourceStatus {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.root == "" {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthMissing,
			Operation: reconcile.OperationIdle,
			Message:   "workspace root not yet configured",
		}
	}

	if len(p.pinStates) == 0 {
		// Check whether the pins dir exists — if not, no pins declared at all.
		dir := pinsDir(p.root)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			return reconcile.ResourceStatus{
				Sync:      reconcile.SyncStatusSynced,
				Health:    reconcile.HealthHealthy,
				Operation: reconcile.OperationIdle,
				Message:   "no pins declared",
			}
		}
	}

	total := len(p.pinStates)
	if total == 0 {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusSynced,
			Health:    reconcile.HealthHealthy,
			Operation: reconcile.OperationIdle,
			Message:   "no pins declared",
		}
	}

	missing, drifted, synced := 0, 0, 0
	var msgs []string
	for _, ps := range p.pinStates {
		switch ps.Health {
		case reconcile.HealthMissing:
			missing++
			msgs = append(msgs, ps.Message)
		case reconcile.HealthDegraded:
			drifted++
			msgs = append(msgs, ps.Message)
		default:
			synced++
		}
	}

	switch {
	case missing > 0:
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusOutOfSync,
			Health:    reconcile.HealthDegraded,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("%d/%d unreachable: %s", missing, total, firstN(msgs, 2)),
		}
	case drifted > 0:
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusOutOfSync,
			Health:    reconcile.HealthDegraded,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("%d/%d pins drifted: %s", drifted, total, firstN(msgs, 2)),
		}
	default:
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusSynced,
			Health:    reconcile.HealthHealthy,
			Operation: reconcile.OperationIdle,
			Message:   fmt.Sprintf("%d/%d pins in sync", synced, total),
		}
	}
}

// firstN joins the first n messages from msgs.
func firstN(msgs []string, n int) string {
	if len(msgs) <= n {
		return strings.Join(msgs, "; ")
	}
	return strings.Join(msgs[:n], "; ") + fmt.Sprintf(" (+%d more)", len(msgs)-n)
}

// ─── RefreshState (read-only cycle) ─────────────────────────────────────────

// RefreshState runs the read-only subset of the reconcile lifecycle:
// LoadConfig → FetchLive → ComputePlan (no ApplyPlan, no filesystem writes).
//
// This is the correct method for health probes. Unlike Reconcile, it never
// calls ApplyPlan and therefore never calls bumpPinFile, so it is safe to call
// from Health() without risk of mutating pin YAML files.
func (p *Provider) RefreshState(ctx context.Context, root string) error {
	p.mu.Lock()
	p.operation = reconcile.OperationSyncing
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.operation = reconcile.OperationIdle
		p.mu.Unlock()
	}()

	cfg, err := p.LoadConfig(root)
	if err != nil {
		return err
	}
	live, err := p.FetchLive(ctx, cfg)
	if err != nil {
		return err
	}
	_, err = p.ComputePlan(cfg, live, nil)
	return err
}

// ─── Reconcile (full cycle) ──────────────────────────────────────────────────

// Reconcile runs the full LoadConfig → FetchLive → ComputePlan → ApplyPlan cycle.
// It is called by the kernel reconcile harness on each tick.
func (p *Provider) Reconcile(ctx context.Context, root string) ([]reconcile.Result, error) {
	p.mu.Lock()
	p.operation = reconcile.OperationSyncing
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.operation = reconcile.OperationIdle
		p.mu.Unlock()
	}()

	cfg, err := p.LoadConfig(root)
	if err != nil {
		return nil, err
	}
	live, err := p.FetchLive(ctx, cfg)
	if err != nil {
		return nil, err
	}
	plan, err := p.ComputePlan(cfg, live, nil)
	if err != nil {
		return nil, err
	}
	return p.ApplyPlan(ctx, plan)
}

// ─── Verify (read-only plan-driven check) ────────────────────────────────────

// VerifyResult describes the outcome for a single pin after a verify run.
type VerifyResult struct {
	Target  string
	OK      bool
	Message string
}

// RunVerify runs the read-only pin verification cycle against root and writes
// human-readable output to w. filterTarget, if non-empty, restricts output to
// that single pin. resolver, if non-nil, overrides the default local git
// resolver (useful for testing).
//
// Returns a non-nil error when any pin is drifted or unreachable, or when
// filterTarget is specified but no matching record exists.
//
// cmdPinVerify in cmd_pin.go is a thin shim over this function that supplies
// the resolved workspace root, os.Stdout, and a nil resolver.
func RunVerify(ctx context.Context, root, filterTarget string, resolver GitHeadResolver, w io.Writer) ([]VerifyResult, error) {
	p := New(resolver) // nil resolver → default localGitHeadResolver
	cfgAny, err := p.LoadConfig(root)
	if err != nil {
		return nil, fmt.Errorf("pin verify: %w", err)
	}
	liveAny, err := p.FetchLive(ctx, cfgAny)
	if err != nil {
		return nil, fmt.Errorf("pin verify: %w", err)
	}
	plan, err := p.ComputePlan(cfgAny, liveAny, nil)
	if err != nil {
		return nil, fmt.Errorf("pin verify: %w", err)
	}

	// Build results from the authoritative plan.
	type vr struct {
		ok      bool
		message string
	}
	byTarget := make(map[string]vr, len(plan.Actions))
	for _, action := range plan.Actions {
		target := action.Name
		reason, _ := action.Details["reason"].(string)
		pinnedRef, _ := action.Details["pinned_ref"].(string)
		liveRef, _ := action.Details["live_ref"].(string)

		switch reason {
		case "in_sync":
			byTarget[target] = vr{ok: true,
				message: fmt.Sprintf("pinned %s == live %s", pinnedRef, liveRef)}
		case "digest_unverifiable":
			pinnedDigest, _ := action.Details["pinned_digest"].(string)
			byTarget[target] = vr{ok: false,
				message: fmt.Sprintf("digest unverifiable: pin declares %s, resolver returned no digest (fail-closed)", pinnedDigest)}
		case "digest_mismatch":
			pinnedDigest, _ := action.Details["pinned_digest"].(string)
			liveDigest, _ := action.Details["live_digest"].(string)
			byTarget[target] = vr{ok: false,
				message: fmt.Sprintf("digest mismatch: want %s, got %s (ref: pinned %s, live %s)", pinnedDigest, liveDigest, pinnedRef, liveRef)}
		case "target_unreachable":
			errStr, _ := action.Details["error"].(string)
			byTarget[target] = vr{ok: false,
				message: fmt.Sprintf("unreachable: %s", errStr)}
		default:
			// ActionUpdate: ref drift (no digest involved).
			if liveRef != "" {
				byTarget[target] = vr{ok: false,
					message: fmt.Sprintf("drift: pinned %s, live %s", pinnedRef, liveRef)}
			} else {
				byTarget[target] = vr{ok: false,
					message: fmt.Sprintf("unexpected plan action %q for %s", action.Action, target)}
			}
		}
	}

	var results []VerifyResult
	found := false
	drifted := 0
	for target, r := range byTarget {
		if filterTarget != "" && target != filterTarget {
			continue
		}
		found = true
		results = append(results, VerifyResult{Target: target, OK: r.ok, Message: r.message})
		if r.ok {
			fmt.Fprintf(w, "[OK]    %s: %s\n", target, r.message)
		} else {
			fmt.Fprintf(w, "[DRIFT] %s: %s\n", target, r.message)
			drifted++
		}
	}

	if filterTarget != "" && !found {
		return nil, fmt.Errorf("pin verify: no pin record found for target %q", filterTarget)
	}
	if drifted > 0 {
		return results, fmt.Errorf("pin verify: %d pin(s) drifted or unreachable", drifted)
	}
	return results, nil
}

// ─── File helpers (used by CLI write operations) ─────────────────────────────

// WritePinRecord serialises rec to <root>/.cog/pins/<filename>.yaml.
// Creates the pins directory if it does not exist.
func WritePinRecord(root string, rec *PinRecord) error {
	dir := pinsDir(root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("pin: creating pins dir %s: %w", dir, err)
	}
	filename := sanitiseFilename(rec.Target) + ".yaml"
	path := filepath.Join(dir, filename)

	data, err := yaml.Marshal(rec)
	if err != nil {
		return fmt.Errorf("pin: marshalling %s: %w", rec.Target, err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("pin: writing %s: %w", path, err)
	}
	return nil
}

// RemovePinRecord deletes the pin YAML for target from the workspace.
func RemovePinRecord(root, target string) error {
	filename := sanitiseFilename(target) + ".yaml"
	path := filepath.Join(pinsDir(root), filename)
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("pin: no pin record found for target %q", target)
		}
		return fmt.Errorf("pin: removing %s: %w", path, err)
	}
	return nil
}

// sanitiseFilename converts a target workspace name to a safe filename stem.
// "myrgic/cogos" → "myrgic_cogos"
func sanitiseFilename(target string) string {
	return strings.NewReplacer("/", "_", ":", "_", " ", "_").Replace(target)
}
