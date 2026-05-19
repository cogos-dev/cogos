// rbac_bindings.go
// K8s-style RBAC binding CRD types for CogOS (Brainstorm Primitive 5).
//
// Each type mirrors a Kubernetes binding resource:
//   - RoleBindingCRD      — identity → role           (like K8s RoleBinding)
//   - AccountBindingCRD   — identity → OS account     (like K8s ServiceAccount binding)
//   - NodeBindingCRD      — identity → node           (like K8s Node affinity)
//   - WorkspaceBindingCRD — identity → workspace URI  (like K8s Namespace binding)
//   - HarnessBindingCRD   — session  → identity       (like K8s Pod binding; ephemeral)
//
// Persistence tier (OQ-6 decision):
//   Structural bindings (RoleBinding, AccountBinding, NodeBinding, WorkspaceBinding)
//   are persisted as YAML under .cog/config/rbac/bindings/<kind>/<name>.yaml.
//   HarnessBinding is in-memory only because it is per-session ephemeral — it
//   is created at harness-registration time and evaporates with the session.
//   The K8s analog: structural bindings are like RoleBindings (persisted);
//   HarnessBindings are like Pod bindings (created at scheduling, not stored).
//
// OQ-7 disambiguation:
//   WorkspaceBindingCRD is the AUTHORITATIVE BINDING RECORD — it declares
//   which identity owns which workspace URI with what access level.
//   IdentityExpression.WorkspaceRoot (Primitive 1) is the SPEC HINT — a
//   declaration of intent on the identity spec. A future WorkspaceReconciler
//   will converge (Wave 6c) the binding record toward the spec hint. They are
//   NOT redundant; they live at different layers (spec vs authoritative binding
//   state).
//
// Storage paths:
//   .cog/config/rbac/bindings/rolebinding/<name>.yaml
//   .cog/config/rbac/bindings/accountbinding/<name>.yaml
//   .cog/config/rbac/bindings/nodebinding/<name>.yaml
//   .cog/config/rbac/bindings/workspacebinding/<name>.yaml
//   (HarnessBinding: in-memory only, no disk path)

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// ─── Envelope ───────────────────────────────────────────────────────────────

