// identity_provider.go
// IdentityProvider implements pkg/reconcile.Reconcilable for the Identity CRD.
//
// Responsibility split (per the project_cogos_identity_as_crd triple):
//   - identity_crd.go    — Spec: on-disk YAML at .cog/config/identities/<sub>.yaml
//   - identity_provider.go — Reconciler + Projections: what writes to DB + cogdoc
//
// This file is the second leg of the triple. It consumes the Spec from
// identity_crd.go, produces Projections into:
//
//   - the Constellation memory-graph (participants table + identity cogdocs)
//   - a short projection path .cog/id/<sub>.cog.md (Layer-2 principal card)
//
// and emits an `identity.*` event for every Apply action so the ledger tells
// the truth about what happened.
//
// All side-effects are reached through narrow dependencies (ConstellationDB,
// KeyResolver, busEmit) so the provider is testable without standing up a
// real SQLite file, a real key vault, or a real ledger.
//
// Not covered in this wave:
//   - concrete wiring to sdk/constellation/db.go (that is a later wave —
//     the interface here is the integration point)
//   - live enforcement of AuthFactors (the spec-shape lives in identity_crd.go
//     now; evaluating a challenge is a follow-up)

package main

import (
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	stdhash "hash"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// ─── DB surface ─────────────────────────────────────────────────────────────────

// ConstellationDB is the narrow dependency-injection surface the
// IdentityProvider needs to read/write projections. A concrete implementation
// wraps *sdk/constellation.DB (out of scope for this wave). Tests supply an
// in-memory fake.
//
// Design: projections (identity cogdocs) and the participants table are
// two tables in the same store. A single interface keeps the boundary clear
// and lets the provider atomically coordinate both when real transactions
// land later.
type ConstellationDB interface {
	UpsertIdentityCogDoc(ctx context.Context, doc IdentityProjection) error
	DeleteIdentityCogDoc(ctx context.Context, sub string) error
	UpsertParticipant(ctx context.Context, row ParticipantRow) error
	DeleteParticipant(ctx context.Context, id string) error
	GetProjection(ctx context.Context, sub string) (*IdentityProjection, error)
	ListProjections(ctx context.Context) ([]IdentityProjection, error)
}

// IdentityProjection is the data the provider writes per reconciled identity.
// Mirrors the spec's identity-relevant fields plus provenance so downstream
// consumers (e.g. dashboards, the agent runtime) can answer "where did this
// identity come from?" without re-reading the YAML.
type IdentityProjection struct {
	Sub           string               `json:"sub"`
	Iss           string               `json:"iss"`
	Type          string               `json:"type"`
	ProjectedAt   time.Time            `json:"projected_at"`
	SpecHash      string               `json:"spec_hash"`
	Expressions   []IdentityExpression `json:"expressions"`
	ContentPath   string               `json:"content_path"`
	// WorkspaceRoot carries the workspace_root from the primary (catch-all or
	// first) expression so callers can resolve the identity's home URI without
	// iterating the full expression list.
	WorkspaceRoot string               `json:"workspace_root,omitempty"`
	// VoiceProfile carries the voice_profile from the primary (catch-all or
	// first) expression so callers can resolve the identity's voice config
	// without iterating the full expression list. Both generative (TTS) and
	// discriminative (speaker recognition) heads are carried when present.
	VoiceProfile *VoiceProfile `json:"voice_profile,omitempty"`
}

// ParticipantRow matches the kernel's existing `participants` table schema
// (see docs/architecture/the-constellation.md). Per convention the row id
// is "<type>:<sub>", with type ∈ {agent, human, service}.
type ParticipantRow struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"`
	Name         string    `json:"name"`
	IdentityPath string    `json:"identity_path"`
	SessionID    string    `json:"session_id"`
	Active       bool      `json:"active"`
	LastSeen     time.Time `json:"last_seen"`
	RegisteredAt time.Time `json:"registered_at"`
	NodeHash     string    `json:"node_hash"`
}

// ─── Key resolution ─────────────────────────────────────────────────────────────

// ErrSchemeNotImplemented is returned when a KeyRef uses a scheme that is
// declared in the CRD allow-list but not yet wired up (vault://, s3://, etc.).
var ErrSchemeNotImplemented = errors.New("identity: key-ref scheme not implemented")

