// rbac_provider.go
// RBACProvider implements pkg/reconcile.Reconcilable for RBAC binding CRDs.
//
// This is the Wave 6b/6c binding layer on top of the existing flat-file Role
// loader (rbac.go). The existing RoleLoader / Role structs continue to work
// unchanged; RBACProvider adds the Reconcilable binding records without
// replacing anything.
//
// Persistence tier (OQ-6):
//   Structural bindings (RoleBinding, AccountBinding, NodeBinding,
//   WorkspaceBinding) are loaded from and written to YAML files under
//   .cog/config/rbac/bindings/<kind>/<name>.yaml. HarnessBindings are
//   in-memory only — they are populated by ApplyPlan when session-register
//   events arrive and cleared when the session ends. LoadConfig does not
//   scan for HarnessBindings.
//
// OQ-7:
//   WorkspaceBindingCRD is the authoritative binding record. The
//   IdentityExpression.WorkspaceRoot field (Primitive 1) is the spec hint.
//   A future WorkspaceReconciler will converge the binding toward the hint.
//   These two objects serve different layers; see rbac_bindings.go for the
//   full note.
//
// Long-term: the flat-file RoleLoader becomes part of this provider in a
// Wave 6d refactor. That consolidation is out of scope for this PR.

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// ─── Event constants ─────────────────────────────────────────────────────────

const (
	EventRBACBindingCreated  = "rbac.binding.created"
	EventRBACBindingUpdated  = "rbac.binding.updated"
	EventRBACBindingDeleted  = "rbac.binding.deleted"
	EventRBACHarnessAttached = "rbac.harness.attached"
	EventRBACHarnessDetached = "rbac.harness.detached"
)

const rbacEventSource = "reconciler/rbac-bindings"

// ─── Internal types ──────────────────────────────────────────────────────────

// rbacConfig is the provider's internal config bundle produced by LoadConfig.
type rbacConfig struct {
	Root     string
	Bindings *RBACBindingSet
}

// rbacLive is the provider's snapshot of current in-memory + disk state.
// Structural bindings are keyed by "<kind>/<name>" for diffing.
// HarnessBindings are keyed by "<session_id>/<type>".
type rbacLive struct {
	// Structural bindings: keyed "<kind>/<name>"
	RoleBindings      map[string]*RoleBindingCRD
	AccountBindings   map[string]*AccountBindingCRD
	NodeBindings      map[string]*NodeBindingCRD
	WorkspaceBindings map[string]*WorkspaceBindingCRD
	// HarnessBindings: keyed "<session_id>/<type>" — in-memory only
	HarnessBindings map[string]*HarnessBindingCRD
}

func newRBACLive() *rbacLive {
	return &rbacLive{
		RoleBindings:      make(map[string]*RoleBindingCRD),
		AccountBindings:   make(map[string]*AccountBindingCRD),
		NodeBindings:      make(map[string]*NodeBindingCRD),
		WorkspaceBindings: make(map[string]*WorkspaceBindingCRD),
		HarnessBindings:   make(map[string]*HarnessBindingCRD),
	}
}

// harnessKey produces the map key for a HarnessBinding.
func harnessKey(sessionID, bindingType string) string {
	return sessionID + "/" + bindingType
}

// ─── RBACProvider ───────────────────────────────────────────────────────────

// RBACProvider implements Reconcilable for RBAC binding CRDs.
// The provider manages five binding types; structural bindings are
// disk-persisted, harness bindings are in-memory only.
type RBACProvider struct {
	mu sync.Mutex

	emit BusEmit // reuse the same BusEmit type from identity_provider.go

	// in-memory live state: updated by ApplyPlan, queried by FetchLive.
	live *rbacLive

	// last reconcile summary for Health() reporting.
	root            string
	lastPlanSummary ReconcileSummary
	schemaErrors    []string
	operation       OperationPhase
}

// NewRBACProvider constructs a provider. A nil emit silently drops events.
func NewRBACProvider(emit BusEmit) *RBACProvider {
	if emit == nil {
		emit = func(string, map[string]any) error { return nil }
	}
	return &RBACProvider{
		emit:      emit,
		live:      newRBACLive(),
		operation: OperationIdle,
	}
}

// Type returns the provider identifier.
func (p *RBACProvider) Type() string { return "rbac-bindings" }