// RBACMeta is the standard metadata envelope for all RBAC binding CRDs.
// Mirrors K8s ObjectMeta; Labels and Annotations are optional.
type RBACMeta struct {
	Name        string            `yaml:"name"`
	Labels      map[string]string `yaml:"labels,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
}

// ─── RoleBinding ────────────────────────────────────────────────────────────

// RoleBindingCRD binds a subject identity to a named role, optionally scoped
// to a workspace, channel, or resource URI. K8s RoleBinding analog.
// Stored at: .cog/config/rbac/bindings/rolebinding/<name>.yaml
type RoleBindingCRD struct {
	APIVersion string          `yaml:"apiVersion"` // "cog.os/v1alpha1"
	Kind       string          `yaml:"kind"`       // "RoleBinding"
	Metadata   RBACMeta        `yaml:"metadata"`
	Spec       RoleBindingSpec `yaml:"spec"`
}

// RoleBindingSpec declares the subject/role pairing.
type RoleBindingSpec struct {
	// Subject is the identity sub-slug being granted the role (e.g. "cog").
	Subject string `yaml:"subject"`
	// RoleRef names the role (e.g. "orchestrator", "operator", "observer").
	RoleRef string `yaml:"role_ref"`
	// Scope optionally narrows the grant to a workspace, channel, or
	// resource URI. Empty means cluster-wide (all workspaces this node manages).
	Scope string `yaml:"scope,omitempty"`
}

// ─── AccountBinding ─────────────────────────────────────────────────────────

// AccountBindingCRD binds a substrate identity to a local OS account on a
// named node. Enables the reconciler to assert "on node darkstar, identity
// cog runs as OS user slowbro."
// Stored at: .cog/config/rbac/bindings/accountbinding/<name>.yaml
type AccountBindingCRD struct {
	APIVersion string             `yaml:"apiVersion"` // "cog.os/v1alpha1"
	Kind       string             `yaml:"kind"`       // "AccountBinding"
	Metadata   RBACMeta           `yaml:"metadata"`
	Spec       AccountBindingSpec `yaml:"spec"`
}

// AccountBindingSpec declares the identity → node → OS-account triple.
type AccountBindingSpec struct {
	// Subject is the identity sub-slug (e.g. "cog").
	Subject string `yaml:"subject"`
	// Node is the node identity slug the account lives on (e.g. "darkstar").
	Node string `yaml:"node"`
	// Account is the OS account name (e.g. "slowbro").
	Account string `yaml:"account"`
}

// ─── NodeBinding ────────────────────────────────────────────────────────────

// NodeBindingCRD asserts that a subject identity has a declared relationship
// to a hardware/cognitive node — either it can embody that node (can-embody)
// or is pinned to it (pinned-to). K8s Node affinity analog.
// Stored at: .cog/config/rbac/bindings/nodebinding/<name>.yaml
type NodeBindingCRD struct {
	APIVersion string         `yaml:"apiVersion"` // "cog.os/v1alpha1"
	Kind       string         `yaml:"kind"`       // "NodeBinding"
	Metadata   RBACMeta       `yaml:"metadata"`
	Spec       NodeBindingSpec `yaml:"spec"`
}

// NodeBindingSpec declares the subject → node relationship.
type NodeBindingSpec struct {
	// Subject is the identity sub-slug (e.g. "cog").
	Subject string `yaml:"subject"`
	// Node is the node identity slug (e.g. "darkstar", "eclipse").
	Node string `yaml:"node"`
	// Relation declares the type of node relationship.
	// "can-embody" — the identity may run as a process on this node.
	// "pinned-to"  — the identity is fixed to this node; will not migrate.
	Relation string `yaml:"relation"` // "can-embody" | "pinned-to"
}

// ─── WorkspaceBinding ───────────────────────────────────────────────────────

// WorkspaceBindingCRD is the AUTHORITATIVE BINDING RECORD asserting that a
// subject identity owns or has access to a workspace URI. This is NOT the
// same as IdentityExpression.WorkspaceRoot (Primitive 1), which is the spec
// hint. The reconciler converges this binding toward the spec hint. See the
// OQ-7 note at the top of this file.
// Stored at: .cog/config/rbac/bindings/workspacebinding/<name>.yaml
type WorkspaceBindingCRD struct {
	APIVersion string               `yaml:"apiVersion"` // "cog.os/v1alpha1"
	Kind       string               `yaml:"kind"`       // "WorkspaceBinding"
	Metadata   RBACMeta             `yaml:"metadata"`
	Spec       WorkspaceBindingSpec `yaml:"spec"`
}

// WorkspaceBindingSpec declares the subject → workspace-URI → access triple.
type WorkspaceBindingSpec struct {
	// Subject is the identity sub-slug (e.g. "cog", "chaz").
	Subject string `yaml:"subject"`
	// WorkspaceURI is the cog:// address of the workspace (e.g. "cog://workspaces/cog").
	WorkspaceURI string `yaml:"workspace_uri"`
	// Access declares the access level.
	// "owner"      — full control, can modify workspace config.
	// "read-write" — read and write workspace content; cannot modify config.
	// "read"       — read-only access to workspace content.
	Access string `yaml:"access"` // "owner" | "read-write" | "read"
}

// ─── HarnessBinding ─────────────────────────────────────────────────────────

// HarnessBindingCRD links a live harness session to the identity it embodies.
// Agentic harnesses (Claude Code, Cursor, etc.) are structurally dual-identity:
// they bind both a user identity and an agent identity simultaneously, so a
// single session may have two HarnessBindingCRDs (type="user" + type="agent").
//
// Persistence: IN-MEMORY ONLY. HarnessBindings are per-session ephemeral —
// they are created by RBACProvider.ApplyPlan when a session-register event
// arrives and are removed when the session ends. They are never written to
// disk. The K8s analog is a Pod binding: created at scheduling time, evaporates
// with the workload. RBACProvider.LoadConfig does not scan for HarnessBindings.
type HarnessBindingCRD struct {
	APIVersion string             `yaml:"apiVersion"` // "cog.os/v1alpha1"
	Kind       string             `yaml:"kind"`       // "HarnessBinding"
	Metadata   RBACMeta           `yaml:"metadata"`
	Spec       HarnessBindingSpec `yaml:"spec"`
}

// HarnessBindingSpec declares the session → identity pairing.
type HarnessBindingSpec struct {
	// SessionID is the session identifier from the session-register event.
	SessionID string `yaml:"session_id"`
	// Subject is the identity sub-slug being bound (e.g. "chaz", "cog").
	Subject string `yaml:"subject"`
	// Type distinguishes the user identity ("user") from the agent identity
	// ("agent") in a dual-identity agentic harness session.
	Type string `yaml:"type"` // "user" | "agent"
	// HarnessType identifies the harness software (e.g. "claude-code", "cursor").
	HarnessType string `yaml:"harness_type"`
	// NodeID optionally names the node where the harness is running.
	NodeID string `yaml:"node_id,omitempty"`
}

// ─── Storage paths ──────────────────────────────────────────────────────────

// rbacBindingsDir returns the root directory for structural RBAC binding YAML files.
// Created by RBACProvider.ApplyPlan on first write.
func rbacBindingsDir(root string) string {
	return filepath.Join(root, ".cog", "config", "rbac", "bindings")
}

// rbacKindDir returns the per-kind subdirectory path (lowercased kind name).
func rbacKindDir(root, kind string) string {
	return filepath.Join(rbacBindingsDir(root), kind)
}

// ─── Loaders ────────────────────────────────────────────────────────────────

// RBACBindingSet holds all loaded structural binding records.
// HarnessBindings are not included — they are populated at runtime.
type RBACBindingSet struct {
	RoleBindings      []*RoleBindingCRD
	AccountBindings   []*AccountBindingCRD
	NodeBindings      []*NodeBindingCRD
	WorkspaceBindings []*WorkspaceBindingCRD
}

// LoadRBACBindings reads all structural binding YAML files from the
// .cog/config/rbac/bindings/ hierarchy. Missing directories are not errors;
// a fresh workspace has no bindings yet.
//
// Two error categories are distinguished:
//   - Fatal I/O errors (unreadable directory, unreadable file) cause an
//     immediate return with nil set and a non-nil error.
//   - Schema errors (YAML parse failures, missing required fields) are
//     accumulated into schemaErrors and the valid files are still returned.
//     This allows partial-load: a single bad file does not block the rest.
//
// Callers that need to surface schema errors in health reporting should
// capture the schemaErrors slice (e.g. into RBACProvider.schemaErrors).
func LoadRBACBindings(root string) (set *RBACBindingSet, schemaErrors []string, err error) {
	base := rbacBindingsDir(root)
	set = &RBACBindingSet{}

	var roleErrs []string
	set.RoleBindings, roleErrs, err = loadRoleBindings(filepath.Join(base, "rolebinding"))
	if err != nil {
		return nil, nil, fmt.Errorf("rbac: load rolebindings: %w", err)
	}
	schemaErrors = append(schemaErrors, roleErrs...)

	var acctErrs []string
	set.AccountBindings, acctErrs, err = loadAccountBindings(filepath.Join(base, "accountbinding"))
	if err != nil {
		return nil, nil, fmt.Errorf("rbac: load accountbindings: %w", err)
	}
	schemaErrors = append(schemaErrors, acctErrs...)

	var nodeErrs []string
	set.NodeBindings, nodeErrs, err = loadNodeBindings(filepath.Join(base, "nodebinding"))
	if err != nil {
		return nil, nil, fmt.Errorf("rbac: load nodebindings: %w", err)
	}
	schemaErrors = append(schemaErrors, nodeErrs...)

	var wsErrs []string
	set.WorkspaceBindings, wsErrs, err = loadWorkspaceBindings(filepath.Join(base, "workspacebinding"))
	if err != nil {
		return nil, nil, fmt.Errorf("rbac: load workspacebindings: %w", err)
	}
	schemaErrors = append(schemaErrors, wsErrs...)

	return set, schemaErrors, nil
}

// loadRoleBindings reads all RoleBindingCRD files from dir. Returns valid
// bindings, any schema errors encountered, and a fatal I/O error if one occurs.
func loadRoleBindings(dir string) ([]*RoleBindingCRD, []string, error) {
	files, err := yamlFilesIn(dir)
	if err != nil {
		return nil, nil, err
	}
	out := make([]*RoleBindingCRD, 0, len(files))
	var errs []string
	for _, f := range files {
		var crd RoleBindingCRD
		if err := decodeYAMLFile(f, &crd); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		if err := validateRoleBinding(&crd); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		out = append(out, &crd)
	}
	return out, errs, nil
}

// loadAccountBindings reads all AccountBindingCRD files from dir. Returns valid
// bindings, any schema errors encountered, and a fatal I/O error if one occurs.
func loadAccountBindings(dir string) ([]*AccountBindingCRD, []string, error) {
	files, err := yamlFilesIn(dir)
	if err != nil {
		return nil, nil, err
	}
	out := make([]*AccountBindingCRD, 0, len(files))
	var errs []string
	for _, f := range files {
		var crd AccountBindingCRD
		if err := decodeYAMLFile(f, &crd); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		if err := validateAccountBinding(&crd); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		out = append(out, &crd)
	}
	return out, errs, nil
}

// loadNodeBindings reads all NodeBindingCRD files from dir. Returns valid
// bindings, any schema errors encountered, and a fatal I/O error if one occurs.
func loadNodeBindings(dir string) ([]*NodeBindingCRD, []string, error) {
	files, err := yamlFilesIn(dir)
	if err != nil {
		return nil, nil, err
	}
	out := make([]*NodeBindingCRD, 0, len(files))
	var errs []string
	for _, f := range files {
		var crd NodeBindingCRD
		if err := decodeYAMLFile(f, &crd); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		if err := validateNodeBinding(&crd); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		out = append(out, &crd)
	}
	return out, errs, nil
}

// loadWorkspaceBindings reads all WorkspaceBindingCRD files from dir. Returns
// valid bindings, any schema errors encountered, and a fatal I/O error if one occurs.
func loadWorkspaceBindings(dir string) ([]*WorkspaceBindingCRD, []string, error) {
	files, err := yamlFilesIn(dir)
	if err != nil {
		return nil, nil, err
	}
	out := make([]*WorkspaceBindingCRD, 0, len(files))
	var errs []string
	for _, f := range files {
		var crd WorkspaceBindingCRD
		if err := decodeYAMLFile(f, &crd); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		if err := validateWorkspaceBinding(&crd); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		out = append(out, &crd)
	}
	return out, errs, nil
}

// ─── Validators ─────────────────────────────────────────────────────────────

// validateRoleBinding checks that required fields are present.
// Returns a descriptive error if any required field is empty.
func validateRoleBinding(crd *RoleBindingCRD) error {
	if crd.Metadata.Name == "" {
		return fmt.Errorf("binding: metadata.name is required")
	}
	if crd.Spec.Subject == "" {
		return fmt.Errorf("binding %q: spec.subject is required", crd.Metadata.Name)
	}
	if crd.Spec.RoleRef == "" {
		return fmt.Errorf("binding %q: spec.role_ref is required", crd.Metadata.Name)
	}
	return nil
}

// validateAccountBinding checks that required fields are present.
func validateAccountBinding(crd *AccountBindingCRD) error {
	if crd.Metadata.Name == "" {
		return fmt.Errorf("binding: metadata.name is required")
	}
	if crd.Spec.Subject == "" {
		return fmt.Errorf("binding %q: spec.subject is required", crd.Metadata.Name)
	}
	if crd.Spec.Node == "" {
		return fmt.Errorf("binding %q: spec.node is required", crd.Metadata.Name)
	}
	if crd.Spec.Account == "" {
		return fmt.Errorf("binding %q: spec.account is required", crd.Metadata.Name)
	}
	return nil
}

// validateNodeBinding checks that required fields are present.
func validateNodeBinding(crd *NodeBindingCRD) error {
	if crd.Metadata.Name == "" {
		return fmt.Errorf("binding: metadata.name is required")
	}
	if crd.Spec.Subject == "" {
		return fmt.Errorf("binding %q: spec.subject is required", crd.Metadata.Name)
	}
	if crd.Spec.Node == "" {
		return fmt.Errorf("binding %q: spec.node is required", crd.Metadata.Name)
	}
	if crd.Spec.Relation == "" {
		return fmt.Errorf("binding %q: spec.relation is required", crd.Metadata.Name)
	}
	return nil
}

// validateWorkspaceBinding checks that required fields are present.
func validateWorkspaceBinding(crd *WorkspaceBindingCRD) error {
	if crd.Metadata.Name == "" {
		return fmt.Errorf("binding: metadata.name is required")
	}
	if crd.Spec.Subject == "" {
		return fmt.Errorf("binding %q: spec.subject is required", crd.Metadata.Name)
	}
	if crd.Spec.WorkspaceURI == "" {
		return fmt.Errorf("binding %q: spec.workspace_uri is required", crd.Metadata.Name)
	}
	if crd.Spec.Access == "" {
		return fmt.Errorf("binding %q: spec.access is required", crd.Metadata.Name)
	}
	return nil
}

// validateHarnessBinding checks that required fields are present.
// Used by callers that construct HarnessBindingCRDs at runtime (AttachHarness).
func validateHarnessBinding(crd *HarnessBindingCRD) error {
	if crd.Metadata.Name == "" {
		return fmt.Errorf("binding: metadata.name is required")
	}
	if crd.Spec.SessionID == "" {
		return fmt.Errorf("binding %q: spec.session_id is required", crd.Metadata.Name)
	}
	if crd.Spec.Subject == "" {
		return fmt.Errorf("binding %q: spec.subject is required", crd.Metadata.Name)
	}
	if crd.Spec.Type == "" {
		return fmt.Errorf("binding %q: spec.type is required", crd.Metadata.Name)
	}
	if crd.Spec.HarnessType == "" {
		return fmt.Errorf("binding %q: spec.harness_type is required", crd.Metadata.Name)
	}
	return nil
}

// ─── Helpers ────────────────────────────────────────────────────────────────

// yamlFilesIn returns the paths of all *.yaml files in dir.
// A missing directory returns an empty slice, not an error.
func yamlFilesIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) == ".yaml" || filepath.Ext(name) == ".yml" {
			out = append(out, filepath.Join(dir, name))
		}
	}
	return out, nil
}

// decodeYAMLFile reads a YAML file and decodes it into dst.
func decodeYAMLFile(path string, dst interface{}) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if err := yaml.Unmarshal(data, dst); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	return nil
}