// KeyResolver resolves key-ref URIs to raw key bytes. Each scheme
// (file://, inline://, vault://, s3://, …) has its own resolver; the
// provider multiplexes them through resolverRegistry.
//
// Resolvers MUST NOT verify the integrity hash — that is the provider's job,
// so that misimplemented resolvers cannot launder bad bytes.
type KeyResolver interface {
	ResolveKey(ctx context.Context, ref string) ([]byte, error)
}

// fileKeyResolver reads key bytes from a local file path (file://<abs>).
type fileKeyResolver struct{}

func (fileKeyResolver) ResolveKey(_ context.Context, ref string) ([]byte, error) {
	path := strings.TrimPrefix(ref, "file://")
	if path == "" {
		return nil, fmt.Errorf("identity: file:// ref missing path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("identity: read file key %q: %w", path, err)
	}
	return data, nil
}

// inlineKeyResolver decodes base64 payloads embedded directly in the ref.
// Form: inline://<base64-std-padded>. Intended for tests and tiny dev keys;
// the CRD loader allows it but operators should avoid it in production.
type inlineKeyResolver struct{}

func (inlineKeyResolver) ResolveKey(_ context.Context, ref string) ([]byte, error) {
	payload := strings.TrimPrefix(ref, "inline://")
	if payload == "" {
		return nil, fmt.Errorf("identity: inline:// ref missing payload")
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, fmt.Errorf("identity: decode inline key: %w", err)
	}
	return data, nil
}

// notImplementedResolver is a placeholder for scheme-allowed-but-unwired
// schemes. Keeping a single struct reduces boilerplate.
type notImplementedResolver struct{ scheme string }

func (r notImplementedResolver) ResolveKey(_ context.Context, _ string) ([]byte, error) {
	return nil, fmt.Errorf("%w: %s", ErrSchemeNotImplemented, r.scheme)
}

// defaultKeyResolvers returns the scheme→resolver map used when the provider
// is constructed without an explicit resolver registry. Tests may override
// specific schemes via NewIdentityProvider's resolvers argument.
func defaultKeyResolvers() map[string]KeyResolver {
	return map[string]KeyResolver{
		"file":     fileKeyResolver{},
		"inline":   inlineKeyResolver{},
		"vault":    notImplementedResolver{scheme: "vault"},
		"s3":       notImplementedResolver{scheme: "s3"},
		"keychain": notImplementedResolver{scheme: "keychain"},
		"env":      notImplementedResolver{scheme: "env"},
		"kms":      notImplementedResolver{scheme: "kms"},
	}
}

// verifyKeyIntegrity checks that sha256/sha512 of key bytes matches the
// CRD's declared integrity hash. Returns an error (not just a bool) so the
// caller can surface algorithm errors distinctly.
func verifyKeyIntegrity(keyBytes []byte, integrityHash string) error {
	algo, expected, ok := strings.Cut(integrityHash, ":")
	if !ok {
		return fmt.Errorf("identity: malformed integrity_hash %q", integrityHash)
	}
	var h stdhash.Hash
	switch algo {
	case "sha256":
		h = sha256.New()
	case "sha512":
		h = sha512.New()
	default:
		return fmt.Errorf("identity: unsupported hash algorithm %q", algo)
	}
	h.Write(keyBytes)
	got := hex.EncodeToString(h.Sum(nil))
	if got != expected {
		return fmt.Errorf("identity: key integrity mismatch: want %s, got %s", expected, got)
	}
	return nil
}

// ─── Event emission ─────────────────────────────────────────────────────────────

// BusEmit is the callback shape the provider uses to emit events. Passing
// it as a function (rather than importing AppendEvent directly) keeps the
// provider unit-testable: tests capture into a slice, production wires to
// engine.AppendEvent (or any equivalent transport).
type BusEmit func(eventType string, data map[string]any) error

// Identity event-type constants. Dot-separated lowercase per event-schema
// convention. Source metadata is always "reconciler/identity".
const (
	EventIdentityReconciled        = "identity.reconciled"
	EventIdentityProjected         = "identity.projected"
	EventIdentityExpressionUpdated = "identity.expression.updated"
	EventIdentityDeregistered      = "identity.deregistered"
	EventIdentityKeyRotated        = "identity.key.rotated"
	EventIdentityKeyMismatch       = "identity.key.mismatch"
	EventIdentityAuthRequired      = "identity.auth.required"
)

const identityEventSource = "reconciler/identity"

// ─── Provider ───────────────────────────────────────────────────────────────────

// IdentityProvider implements Reconcilable for the Identity CRD.
//
// Construction is explicit (see NewIdentityProvider) because DB and event-bus
// wiring are external concerns — the kernel's reconcile harness wires them
// at startup, tests wire fakes. There is no init()-style RegisterProvider
// here; that is the kernel's wiring step in a later wave.
type IdentityProvider struct {
	mu sync.Mutex

	db        ConstellationDB
	resolvers map[string]KeyResolver
	emit      BusEmit

	// State populated during the reconcile loop and surfaced in Health.
	root             string
	lastPlanSummary  ReconcileSummary
	lastMismatchSubs map[string]struct{} // sub → present means "bad hash"
	missingSpecSubs  map[string]struct{} // sub → present means "projection without spec"
	operation        OperationPhase
}

// NewIdentityProvider constructs a provider with explicit dependencies. A
// nil resolvers map falls back to defaultKeyResolvers. A nil emit silently
// drops events (safe default for tests that don't assert on events).
func NewIdentityProvider(db ConstellationDB, resolvers map[string]KeyResolver, emit BusEmit) *IdentityProvider {
	if resolvers == nil {
		resolvers = defaultKeyResolvers()
	}
	if emit == nil {
		emit = func(string, map[string]any) error { return nil }
	}
	return &IdentityProvider{
		db:               db,
		resolvers:        resolvers,
		emit:             emit,
		lastMismatchSubs: make(map[string]struct{}),
		missingSpecSubs:  make(map[string]struct{}),
		operation:        OperationIdle,
	}
}

// Type returns the provider identifier. Matches CRD kind lowercase per
// reconciler convention.
func (p *IdentityProvider) Type() string { return "identity" }

// ─── LoadConfig ─────────────────────────────────────────────────────────────────

// identityConfig is the provider's internal config bundle. We keep raw CRDs
// plus a precomputed per-sub spec hash so ComputePlan can diff cheaply.
type identityConfig struct {
	Root      string
	CRDs      []*IdentityCRD
	SpecHash  map[string]string // sub → "sha256:<hex>" of the canonicalized spec
	ConfigDir string
}

// LoadConfig reads all identity CRDs from .cog/config/identities/ and
// computes a spec hash per identity for drift detection. The spec hash is
// sha256 of the canonical YAML of spec (not the whole file) so a cosmetic
// comment edit does not trigger a projection update.
func (p *IdentityProvider) LoadConfig(root string) (any, error) {
	p.mu.Lock()
	p.root = root
	p.mu.Unlock()

	crds, err := LoadIdentityCRDs(root)
	if err != nil {
		return nil, fmt.Errorf("identity provider: load config: %w", err)
	}
	if crds == nil {
		crds = []*IdentityCRD{}
	}

	cfg := &identityConfig{
		Root:      root,
		CRDs:      crds,
		SpecHash:  make(map[string]string, len(crds)),
		ConfigDir: identityCRDDir(root),
	}
	for _, crd := range crds {
		hash, err := hashIdentitySpec(&crd.Spec)
		if err != nil {
			return nil, fmt.Errorf("identity provider: hash spec %q: %w", crd.Spec.Subject, err)
		}
		cfg.SpecHash[crd.Spec.Subject] = hash
	}
	return cfg, nil
}

// hashIdentitySpec produces a stable "sha256:<hex>" over the YAML-marshaled
// spec. Using yaml.Marshal with default sort order keeps the hash stable for
// equivalent specs without pulling in a dedicated RFC-8785 canonicalizer.
func hashIdentitySpec(spec *IdentityCRDSpec) (string, error) {
	b, err := yaml.Marshal(spec)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// ─── FetchLive ──────────────────────────────────────────────────────────────────

// identityLive is the provider's snapshot of the current projection state.
type identityLive struct {
	Projections map[string]IdentityProjection // sub → projection
}

// FetchLive reads all identity projections from the database. Missing DB
// is a hard error — we cannot reconcile without knowing current state. An
// empty projection set (no rows) is not an error.
func (p *IdentityProvider) FetchLive(ctx context.Context, _ any) (any, error) {
	if p.db == nil {
		return nil, fmt.Errorf("identity provider: no ConstellationDB configured")
	}
	rows, err := p.db.ListProjections(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity provider: list projections: %w", err)
	}
	live := &identityLive{Projections: make(map[string]IdentityProjection, len(rows))}
	for _, row := range rows {
		live.Projections[row.Sub] = row
	}
	return live, nil
}

// ─── ComputePlan ────────────────────────────────────────────────────────────────

// ComputePlan compares the declared specs against the live projections:
//   - spec present, projection absent  → create
//   - spec present, projection present, spec-hash differs → update
//   - spec absent, projection present  → delete
//   - spec present, projection present, spec-hash matches → skip
//
// Actions are sorted by (action-class, sub) for deterministic output.
func (p *IdentityProvider) ComputePlan(config any, live any, _ *ReconcileState) (*ReconcilePlan, error) {
	cfg, ok := config.(*identityConfig)
	if !ok {
		return nil, fmt.Errorf("identity provider: expected *identityConfig, got %T", config)
	}
	liveState, ok := live.(*identityLive)
	if !ok {
		return nil, fmt.Errorf("identity provider: expected *identityLive, got %T", live)
	}

	plan := &ReconcilePlan{
		ResourceType: "identity",
		GeneratedAt:  nowISO(),
		ConfigPath:   cfg.ConfigDir,
		Metadata:     map[string]any{},
	}

	seen := make(map[string]struct{}, len(cfg.CRDs))

	// Walk declared specs.
	for _, crd := range cfg.CRDs {
		sub := crd.Spec.Subject
		seen[sub] = struct{}{}
		specHash := cfg.SpecHash[sub]
		existing, hasProjection := liveState.Projections[sub]

		switch {
		case !hasProjection:
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionCreate,
				ResourceType: "identity",
				Name:         sub,
				Details: map[string]any{
					"iss":       crd.Spec.Issuer,
					"type":      crd.Spec.Type,
					"spec_hash": specHash,
				},
			})
			plan.Summary.Creates++

		case existing.SpecHash != specHash:
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionUpdate,
				ResourceType: "identity",
				Name:         sub,
				Details: map[string]any{
					"iss":                crd.Spec.Issuer,
					"type":               crd.Spec.Type,
					"spec_hash":          specHash,
					"previous_spec_hash": existing.SpecHash,
				},
			})
			plan.Summary.Updates++

		default:
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action:       ActionSkip,
				ResourceType: "identity",
				Name:         sub,
				Details: map[string]any{
					"reason":    "in sync",
					"spec_hash": specHash,
				},
			})
			plan.Summary.Skipped++
		}
	}

	// Any projection without a matching spec is a delete.
	for sub, proj := range liveState.Projections {
		if _, ok := seen[sub]; ok {
			continue
		}
		plan.Actions = append(plan.Actions, ReconcileAction{
			Action:       ActionDelete,
			ResourceType: "identity",
			Name:         sub,
			Details: map[string]any{
				"iss":                proj.Iss,
				"type":               proj.Type,
				"previous_spec_hash": proj.SpecHash,
			},
		})
		plan.Summary.Deletes++
	}

	// Deterministic order so test assertions and diff viewers are stable.
	sort.Slice(plan.Actions, func(i, j int) bool {
		if plan.Actions[i].Action != plan.Actions[j].Action {
			return plan.Actions[i].Action < plan.Actions[j].Action
		}
		return plan.Actions[i].Name < plan.Actions[j].Name
	})

	// Remember summary for Health() — OutOfSync is simply "plan had any
	// non-skip action".
	p.mu.Lock()
	p.lastPlanSummary = plan.Summary
	p.missingSpecSubs = make(map[string]struct{}, plan.Summary.Deletes)
	for _, a := range plan.Actions {
		if a.Action == ActionDelete {
			p.missingSpecSubs[a.Name] = struct{}{}
		}
	}
	p.mu.Unlock()

	return plan, nil
}