// ─── LoadConfig ──────────────────────────────────────────────────────────────

// LoadConfig reads all structural binding YAML files from
// .cog/config/rbac/bindings/. HarnessBindings are NOT loaded here —
// they are populated at runtime via ApplyPlan when session events arrive.
//
// Schema errors (YAML parse failures, missing required fields) are stored in
// p.schemaErrors so that Health() can report them. Valid files are still loaded;
// schema errors do not abort the load. Fatal I/O errors cause LoadConfig to
// return a non-nil error.
func (p *RBACProvider) LoadConfig(root string) (any, error) {
	p.mu.Lock()
	p.root = root
	p.mu.Unlock()

	bindings, schemaErrs, err := LoadRBACBindings(root)
	if err != nil {
		return nil, fmt.Errorf("rbac provider: load config: %w", err)
	}

	p.mu.Lock()
	p.schemaErrors = schemaErrs
	p.mu.Unlock()

	return &rbacConfig{Root: root, Bindings: bindings}, nil
}

// ─── FetchLive ───────────────────────────────────────────────────────────────

// FetchLive returns the current in-memory live state. Because structural
// bindings are disk-backed and harness bindings are in-memory, FetchLive
// returns a snapshot of the provider's running memory. This is consistent
// with the stub-constellation pattern: the provider is the source of truth
// until a real persistence layer is wired in Wave 6c.
func (p *RBACProvider) FetchLive(_ context.Context, _ any) (any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// Return a shallow copy to avoid races between FetchLive and ApplyPlan.
	snapshot := newRBACLive()
	for k, v := range p.live.RoleBindings {
		snapshot.RoleBindings[k] = v
	}
	for k, v := range p.live.AccountBindings {
		snapshot.AccountBindings[k] = v
	}
	for k, v := range p.live.NodeBindings {
		snapshot.NodeBindings[k] = v
	}
	for k, v := range p.live.WorkspaceBindings {
		snapshot.WorkspaceBindings[k] = v
	}
	for k, v := range p.live.HarnessBindings {
		snapshot.HarnessBindings[k] = v
	}
	return snapshot, nil
}

// ─── ComputePlan ─────────────────────────────────────────────────────────────

// ComputePlan diffs declared config (from LoadConfig) against live state
// (from FetchLive). Produces create/update/delete/skip actions per binding.
// HarnessBindings are never in the config set; they only appear in live state
// (populated at runtime). ComputePlan will never emit a create for them.
func (p *RBACProvider) ComputePlan(config any, live any, _ *ReconcileState) (*ReconcilePlan, error) {
	cfg, ok := config.(*rbacConfig)
	if !ok {
		return nil, fmt.Errorf("rbac provider: expected *rbacConfig, got %T", config)
	}
	liveState, ok := live.(*rbacLive)
	if !ok {
		return nil, fmt.Errorf("rbac provider: expected *rbacLive, got %T", live)
	}

	plan := &ReconcilePlan{
		ResourceType: "rbac-bindings",
		GeneratedAt:  nowISO(),
		ConfigPath:   rbacBindingsDir(cfg.Root),
		Metadata:     map[string]any{},
	}

	// Diff each binding kind.
	diffRoleBindings(plan, cfg.Bindings.RoleBindings, liveState.RoleBindings)
	diffAccountBindings(plan, cfg.Bindings.AccountBindings, liveState.AccountBindings)
	diffNodeBindings(plan, cfg.Bindings.NodeBindings, liveState.NodeBindings)
	diffWorkspaceBindings(plan, cfg.Bindings.WorkspaceBindings, liveState.WorkspaceBindings)
	// HarnessBindings: no config set; never generate creates from this path.

	// Deterministic order.
	sort.Slice(plan.Actions, func(i, j int) bool {
		if plan.Actions[i].Action != plan.Actions[j].Action {
			return plan.Actions[i].Action < plan.Actions[j].Action
		}
		if plan.Actions[i].ResourceType != plan.Actions[j].ResourceType {
			return plan.Actions[i].ResourceType < plan.Actions[j].ResourceType
		}
		return plan.Actions[i].Name < plan.Actions[j].Name
	})

	p.mu.Lock()
	p.lastPlanSummary = plan.Summary
	p.mu.Unlock()

	return plan, nil
}

// ─── diff helpers ─────────────────────────────────────────────────────────────