// ─── ApplyPlan ──────────────────────────────────────────────────────────────────

// ApplyPlan executes every non-skip action in order, emitting exactly one
// event per action. Key-hash mismatches produce ApplyFailed + an
// identity.key.mismatch event; the provider does NOT abort the whole plan
// on a single mismatch (other identities can still reconcile).
func (p *IdentityProvider) ApplyPlan(ctx context.Context, plan *ReconcilePlan) ([]ReconcileResult, error) {
	if plan == nil {
		return nil, fmt.Errorf("identity provider: nil plan")
	}
	if p.db == nil {
		return nil, fmt.Errorf("identity provider: no ConstellationDB configured")
	}

	p.mu.Lock()
	p.operation = OperationSyncing
	root := p.root
	p.lastMismatchSubs = make(map[string]struct{})
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.operation = OperationIdle
		p.mu.Unlock()
	}()

	// Reload CRDs keyed by sub so Apply can access the full spec. (The plan
	// only carries summary details — the ApplyPlan contract says we can
	// trust the on-disk spec at apply time.)
	crds, err := LoadIdentityCRDs(root)
	if err != nil {
		return nil, fmt.Errorf("identity provider: reload specs: %w", err)
	}
	crdBySub := make(map[string]*IdentityCRD, len(crds))
	for _, c := range crds {
		crdBySub[c.Spec.Subject] = c
	}

	var results []ReconcileResult
	for _, action := range plan.Actions {
		if action.Action == ActionSkip {
			continue
		}

		res := ReconcileResult{
			Phase:  "identity",
			Action: string(action.Action),
			Name:   action.Name,
		}

		switch action.Action {
		case ActionCreate, ActionUpdate:
			crd, ok := crdBySub[action.Name]
			if !ok {
				res.Status = ApplyFailed
				res.Error = fmt.Sprintf("spec for %q disappeared between plan and apply", action.Name)
				results = append(results, res)
				continue
			}

			// Idempotency short-circuit (Wave 6a-4): if an existing projection
			// already matches this action's spec_hash AND the cogdoc file is on
			// disk, skip the whole apply — no DB write, no file write, no event.
			// Keeps scheduler-driven re-ticks (kernel-interior reconciler cadence)
			// from churning files, participant rows, and the bus on every tick
			// when nothing has changed. See project_cogos_reconciler_is_agent.md:
			// ApplyPlan must be provably idempotent for the kernel-interior
			// harness to call reconcilers on a schedule safely.
			if specHash := stringDetail(action.Details, "spec_hash"); specHash != "" {
				if existing, gerr := p.db.GetProjection(ctx, action.Name); gerr == nil && existing != nil && existing.SpecHash == specHash {
					absProjection := filepath.Join(root, existing.ContentPath)
					if _, serr := os.Stat(absProjection); serr == nil {
						res.Status = ApplySkipped
						res.CreatedID = existing.Sub
						results = append(results, res)
						continue
					}
				}
			}

			applied, err := p.applyUpsert(ctx, root, crd, action)
			if err != nil {
				res.Status = ApplyFailed
				res.Error = err.Error()
				results = append(results, res)
				continue
			}
			res.Status = ApplySucceeded
			res.CreatedID = applied.Sub
			results = append(results, res)

		case ActionDelete:
			if err := p.applyDelete(ctx, action); err != nil {
				res.Status = ApplyFailed
				res.Error = err.Error()
				results = append(results, res)
				continue
			}
			res.Status = ApplySucceeded
			results = append(results, res)

		default:
			res.Status = ApplySkipped
			results = append(results, res)
		}
	}

	return results, nil
}

// applyUpsert handles both create and update. Sequence:
//  1. If private_key set: resolve + verify integrity. Mismatch → mismatch event, error.
//  2. Write projection cogdoc at .cog/id/<sub>.cog.md.
//  3. Upsert DB row.
//  4. Upsert participants row.
//  5. Emit the appropriate identity.* event.
func (p *IdentityProvider) applyUpsert(ctx context.Context, root string, crd *IdentityCRD, action ReconcileAction) (*IdentityProjection, error) {
	sub := crd.Spec.Subject
	specHash := stringDetail(action.Details, "spec_hash")

	if crd.Spec.PrivateKey != nil {
		keyBytes, err := p.resolveKey(ctx, crd.Spec.PrivateKey.Ref)
		if err != nil {
			// Resolver failure is distinct from hash mismatch — surface as
			// a plain error, no mismatch event.
			return nil, fmt.Errorf("resolve key for %q: %w", sub, err)
		}
		if err := verifyKeyIntegrity(keyBytes, crd.Spec.PrivateKey.IntegrityHash); err != nil {
			p.mu.Lock()
			p.lastMismatchSubs[sub] = struct{}{}
			p.mu.Unlock()
			p.emitIdentityEvent(EventIdentityKeyMismatch, map[string]any{
				"iss":       crd.Spec.Issuer,
				"sub":       sub,
				"spec_hash": specHash,
				"error":     err.Error(),
			})
			return nil, err
		}
	}

	now := time.Now().UTC()
	projectionPath := filepath.Join(".cog", "id", sub+".cog.md")
	absProjection := filepath.Join(root, projectionPath)

	// Pull WorkspaceRoot and VoiceProfile from the primary expression so callers
	// don't need to iterate expressions to find the home URI or voice config.
	var workspaceRoot string
	var voiceProfile *VoiceProfile
	if exp := crd.Spec.ExpressionFor("*"); exp != nil {
		workspaceRoot = exp.WorkspaceRoot
		voiceProfile = exp.VoiceProfile
	} else if len(crd.Spec.Expressions) > 0 {
		workspaceRoot = crd.Spec.Expressions[0].WorkspaceRoot
		voiceProfile = crd.Spec.Expressions[0].VoiceProfile
	}

	proj := IdentityProjection{
		Sub:           sub,
		Iss:           crd.Spec.Issuer,
		Type:          crd.Spec.Type,
		ProjectedAt:   now,
		SpecHash:      specHash,
		Expressions:   append([]IdentityExpression(nil), crd.Spec.Expressions...),
		ContentPath:   projectionPath,
		WorkspaceRoot: workspaceRoot,
		VoiceProfile:  voiceProfile,
	}

	// Write projection cogdoc first — if it fails we haven't touched the DB.
	if err := writeIdentityProjectionCogDoc(absProjection, crd, specHash, now); err != nil {
		return nil, fmt.Errorf("write projection cogdoc %q: %w", absProjection, err)
	}

	if err := p.db.UpsertIdentityCogDoc(ctx, proj); err != nil {
		return nil, fmt.Errorf("upsert identity cogdoc: %w", err)
	}

	row := participantRowForCRD(crd, now, projectionPath)
	if err := p.db.UpsertParticipant(ctx, row); err != nil {
		return nil, fmt.Errorf("upsert participant: %w", err)
	}

	eventType := EventIdentityProjected
	if action.Action == ActionUpdate {
		eventType = EventIdentityExpressionUpdated
	}
	p.emitIdentityEvent(eventType, map[string]any{
		"iss":             crd.Spec.Issuer,
		"sub":             sub,
		"aud":             primaryAudience(&crd.Spec),
		"action":          string(action.Action),
		"spec_hash":       specHash,
		"projection_path": projectionPath,
		"applied_at":      now.Format(time.RFC3339),
	})

	return &proj, nil
}