func diffRoleBindings(plan *ReconcilePlan, spec []*RoleBindingCRD, live map[string]*RoleBindingCRD) {
	seen := make(map[string]struct{}, len(spec))
	for _, crd := range spec {
		key := "rolebinding/" + crd.Metadata.Name
		seen[key] = struct{}{}
		if _, exists := live[key]; !exists {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action: ActionCreate, ResourceType: "rolebinding", Name: crd.Metadata.Name,
				Details: map[string]any{"subject": crd.Spec.Subject, "role_ref": crd.Spec.RoleRef},
			})
			plan.Summary.Creates++
		} else {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action: ActionSkip, ResourceType: "rolebinding", Name: crd.Metadata.Name,
				Details: map[string]any{"reason": "in sync"},
			})
			plan.Summary.Skipped++
		}
	}
	for key, crd := range live {
		if _, ok := seen[key]; !ok {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action: ActionDelete, ResourceType: "rolebinding", Name: crd.Metadata.Name,
				Details: map[string]any{"subject": crd.Spec.Subject},
			})
			plan.Summary.Deletes++
		}
	}
}

func diffAccountBindings(plan *ReconcilePlan, spec []*AccountBindingCRD, live map[string]*AccountBindingCRD) {
	seen := make(map[string]struct{}, len(spec))
	for _, crd := range spec {
		key := "accountbinding/" + crd.Metadata.Name
		seen[key] = struct{}{}
		if _, exists := live[key]; !exists {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action: ActionCreate, ResourceType: "accountbinding", Name: crd.Metadata.Name,
				Details: map[string]any{"subject": crd.Spec.Subject, "node": crd.Spec.Node, "account": crd.Spec.Account},
			})
			plan.Summary.Creates++
		} else {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action: ActionSkip, ResourceType: "accountbinding", Name: crd.Metadata.Name,
				Details: map[string]any{"reason": "in sync"},
			})
			plan.Summary.Skipped++
		}
	}
	for key, crd := range live {
		if _, ok := seen[key]; !ok {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action: ActionDelete, ResourceType: "accountbinding", Name: crd.Metadata.Name,
				Details: map[string]any{"subject": crd.Spec.Subject},
			})
			plan.Summary.Deletes++
		}
	}
}

func diffNodeBindings(plan *ReconcilePlan, spec []*NodeBindingCRD, live map[string]*NodeBindingCRD) {
	seen := make(map[string]struct{}, len(spec))
	for _, crd := range spec {
		key := "nodebinding/" + crd.Metadata.Name
		seen[key] = struct{}{}
		if _, exists := live[key]; !exists {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action: ActionCreate, ResourceType: "nodebinding", Name: crd.Metadata.Name,
				Details: map[string]any{"subject": crd.Spec.Subject, "node": crd.Spec.Node, "relation": crd.Spec.Relation},
			})
			plan.Summary.Creates++
		} else {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action: ActionSkip, ResourceType: "nodebinding", Name: crd.Metadata.Name,
				Details: map[string]any{"reason": "in sync"},
			})
			plan.Summary.Skipped++
		}
	}
	for key, crd := range live {
		if _, ok := seen[key]; !ok {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action: ActionDelete, ResourceType: "nodebinding", Name: crd.Metadata.Name,
				Details: map[string]any{"subject": crd.Spec.Subject},
			})
			plan.Summary.Deletes++
		}
	}
}

func diffWorkspaceBindings(plan *ReconcilePlan, spec []*WorkspaceBindingCRD, live map[string]*WorkspaceBindingCRD) {
	seen := make(map[string]struct{}, len(spec))
	for _, crd := range spec {
		key := "workspacebinding/" + crd.Metadata.Name
		seen[key] = struct{}{}
		if _, exists := live[key]; !exists {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action: ActionCreate, ResourceType: "workspacebinding", Name: crd.Metadata.Name,
				Details: map[string]any{"subject": crd.Spec.Subject, "workspace_uri": crd.Spec.WorkspaceURI, "access": crd.Spec.Access},
			})
			plan.Summary.Creates++
		} else {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action: ActionSkip, ResourceType: "workspacebinding", Name: crd.Metadata.Name,
				Details: map[string]any{"reason": "in sync"},
			})
			plan.Summary.Skipped++
		}
	}
	for key, crd := range live {
		if _, ok := seen[key]; !ok {
			plan.Actions = append(plan.Actions, ReconcileAction{
				Action: ActionDelete, ResourceType: "workspacebinding", Name: crd.Metadata.Name,
				Details: map[string]any{"subject": crd.Spec.Subject},
			})
			plan.Summary.Deletes++
		}
	}
}