// applyDelete removes both the participants row and the projection cogdoc
// from the DB, and emits identity.deregistered.
func (p *IdentityProvider) applyDelete(ctx context.Context, action ReconcileAction) error {
	sub := action.Name
	iss := stringDetail(action.Details, "iss")
	typ := stringDetail(action.Details, "type")

	if err := p.db.DeleteIdentityCogDoc(ctx, sub); err != nil {
		return fmt.Errorf("delete identity cogdoc: %w", err)
	}
	if err := p.db.DeleteParticipant(ctx, participantID(typ, sub)); err != nil {
		return fmt.Errorf("delete participant: %w", err)
	}

	// Remove the disk projection if we have a root. Missing file is fine.
	p.mu.Lock()
	root := p.root
	p.mu.Unlock()
	if root != "" {
		_ = os.Remove(filepath.Join(root, ".cog", "id", sub+".cog.md"))
	}

	p.emitIdentityEvent(EventIdentityDeregistered, map[string]any{
		"iss":        iss,
		"sub":        sub,
		"action":     string(ActionDelete),
		"spec_hash":  stringDetail(action.Details, "previous_spec_hash"),
		"applied_at": time.Now().UTC().Format(time.RFC3339),
	})
	return nil
}

// resolveKey dispatches to the registered resolver for the ref's scheme.
// Returns ErrSchemeNotImplemented for registered-but-unwired schemes.
func (p *IdentityProvider) resolveKey(ctx context.Context, ref string) ([]byte, error) {
	scheme, ok := parseKeyRefScheme(ref)
	if !ok {
		return nil, fmt.Errorf("identity: malformed key ref %q", ref)
	}
	resolver, ok := p.resolvers[scheme]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrSchemeNotImplemented, scheme)
	}
	return resolver.ResolveKey(ctx, ref)
}