// ─── ApplyPlan ───────────────────────────────────────────────────────────────

// ApplyPlan executes the plan actions. Structural bindings are written to disk
// and mirrored into the in-memory live map. Delete actions remove from disk
// and from memory. HarnessBinding creates arrive via separate AttachHarness
// calls (not through the standard plan path) because they are runtime events,
// not config-driven.
func (p *RBACProvider) ApplyPlan(ctx context.Context, plan *ReconcilePlan) ([]ReconcileResult, error) {
	if plan == nil {
		return nil, fmt.Errorf("rbac provider: nil plan")
	}

	p.mu.Lock()
	p.operation = OperationSyncing
	root := p.root
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.operation = OperationIdle
		p.mu.Unlock()
	}()

	// Reload specs so we have the full YAML for creates.
	// Schema errors from this reload are intentionally not re-stored in
	// p.schemaErrors here — LoadConfig is the authoritative load path for
	// health tracking. ApplyPlan uses the reload only to fetch CRD structs
	// for creates; any new schema errors will surface on the next LoadConfig.
	bindings, _, err := LoadRBACBindings(root)
	if err != nil {
		return nil, fmt.Errorf("rbac provider: reload specs for apply: %w", err)
	}

	var results []ReconcileResult

	for _, action := range plan.Actions {
		if action.Action == ActionSkip {
			continue
		}

		res := ReconcileResult{
			Phase:  "rbac-bindings",
			Action: string(action.Action),
			Name:   action.Name,
		}

		var applyErr error
		switch action.ResourceType {
		case "rolebinding":
			applyErr = p.applyRoleBinding(root, action, bindings)
		case "accountbinding":
			applyErr = p.applyAccountBinding(root, action, bindings)
		case "nodebinding":
			applyErr = p.applyNodeBinding(root, action, bindings)
		case "workspacebinding":
			applyErr = p.applyWorkspaceBinding(root, action, bindings)
		default:
			res.Status = ApplySkipped
			results = append(results, res)
			continue
		}

		if applyErr != nil {
			res.Status = ApplyFailed
			res.Error = applyErr.Error()
		} else {
			res.Status = ApplySucceeded
		}
		results = append(results, res)
	}

	return results, nil
}

// AttachHarness registers a HarnessBindingCRD into the in-memory live state
// and emits rbac.harness.attached. Called by session-register hooks at runtime.
// This path is separate from the config-driven plan/apply cycle because
// HarnessBindings are ephemeral runtime state, not declared config.
func (p *RBACProvider) AttachHarness(binding *HarnessBindingCRD) {
	key := harnessKey(binding.Spec.SessionID, binding.Spec.Type)
	p.mu.Lock()
	p.live.HarnessBindings[key] = binding
	p.mu.Unlock()
	p.emitRBACEvent(EventRBACHarnessAttached, map[string]any{
		"session_id":   binding.Spec.SessionID,
		"subject":      binding.Spec.Subject,
		"type":         binding.Spec.Type,
		"harness_type": binding.Spec.HarnessType,
		"attached_at":  time.Now().UTC().Format(time.RFC3339),
	})
}

// ResolveHarnessBinding returns the in-memory HarnessBindingCRD for
// (sessionID, bindingType), or (nil, false) when no binding exists.
// Read-only; no mutation. Satisfies the engine.HarnessAttacher interface
// so the MCP server can look up the binding after registration.
func (p *RBACProvider) ResolveHarnessBinding(sessionID, bindingType string) (*HarnessBindingCRD, bool) {
	key := harnessKey(sessionID, bindingType)
	p.mu.Lock()
	binding, ok := p.live.HarnessBindings[key]
	p.mu.Unlock()
	if !ok {
		return nil, false
	}
	cp := *binding
	return &cp, true
}

// DetachHarness removes a HarnessBindingCRD from in-memory state and emits
// rbac.harness.detached. Called when a session ends.
func (p *RBACProvider) DetachHarness(sessionID, bindingType string) {
	key := harnessKey(sessionID, bindingType)
	p.mu.Lock()
	binding, ok := p.live.HarnessBindings[key]
	if ok {
		delete(p.live.HarnessBindings, key)
	}
	p.mu.Unlock()
	if ok {
		p.emitRBACEvent(EventRBACHarnessDetached, map[string]any{
			"session_id":  sessionID,
			"subject":     binding.Spec.Subject,
			"type":        bindingType,
			"detached_at": time.Now().UTC().Format(time.RFC3339),
		})
	}
}

// ─── apply helpers ────────────────────────────────────────────────────────────

func (p *RBACProvider) applyRoleBinding(root string, action ReconcileAction, bindings *RBACBindingSet) error {
	key := "rolebinding/" + action.Name
	if action.Action == ActionDelete {
		if err := removeBindingFile(root, "rolebinding", action.Name); err != nil {
			return err
		}
		p.mu.Lock()
		delete(p.live.RoleBindings, key)
		p.mu.Unlock()
		p.emitRBACEvent(EventRBACBindingDeleted, map[string]any{"kind": "rolebinding", "name": action.Name})
		return nil
	}
	crd := findRoleBinding(bindings.RoleBindings, action.Name)
	if crd == nil {
		return fmt.Errorf("rolebinding %q not found in spec", action.Name)
	}
	if err := writeBindingFile(root, "rolebinding", action.Name, crd); err != nil {
		return err
	}
	p.mu.Lock()
	p.live.RoleBindings[key] = crd
	p.mu.Unlock()
	p.emitRBACEvent(EventRBACBindingCreated, map[string]any{"kind": "rolebinding", "name": action.Name, "subject": crd.Spec.Subject})
	return nil
}

func (p *RBACProvider) applyAccountBinding(root string, action ReconcileAction, bindings *RBACBindingSet) error {
	key := "accountbinding/" + action.Name
	if action.Action == ActionDelete {
		if err := removeBindingFile(root, "accountbinding", action.Name); err != nil {
			return err
		}
		p.mu.Lock()
		delete(p.live.AccountBindings, key)
		p.mu.Unlock()
		p.emitRBACEvent(EventRBACBindingDeleted, map[string]any{"kind": "accountbinding", "name": action.Name})
		return nil
	}
	crd := findAccountBinding(bindings.AccountBindings, action.Name)
	if crd == nil {
		return fmt.Errorf("accountbinding %q not found in spec", action.Name)
	}
	if err := writeBindingFile(root, "accountbinding", action.Name, crd); err != nil {
		return err
	}
	p.mu.Lock()
	p.live.AccountBindings[key] = crd
	p.mu.Unlock()
	p.emitRBACEvent(EventRBACBindingCreated, map[string]any{"kind": "accountbinding", "name": action.Name, "subject": crd.Spec.Subject})
	return nil
}

func (p *RBACProvider) applyNodeBinding(root string, action ReconcileAction, bindings *RBACBindingSet) error {
	key := "nodebinding/" + action.Name
	if action.Action == ActionDelete {
		if err := removeBindingFile(root, "nodebinding", action.Name); err != nil {
			return err
		}
		p.mu.Lock()
		delete(p.live.NodeBindings, key)
		p.mu.Unlock()
		p.emitRBACEvent(EventRBACBindingDeleted, map[string]any{"kind": "nodebinding", "name": action.Name})
		return nil
	}
	crd := findNodeBinding(bindings.NodeBindings, action.Name)
	if crd == nil {
		return fmt.Errorf("nodebinding %q not found in spec", action.Name)
	}
	if err := writeBindingFile(root, "nodebinding", action.Name, crd); err != nil {
		return err
	}
	p.mu.Lock()
	p.live.NodeBindings[key] = crd
	p.mu.Unlock()
	p.emitRBACEvent(EventRBACBindingCreated, map[string]any{"kind": "nodebinding", "name": action.Name, "subject": crd.Spec.Subject})
	return nil
}