// emitIdentityEvent is a thin wrapper that logs but does not fail on emit
// errors — the projection has already happened, losing the event is a
// reliability concern for the bus, not a correctness concern for reconcile.
func (p *IdentityProvider) emitIdentityEvent(eventType string, data map[string]any) {
	if p.emit == nil {
		return
	}
	if err := p.emit(eventType, data); err != nil {
		log.Printf("[identity] emit %s: %v", eventType, err)
	}
}

// ─── BuildState ─────────────────────────────────────────────────────────────────

// BuildState constructs a Terraform-style reconcile state from live
// projections. Lineage is preserved across calls (so operators can track a
// continuous history); serial is incremented each time.
func (p *IdentityProvider) BuildState(_ any, live any, existing *ReconcileState) (*ReconcileState, error) {
	liveState, ok := live.(*identityLive)
	if !ok {
		return nil, fmt.Errorf("identity provider: expected *identityLive, got %T", live)
	}

	state := &ReconcileState{
		Version:      1,
		ResourceType: "identity",
		GeneratedAt:  nowISO(),
		Resources:    []ReconcileResource{},
		Metadata:     map[string]any{},
	}

	if existing != nil && existing.Lineage != "" {
		state.Lineage = existing.Lineage
		state.Serial = existing.Serial + 1
	} else {
		state.Lineage = "identity-" + uuid.New().String()
		state.Serial = 1
	}

	subs := make([]string, 0, len(liveState.Projections))
	for sub := range liveState.Projections {
		subs = append(subs, sub)
	}
	sort.Strings(subs)

	now := nowISO()
	for _, sub := range subs {
		proj := liveState.Projections[sub]
		state.Resources = append(state.Resources, ReconcileResource{
			Address:       "identity." + sub,
			Type:          "identity",
			Mode:          ModeManaged,
			ExternalID:    participantID(proj.Type, sub),
			Name:          sub,
			LastRefreshed: now,
			Attributes: map[string]any{
				"iss":             proj.Iss,
				"type":            proj.Type,
				"spec_hash":       proj.SpecHash,
				"projection_path": proj.ContentPath,
				"projected_at":    proj.ProjectedAt.Format(time.RFC3339),
			},
		})
	}

	return state, nil
}