func (p *RBACProvider) applyWorkspaceBinding(root string, action ReconcileAction, bindings *RBACBindingSet) error {
	key := "workspacebinding/" + action.Name
	if action.Action == ActionDelete {
		if err := removeBindingFile(root, "workspacebinding", action.Name); err != nil {
			return err
		}
		p.mu.Lock()
		delete(p.live.WorkspaceBindings, key)
		p.mu.Unlock()
		p.emitRBACEvent(EventRBACBindingDeleted, map[string]any{"kind": "workspacebinding", "name": action.Name})
		return nil
	}
	crd := findWorkspaceBinding(bindings.WorkspaceBindings, action.Name)
	if crd == nil {
		return fmt.Errorf("workspacebinding %q not found in spec", action.Name)
	}
	if err := writeBindingFile(root, "workspacebinding", action.Name, crd); err != nil {
		return err
	}
	p.mu.Lock()
	p.live.WorkspaceBindings[key] = crd
	p.mu.Unlock()
	p.emitRBACEvent(EventRBACBindingCreated, map[string]any{"kind": "workspacebinding", "name": action.Name, "subject": crd.Spec.Subject})
	return nil
}

// ─── BuildState ───────────────────────────────────────────────────────────────

// BuildState constructs a Terraform-style state snapshot from live bindings.
func (p *RBACProvider) BuildState(_ any, live any, existing *ReconcileState) (*ReconcileState, error) {
	liveState, ok := live.(*rbacLive)
	if !ok {
		return nil, fmt.Errorf("rbac provider: expected *rbacLive, got %T", live)
	}

	state := &ReconcileState{
		Version:      1,
		ResourceType: "rbac-bindings",
		GeneratedAt:  nowISO(),
		Resources:    []ReconcileResource{},
		Metadata:     map[string]any{},
	}

	if existing != nil && existing.Lineage != "" {
		state.Lineage = existing.Lineage
		state.Serial = existing.Serial + 1
	} else {
		state.Lineage = "rbac-bindings-" + nowISO()
		state.Serial = 1
	}

	now := nowISO()

	// Collect and sort keys for deterministic output.
	var roleKeys []string
	for k := range liveState.RoleBindings {
		roleKeys = append(roleKeys, k)
	}
	sort.Strings(roleKeys)
	for _, k := range roleKeys {
		crd := liveState.RoleBindings[k]
		state.Resources = append(state.Resources, ReconcileResource{
			Address: k, Type: "rolebinding", Mode: ModeManaged,
			ExternalID: k, Name: crd.Metadata.Name, LastRefreshed: now,
			Attributes: map[string]any{"subject": crd.Spec.Subject, "role_ref": crd.Spec.RoleRef, "scope": crd.Spec.Scope},
		})
	}

	var acctKeys []string
	for k := range liveState.AccountBindings {
		acctKeys = append(acctKeys, k)
	}
	sort.Strings(acctKeys)
	for _, k := range acctKeys {
		crd := liveState.AccountBindings[k]
		state.Resources = append(state.Resources, ReconcileResource{
			Address: k, Type: "accountbinding", Mode: ModeManaged,
			ExternalID: k, Name: crd.Metadata.Name, LastRefreshed: now,
			Attributes: map[string]any{"subject": crd.Spec.Subject, "node": crd.Spec.Node, "account": crd.Spec.Account},
		})
	}

	var nodeKeys []string
	for k := range liveState.NodeBindings {
		nodeKeys = append(nodeKeys, k)
	}
	sort.Strings(nodeKeys)
	for _, k := range nodeKeys {
		crd := liveState.NodeBindings[k]
		state.Resources = append(state.Resources, ReconcileResource{
			Address: k, Type: "nodebinding", Mode: ModeManaged,
			ExternalID: k, Name: crd.Metadata.Name, LastRefreshed: now,
			Attributes: map[string]any{"subject": crd.Spec.Subject, "node": crd.Spec.Node, "relation": crd.Spec.Relation},
		})
	}

	var wsKeys []string
	for k := range liveState.WorkspaceBindings {
		wsKeys = append(wsKeys, k)
	}
	sort.Strings(wsKeys)
	for _, k := range wsKeys {
		crd := liveState.WorkspaceBindings[k]
		state.Resources = append(state.Resources, ReconcileResource{
			Address: k, Type: "workspacebinding", Mode: ModeManaged,
			ExternalID: k, Name: crd.Metadata.Name, LastRefreshed: now,
			Attributes: map[string]any{"subject": crd.Spec.Subject, "workspace_uri": crd.Spec.WorkspaceURI, "access": crd.Spec.Access},
		})
	}

	var harnessKeys []string
	for k := range liveState.HarnessBindings {
		harnessKeys = append(harnessKeys, k)
	}
	sort.Strings(harnessKeys)
	for _, k := range harnessKeys {
		crd := liveState.HarnessBindings[k]
		state.Resources = append(state.Resources, ReconcileResource{
			Address: k, Type: "harnessbinding", Mode: ModeManaged,
			ExternalID: k, Name: crd.Metadata.Name, LastRefreshed: now,
			Attributes: map[string]any{
				"session_id":   crd.Spec.SessionID,
				"subject":      crd.Spec.Subject,
				"type":         crd.Spec.Type,
				"harness_type": crd.Spec.HarnessType,
				"ephemeral":    true,
			},
		})
	}

	// Summary counts.
	state.Metadata["role_binding_count"] = len(liveState.RoleBindings)
	state.Metadata["account_binding_count"] = len(liveState.AccountBindings)
	state.Metadata["node_binding_count"] = len(liveState.NodeBindings)
	state.Metadata["workspace_binding_count"] = len(liveState.WorkspaceBindings)
	state.Metadata["harness_binding_count"] = len(liveState.HarnessBindings)

	return state, nil
}

// ─── Health ───────────────────────────────────────────────────────────────────

// Health returns the three-axis status.
//
//	Sync      — Synced when last plan had no non-skip actions; OutOfSync otherwise.
//	Health    — Degraded when LoadConfig or ApplyPlan recorded schema errors;
//	            Healthy otherwise.
//	Operation — Syncing while ApplyPlan is running; Idle otherwise.
func (p *RBACProvider) Health() ResourceStatus {
	p.mu.Lock()
	summary := p.lastPlanSummary
	schemaErrs := len(p.schemaErrors)
	op := p.operation
	p.mu.Unlock()

	sync := SyncStatusSynced
	if summary.HasChanges() {
		sync = SyncStatusOutOfSync
	}

	health := HealthHealthy
	msg := ""
	if schemaErrs > 0 {
		health = HealthDegraded
		msg = fmt.Sprintf("%d schema error(s) in RBAC binding files", schemaErrs)
	}

	return ResourceStatus{
		Sync:      sync,
		Health:    health,
		Operation: op,
		Message:   msg,
	}
}

// ─── Disk I/O ─────────────────────────────────────────────────────────────────

// writeBindingFile marshals a binding CRD to YAML and writes it to the
// per-kind subdirectory, creating parent directories as needed.
func writeBindingFile(root, kind, name string, v interface{}) error {
	dir := rbacKindDir(root, kind)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("rbac: mkdir %s: %w", dir, err)
	}
	data, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Errorf("rbac: marshal %s/%s: %w", kind, name, err)
	}
	path := filepath.Join(dir, name+".yaml")
	if err := os.WriteFile(path, data, 0o640); err != nil {
		return fmt.Errorf("rbac: write %s: %w", path, err)
	}
	return nil
}

// removeBindingFile removes a binding YAML file. Missing file is not an error.
func removeBindingFile(root, kind, name string) error {
	path := filepath.Join(rbacKindDir(root, kind), name+".yaml")
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rbac: remove %s: %w", path, err)
	}
	return nil
}

// ─── Lookup helpers ───────────────────────────────────────────────────────────

func findRoleBinding(list []*RoleBindingCRD, name string) *RoleBindingCRD {
	for _, crd := range list {
		if crd.Metadata.Name == name {
			return crd
		}
	}
	return nil
}

func findAccountBinding(list []*AccountBindingCRD, name string) *AccountBindingCRD {
	for _, crd := range list {
		if crd.Metadata.Name == name {
			return crd
		}
	}
	return nil
}

func findNodeBinding(list []*NodeBindingCRD, name string) *NodeBindingCRD {
	for _, crd := range list {
		if crd.Metadata.Name == name {
			return crd
		}
	}
	return nil
}

func findWorkspaceBinding(list []*WorkspaceBindingCRD, name string) *WorkspaceBindingCRD {
	for _, crd := range list {
		if crd.Metadata.Name == name {
			return crd
		}
	}
	return nil
}

// ─── Event emission ───────────────────────────────────────────────────────────

func (p *RBACProvider) emitRBACEvent(eventType string, data map[string]any) {
	if p.emit == nil {
		return
	}
	data["source"] = rbacEventSource
	if err := p.emit(eventType, data); err != nil {
		log.Printf("[rbac] emit %s: %v", eventType, err)
	}
}