// ─── Health ─────────────────────────────────────────────────────────────────────

// Health returns the three-axis status:
//
//	Sync      — Synced when last plan had zero non-skip actions; OutOfSync otherwise.
//	Health    — Missing when any projection lacks a spec (delete pending);
//	            Degraded when any identity hit a key-hash mismatch;
//	            Healthy otherwise.
//	Operation — Syncing while ApplyPlan is running; Idle otherwise.
func (p *IdentityProvider) Health() ResourceStatus {
	p.mu.Lock()
	summary := p.lastPlanSummary
	mismatches := len(p.lastMismatchSubs)
	missing := len(p.missingSpecSubs)
	op := p.operation
	p.mu.Unlock()

	sync := SyncStatusSynced
	if summary.HasChanges() {
		sync = SyncStatusOutOfSync
	}

	health := HealthHealthy
	msg := ""
	switch {
	case mismatches > 0:
		health = HealthDegraded
		msg = fmt.Sprintf("%d identity key-hash mismatch(es)", mismatches)
	case missing > 0:
		health = HealthMissing
		msg = fmt.Sprintf("%d projection(s) without matching spec", missing)
	}

	return ResourceStatus{
		Sync:      sync,
		Health:    health,
		Operation: op,
		Message:   msg,
	}
}

// ─── Projection cogdoc writer ───────────────────────────────────────────────────

// writeIdentityProjectionCogDoc writes the Layer-2 principal card markdown
// file with provenance frontmatter. Directory is created if missing.
func writeIdentityProjectionCogDoc(absPath string, crd *IdentityCRD, specHash string, appliedAt time.Time) error {
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return err
	}

	exp := crd.Spec.ExpressionFor("*")
	if exp == nil && len(crd.Spec.Expressions) > 0 {
		exp = &crd.Spec.Expressions[0]
	}

	displayName := crd.Spec.Subject
	if exp != nil && exp.DisplayName != "" {
		displayName = exp.DisplayName
	}

	specPath := filepath.Join(".cog", "config", "identities", crd.Spec.Subject+".yaml")

	var buf strings.Builder
	buf.WriteString("---\n")
	buf.WriteString("type: identity\n")
	fmt.Fprintf(&buf, "id: %s\n", crd.Spec.Subject)
	fmt.Fprintf(&buf, "iss: %s\n", crd.Spec.Issuer)
	fmt.Fprintf(&buf, "title: %q\n", displayName)
	buf.WriteString("derived_from:\n")
	fmt.Fprintf(&buf, "  spec_path: %q\n", specPath)
	fmt.Fprintf(&buf, "  spec_hash: %q\n", specHash)
	buf.WriteString("  reconciler: IdentityProvider\n")
	fmt.Fprintf(&buf, "  applied_at: %q\n", appliedAt.UTC().Format(time.RFC3339))
	buf.WriteString("---\n\n")

	fmt.Fprintf(&buf, "# %s\n\n", displayName)
	if exp != nil {
		if exp.Role != "" {
			fmt.Fprintf(&buf, "**Role:** %s\n\n", exp.Role)
		}
		if vp := exp.VoiceProfile; vp != nil {
			if vp.Generative != nil {
				fmt.Fprintf(&buf, "**Voice (generative):** engine=%s ref=%s\n\n",
					vp.Generative.Engine, vp.Generative.ConditionalsRef)
			}
			if vp.Discriminative != nil {
				fmt.Fprintf(&buf, "**Voice (discriminative):** model=%s ref=%s\n\n",
					vp.Discriminative.Model, vp.Discriminative.EmbeddingRef)
			}
		}
		if len(exp.Skills) > 0 {
			buf.WriteString("**Skills:**\n")
			for _, s := range exp.Skills {
				fmt.Fprintf(&buf, "- %s\n", s)
			}
			buf.WriteString("\n")
		}
		if exp.MemoryNamespace != "" {
			fmt.Fprintf(&buf, "**Memory namespace:** `%s`\n\n", exp.MemoryNamespace)
		}
		if exp.WorkspaceRoot != "" {
			fmt.Fprintf(&buf, "**Workspace root:** `%s`\n\n", exp.WorkspaceRoot)
		}
	}
	fmt.Fprintf(&buf, "_Generated by IdentityProvider from `%s` (%s)._\n", specPath, specHash)

	return os.WriteFile(absPath, []byte(buf.String()), 0o644)
}

// ─── Small helpers ──────────────────────────────────────────────────────────────

// participantID returns the "<type>:<sub>" identifier used as participants.id.
// Unknown types fall back to "<type>:<sub>" as-is — validation upstream
// should have caught the bad type.
func participantID(typ, sub string) string {
	return typ + ":" + sub
}

// participantRowForCRD projects a CRD into a participants-table row.
func participantRowForCRD(crd *IdentityCRD, now time.Time, projectionPath string) ParticipantRow {
	name := crd.Metadata.Name
	// Prefer a display_name from the catch-all / first expression when present;
	// keep metadata.name as the ID fallback.
	if exp := crd.Spec.ExpressionFor("*"); exp != nil && exp.DisplayName != "" {
		name = exp.DisplayName
	} else if len(crd.Spec.Expressions) > 0 && crd.Spec.Expressions[0].DisplayName != "" {
		name = crd.Spec.Expressions[0].DisplayName
	}
	return ParticipantRow{
		ID:           participantID(crd.Spec.Type, crd.Spec.Subject),
		Type:         crd.Spec.Type,
		Name:         name,
		IdentityPath: projectionPath,
		SessionID:    "",
		Active:       true,
		LastSeen:     now,
		RegisteredAt: now,
		NodeHash:     "",
	}
}

// primaryAudience picks the audience used for an event's `aud` field —
// prefers the catch-all `*` expression, falls back to the first one. The
// audience is informational on the event; downstream consumers that care
// about per-audience projection pull the full expression list from the DB.
func primaryAudience(spec *IdentityCRDSpec) string {
	if spec == nil {
		return ""
	}
	if exp := spec.ExpressionFor("*"); exp != nil {
		return exp.Audience
	}
	if len(spec.Expressions) > 0 {
		return spec.Expressions[0].Audience
	}
	return ""
}

// stringDetail fetches a string value from an Action.Details map with a
// zero-value default. Keeps the apply code clean of repeated type asserts.
func stringDetail(d map[string]any, key string) string {
	if d == nil {
		return ""
	}
	if v, ok := d[key].(string); ok {
		return v
	}
	return ""
}
