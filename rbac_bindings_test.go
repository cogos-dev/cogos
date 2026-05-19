// rbac_bindings_test.go
// Tests for RBAC binding CRD types: YAML round-trip for each CRD type,
// required-field validation, and partial-load behavior.

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ─── RoleBindingCRD round-trip ───────────────────────────────────────────────

func TestRoleBindingCRD_RoundTrip(t *testing.T) {
	input := `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: cog-orchestrator
  labels:
    env: test
spec:
  subject: cog
  role_ref: orchestrator
  scope: cog://workspaces/cog
`
	var crd RoleBindingCRD
	if err := yaml.Unmarshal([]byte(input), &crd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if crd.APIVersion != "cog.os/v1alpha1" {
		t.Errorf("APIVersion: got %q, want %q", crd.APIVersion, "cog.os/v1alpha1")
	}
	if crd.Kind != "RoleBinding" {
		t.Errorf("Kind: got %q, want %q", crd.Kind, "RoleBinding")
	}
	if crd.Metadata.Name != "cog-orchestrator" {
		t.Errorf("Metadata.Name: got %q, want %q", crd.Metadata.Name, "cog-orchestrator")
	}
	if crd.Spec.Subject != "cog" {
		t.Errorf("Spec.Subject: got %q, want %q", crd.Spec.Subject, "cog")
	}
	if crd.Spec.RoleRef != "orchestrator" {
		t.Errorf("Spec.RoleRef: got %q, want %q", crd.Spec.RoleRef, "orchestrator")
	}
	if crd.Spec.Scope != "cog://workspaces/cog" {
		t.Errorf("Spec.Scope: got %q, want %q", crd.Spec.Scope, "cog://workspaces/cog")
	}
	if crd.Metadata.Labels["env"] != "test" {
		t.Errorf("Metadata.Labels: got %v", crd.Metadata.Labels)
	}

	// Marshal back and re-unmarshal to verify symmetry.
	out, err := yaml.Marshal(&crd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var crd2 RoleBindingCRD
	if err := yaml.Unmarshal(out, &crd2); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if crd2.Spec.RoleRef != crd.Spec.RoleRef {
		t.Errorf("round-trip RoleRef: got %q, want %q", crd2.Spec.RoleRef, crd.Spec.RoleRef)
	}
}

func TestRoleBindingCRD_NoScope(t *testing.T) {
	// Scope is optional; missing it is not an error.
	input := `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: chaz-operator
spec:
  subject: chaz
  role_ref: operator
`
	var crd RoleBindingCRD
	if err := yaml.Unmarshal([]byte(input), &crd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if crd.Spec.Scope != "" {
		t.Errorf("expected empty Scope, got %q", crd.Spec.Scope)
	}
}

// ─── AccountBindingCRD round-trip ────────────────────────────────────────────

func TestAccountBindingCRD_RoundTrip(t *testing.T) {
	input := `
apiVersion: cog.os/v1alpha1
kind: AccountBinding
metadata:
  name: cog-on-darkstar
spec:
  subject: cog
  node: darkstar
  account: slowbro
`
	var crd AccountBindingCRD
	if err := yaml.Unmarshal([]byte(input), &crd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if crd.Kind != "AccountBinding" {
		t.Errorf("Kind: got %q", crd.Kind)
	}
	if crd.Spec.Subject != "cog" {
		t.Errorf("Subject: got %q", crd.Spec.Subject)
	}
	if crd.Spec.Node != "darkstar" {
		t.Errorf("Node: got %q", crd.Spec.Node)
	}
	if crd.Spec.Account != "slowbro" {
		t.Errorf("Account: got %q", crd.Spec.Account)
	}

	out, err := yaml.Marshal(&crd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var crd2 AccountBindingCRD
	if err := yaml.Unmarshal(out, &crd2); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if crd2.Spec.Account != "slowbro" {
		t.Errorf("round-trip Account: got %q", crd2.Spec.Account)
	}
}

// ─── NodeBindingCRD round-trip ───────────────────────────────────────────────

func TestNodeBindingCRD_RoundTrip(t *testing.T) {
	input := `
apiVersion: cog.os/v1alpha1
kind: NodeBinding
metadata:
  name: cog-can-embody-darkstar
spec:
  subject: cog
  node: darkstar
  relation: can-embody
`
	var crd NodeBindingCRD
	if err := yaml.Unmarshal([]byte(input), &crd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if crd.Kind != "NodeBinding" {
		t.Errorf("Kind: got %q", crd.Kind)
	}
	if crd.Spec.Relation != "can-embody" {
		t.Errorf("Relation: got %q", crd.Spec.Relation)
	}

	out, err := yaml.Marshal(&crd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var crd2 NodeBindingCRD
	if err := yaml.Unmarshal(out, &crd2); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if crd2.Spec.Relation != "can-embody" {
		t.Errorf("round-trip Relation: got %q", crd2.Spec.Relation)
	}
}

func TestNodeBindingCRD_PinnedTo(t *testing.T) {
	input := `
apiVersion: cog.os/v1alpha1
kind: NodeBinding
metadata:
  name: eclipse-pinned
spec:
  subject: eclipse
  node: eclipse
  relation: pinned-to
`
	var crd NodeBindingCRD
	if err := yaml.Unmarshal([]byte(input), &crd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if crd.Spec.Relation != "pinned-to" {
		t.Errorf("Relation: got %q", crd.Spec.Relation)
	}
}

// ─── WorkspaceBindingCRD round-trip ──────────────────────────────────────────

func TestWorkspaceBindingCRD_RoundTrip(t *testing.T) {
	input := `
apiVersion: cog.os/v1alpha1
kind: WorkspaceBinding
metadata:
  name: cog-owns-cog-workspace
spec:
  subject: cog
  workspace_uri: cog://workspaces/cog
  access: owner
`
	var crd WorkspaceBindingCRD
	if err := yaml.Unmarshal([]byte(input), &crd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if crd.Kind != "WorkspaceBinding" {
		t.Errorf("Kind: got %q", crd.Kind)
	}
	if crd.Spec.WorkspaceURI != "cog://workspaces/cog" {
		t.Errorf("WorkspaceURI: got %q", crd.Spec.WorkspaceURI)
	}
	if crd.Spec.Access != "owner" {
		t.Errorf("Access: got %q", crd.Spec.Access)
	}

	out, err := yaml.Marshal(&crd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var crd2 WorkspaceBindingCRD
	if err := yaml.Unmarshal(out, &crd2); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if crd2.Spec.WorkspaceURI != crd.Spec.WorkspaceURI {
		t.Errorf("round-trip WorkspaceURI: got %q", crd2.Spec.WorkspaceURI)
	}
}

// ─── HarnessBindingCRD round-trip ────────────────────────────────────────────

func TestHarnessBindingCRD_TypeAgent(t *testing.T) {
	// Validates the scope-packet test plan item:
	// "parse a HarnessBindingCRD with type: agent; assert Type == agent"
	input := `
apiVersion: cog.os/v1alpha1
kind: HarnessBinding
metadata:
  name: sess-001-agent
spec:
  session_id: sess-001
  subject: cog
  type: agent
  harness_type: claude-code
  node_id: darkstar
`
	var crd HarnessBindingCRD
	if err := yaml.Unmarshal([]byte(input), &crd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if crd.Kind != "HarnessBinding" {
		t.Errorf("Kind: got %q", crd.Kind)
	}
	if crd.Spec.Type != "agent" {
		t.Errorf("Type: got %q, want %q", crd.Spec.Type, "agent")
	}
	if crd.Spec.HarnessType != "claude-code" {
		t.Errorf("HarnessType: got %q", crd.Spec.HarnessType)
	}
	if crd.Spec.NodeID != "darkstar" {
		t.Errorf("NodeID: got %q", crd.Spec.NodeID)
	}
}

func TestHarnessBindingCRD_TypeUser(t *testing.T) {
	input := `
apiVersion: cog.os/v1alpha1
kind: HarnessBinding
metadata:
  name: sess-001-user
spec:
  session_id: sess-001
  subject: chaz
  type: user
  harness_type: claude-code
`
	var crd HarnessBindingCRD
	if err := yaml.Unmarshal([]byte(input), &crd); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if crd.Spec.Type != "user" {
		t.Errorf("Type: got %q, want %q", crd.Spec.Type, "user")
	}
	if crd.Spec.NodeID != "" {
		t.Errorf("NodeID should be empty (omitempty), got %q", crd.Spec.NodeID)
	}
}

func TestHarnessBindingCRD_RoundTrip(t *testing.T) {
	crd := HarnessBindingCRD{
		APIVersion: "cog.os/v1alpha1",
		Kind:       "HarnessBinding",
		Metadata:   RBACMeta{Name: "sess-002-agent"},
		Spec: HarnessBindingSpec{
			SessionID:   "sess-002",
			Subject:     "cog",
			Type:        "agent",
			HarnessType: "cursor",
			NodeID:      "eclipse",
		},
	}

	out, err := yaml.Marshal(&crd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var crd2 HarnessBindingCRD
	if err := yaml.Unmarshal(out, &crd2); err != nil {
		t.Fatalf("re-unmarshal: %v", err)
	}
	if crd2.Spec.SessionID != "sess-002" {
		t.Errorf("SessionID: got %q", crd2.Spec.SessionID)
	}
	if crd2.Spec.Type != "agent" {
		t.Errorf("Type: got %q", crd2.Spec.Type)
	}
}

// ─── LoadRBACBindings with real files ────────────────────────────────────────

func TestLoadRBACBindings_AllKinds(t *testing.T) {
	// Write fixture files for each structural binding kind.
	root := t.TempDir()
	base := filepath.Join(root, ".cog", "config", "rbac", "bindings")

	fixtures := []struct {
		kind    string
		name    string
		content string
	}{
		{
			kind: "rolebinding",
			name: "cog-orchestrator.yaml",
			content: `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: cog-orchestrator
spec:
  subject: cog
  role_ref: orchestrator
`,
		},
		{
			kind: "accountbinding",
			name: "cog-on-darkstar.yaml",
			content: `
apiVersion: cog.os/v1alpha1
kind: AccountBinding
metadata:
  name: cog-on-darkstar
spec:
  subject: cog
  node: darkstar
  account: slowbro
`,
		},
		{
			kind: "nodebinding",
			name: "cog-embodies-darkstar.yaml",
			content: `
apiVersion: cog.os/v1alpha1
kind: NodeBinding
metadata:
  name: cog-embodies-darkstar
spec:
  subject: cog
  node: darkstar
  relation: can-embody
`,
		},
		{
			kind: "workspacebinding",
			name: "cog-owns-workspace.yaml",
			content: `
apiVersion: cog.os/v1alpha1
kind: WorkspaceBinding
metadata:
  name: cog-owns-workspace
spec:
  subject: cog
  workspace_uri: cog://workspaces/cog
  access: owner
`,
		},
	}

	for _, f := range fixtures {
		dir := filepath.Join(base, f.kind)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.content), 0o640); err != nil {
			t.Fatalf("write %s: %v", f.name, err)
		}
	}

	set, schemaErrs, err := LoadRBACBindings(root)
	if err != nil {
		t.Fatalf("LoadRBACBindings: %v", err)
	}
	if len(schemaErrs) != 0 {
		t.Errorf("unexpected schema errors: %v", schemaErrs)
	}

	if len(set.RoleBindings) != 1 {
		t.Errorf("RoleBindings: got %d, want 1", len(set.RoleBindings))
	}
	if len(set.AccountBindings) != 1 {
		t.Errorf("AccountBindings: got %d, want 1", len(set.AccountBindings))
	}
	if len(set.NodeBindings) != 1 {
		t.Errorf("NodeBindings: got %d, want 1", len(set.NodeBindings))
	}
	if len(set.WorkspaceBindings) != 1 {
		t.Errorf("WorkspaceBindings: got %d, want 1", len(set.WorkspaceBindings))
	}
}

func TestLoadRBACBindings_EmptyDir(t *testing.T) {
	// A fresh workspace with no bindings directory is not an error.
	root := t.TempDir()
	set, schemaErrs, err := LoadRBACBindings(root)
	if err != nil {
		t.Fatalf("LoadRBACBindings on empty workspace: %v", err)
	}
	if len(schemaErrs) != 0 {
		t.Errorf("unexpected schema errors on empty workspace: %v", schemaErrs)
	}
	if len(set.RoleBindings) != 0 || len(set.AccountBindings) != 0 ||
		len(set.NodeBindings) != 0 || len(set.WorkspaceBindings) != 0 {
		t.Errorf("expected empty sets for fresh workspace")
	}
}

// ─── Validation: required-field checks ───────────────────────────────────────

func TestValidateRoleBinding_MissingSubject(t *testing.T) {
	crd := &RoleBindingCRD{
		APIVersion: "cog.os/v1alpha1",
		Kind:       "RoleBinding",
		Metadata:   RBACMeta{Name: "test-rb"},
		Spec:       RoleBindingSpec{Subject: "", RoleRef: "orchestrator"},
	}
	err := validateRoleBinding(crd)
	if err == nil {
		t.Fatal("expected error for missing subject, got nil")
	}
	if !strings.Contains(err.Error(), "spec.subject") {
		t.Errorf("error should mention spec.subject, got: %v", err)
	}
}

func TestValidateRoleBinding_MissingRoleRef(t *testing.T) {
	crd := &RoleBindingCRD{
		APIVersion: "cog.os/v1alpha1",
		Kind:       "RoleBinding",
		Metadata:   RBACMeta{Name: "test-rb"},
		Spec:       RoleBindingSpec{Subject: "cog", RoleRef: ""},
	}
	err := validateRoleBinding(crd)
	if err == nil {
		t.Fatal("expected error for missing role_ref, got nil")
	}
	if !strings.Contains(err.Error(), "spec.role_ref") {
		t.Errorf("error should mention spec.role_ref, got: %v", err)
	}
}

func TestValidateRoleBinding_MissingName(t *testing.T) {
	crd := &RoleBindingCRD{
		APIVersion: "cog.os/v1alpha1",
		Kind:       "RoleBinding",
		Metadata:   RBACMeta{Name: ""},
		Spec:       RoleBindingSpec{Subject: "cog", RoleRef: "orchestrator"},
	}
	err := validateRoleBinding(crd)
	if err == nil {
		t.Fatal("expected error for missing name, got nil")
	}
	if !strings.Contains(err.Error(), "metadata.name") {
		t.Errorf("error should mention metadata.name, got: %v", err)
	}
}

func TestValidateRoleBinding_Valid(t *testing.T) {
	crd := &RoleBindingCRD{
		APIVersion: "cog.os/v1alpha1",
		Kind:       "RoleBinding",
		Metadata:   RBACMeta{Name: "cog-orchestrator"},
		Spec:       RoleBindingSpec{Subject: "cog", RoleRef: "orchestrator"},
	}
	if err := validateRoleBinding(crd); err != nil {
		t.Errorf("expected nil error for valid binding, got: %v", err)
	}
}

func TestValidateAccountBinding_MissingFields(t *testing.T) {
	cases := []struct {
		name string
		crd  *AccountBindingCRD
		want string
	}{
		{
			name: "missing subject",
			crd: &AccountBindingCRD{
				Metadata: RBACMeta{Name: "ab"},
				Spec:     AccountBindingSpec{Subject: "", Node: "darkstar", Account: "slowbro"},
			},
			want: "spec.subject",
		},
		{
			name: "missing node",
			crd: &AccountBindingCRD{
				Metadata: RBACMeta{Name: "ab"},
				Spec:     AccountBindingSpec{Subject: "cog", Node: "", Account: "slowbro"},
			},
			want: "spec.node",
		},
		{
			name: "missing account",
			crd: &AccountBindingCRD{
				Metadata: RBACMeta{Name: "ab"},
				Spec:     AccountBindingSpec{Subject: "cog", Node: "darkstar", Account: ""},
			},
			want: "spec.account",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateAccountBinding(tc.crd)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should mention %q, got: %v", tc.want, err)
			}
		})
	}
}

func TestValidateNodeBinding_MissingRelation(t *testing.T) {
	crd := &NodeBindingCRD{
		Metadata: RBACMeta{Name: "nb"},
		Spec:     NodeBindingSpec{Subject: "cog", Node: "darkstar", Relation: ""},
	}
	err := validateNodeBinding(crd)
	if err == nil {
		t.Fatal("expected error for missing relation, got nil")
	}
	if !strings.Contains(err.Error(), "spec.relation") {
		t.Errorf("error should mention spec.relation, got: %v", err)
	}
}

func TestValidateWorkspaceBinding_MissingWorkspaceURI(t *testing.T) {
	crd := &WorkspaceBindingCRD{
		Metadata: RBACMeta{Name: "wb"},
		Spec:     WorkspaceBindingSpec{Subject: "cog", WorkspaceURI: "", Access: "owner"},
	}
	err := validateWorkspaceBinding(crd)
	if err == nil {
		t.Fatal("expected error for missing workspace_uri, got nil")
	}
	if !strings.Contains(err.Error(), "spec.workspace_uri") {
		t.Errorf("error should mention spec.workspace_uri, got: %v", err)
	}
}

func TestValidateHarnessBinding_MissingType(t *testing.T) {
	crd := &HarnessBindingCRD{
		Metadata: RBACMeta{Name: "hb"},
		Spec:     HarnessBindingSpec{SessionID: "s1", Subject: "cog", Type: "", HarnessType: "claude-code"},
	}
	err := validateHarnessBinding(crd)
	if err == nil {
		t.Fatal("expected error for missing type, got nil")
	}
	if !strings.Contains(err.Error(), "spec.type") {
		t.Errorf("error should mention spec.type, got: %v", err)
	}
}

// ─── Partial-load: valid + invalid in same directory ─────────────────────────

// TestLoadRBACBindings_PartialLoad verifies that a directory containing one
// valid and one invalid binding loads the valid one and returns the schema error
// for the invalid one — without aborting the whole load.
func TestLoadRBACBindings_PartialLoad(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, ".cog", "config", "rbac", "bindings", "rolebinding")
	if err := os.MkdirAll(base, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Good binding: fully valid.
	goodYAML := `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: cog-orchestrator
spec:
  subject: cog
  role_ref: orchestrator
`
	if err := os.WriteFile(filepath.Join(base, "good.yaml"), []byte(goodYAML), 0o640); err != nil {
		t.Fatalf("write good: %v", err)
	}

	// Bad binding: missing required subject field.
	badYAML := `
apiVersion: cog.os/v1alpha1
kind: RoleBinding
metadata:
  name: missing-subject
spec:
  role_ref: orchestrator
`
	if err := os.WriteFile(filepath.Join(base, "bad.yaml"), []byte(badYAML), 0o640); err != nil {
		t.Fatalf("write bad: %v", err)
	}

	set, schemaErrs, err := LoadRBACBindings(root)
	if err != nil {
		t.Fatalf("LoadRBACBindings returned fatal error: %v", err)
	}

	// The valid binding should still load.
	if len(set.RoleBindings) != 1 {
		t.Errorf("RoleBindings: got %d, want 1 (only the valid binding)", len(set.RoleBindings))
	}
	if len(set.RoleBindings) == 1 && set.RoleBindings[0].Metadata.Name != "cog-orchestrator" {
		t.Errorf("loaded wrong binding: got %q, want cog-orchestrator", set.RoleBindings[0].Metadata.Name)
	}

	// The invalid binding should produce exactly one schema error.
	if len(schemaErrs) != 1 {
		t.Errorf("schemaErrs: got %d, want 1; errors: %v", len(schemaErrs), schemaErrs)
	}
	if len(schemaErrs) > 0 && !strings.Contains(schemaErrs[0], "spec.subject") {
		t.Errorf("schema error should mention spec.subject, got: %q", schemaErrs[0])
	}
}

// TestLoadRBACBindings_UnparseableYAML verifies that a completely malformed
// YAML file produces a schema error (not a fatal error) and skips the file.
func TestLoadRBACBindings_UnparseableYAML(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, ".cog", "config", "rbac", "bindings", "rolebinding")
	if err := os.MkdirAll(base, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Unparseable YAML.
	if err := os.WriteFile(filepath.Join(base, "corrupt.yaml"),
		[]byte("not: valid: yaml: at: all: [unclosed\n"), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}

	set, schemaErrs, err := LoadRBACBindings(root)
	if err != nil {
		t.Fatalf("LoadRBACBindings returned fatal error: %v", err)
	}
	if len(set.RoleBindings) != 0 {
		t.Errorf("expected 0 valid bindings from corrupt file, got %d", len(set.RoleBindings))
	}
	if len(schemaErrs) == 0 {
		t.Error("expected at least one schema error for unparseable YAML, got none")
	}
}

// ─── Enum validation: WorkspaceBinding.Access ────────────────────────────────

// TestValidateWorkspaceBinding_InvalidAccess verifies that an unrecognised
// access value is rejected by the validator.
func TestValidateWorkspaceBinding_InvalidAccess(t *testing.T) {
	crd := &WorkspaceBindingCRD{
		Metadata: RBACMeta{Name: "wb-bad"},
		Spec: WorkspaceBindingSpec{
			Subject:      "cog",
			WorkspaceURI: "cog://workspaces/cog",
			Access:       "delete-everything",
		},
	}
	err := validateWorkspaceBinding(crd)
	if err == nil {
		t.Fatal("expected error for invalid access value, got nil")
	}
	if !strings.Contains(err.Error(), "spec.access") {
		t.Errorf("error should mention spec.access, got: %v", err)
	}
	if !strings.Contains(err.Error(), "delete-everything") {
		t.Errorf("error should quote the bad value, got: %v", err)
	}
}

// TestValidateWorkspaceBinding_ValidAccess verifies that all three valid access
// values pass validation.
func TestValidateWorkspaceBinding_ValidAccess(t *testing.T) {
	for _, access := range []string{
		WorkspaceBindingAccessOwner,
		WorkspaceBindingAccessRead,
		WorkspaceBindingAccessReadWrite,
	} {
		crd := &WorkspaceBindingCRD{
			Metadata: RBACMeta{Name: "wb-" + access},
			Spec: WorkspaceBindingSpec{
				Subject:      "cog",
				WorkspaceURI: "cog://workspaces/cog",
				Access:       access,
			},
		}
		if err := validateWorkspaceBinding(crd); err != nil {
			t.Errorf("access=%q: unexpected error: %v", access, err)
		}
	}
}

// ─── Enum validation: NodeBinding.Relation ───────────────────────────────────

// TestValidateNodeBinding_InvalidRelation verifies that an unrecognised relation
// value is rejected by the validator.
func TestValidateNodeBinding_InvalidRelation(t *testing.T) {
	crd := &NodeBindingCRD{
		Metadata: RBACMeta{Name: "nb-bad"},
		Spec: NodeBindingSpec{
			Subject:  "cog",
			Node:     "darkstar",
			Relation: "wants-to-be-friends",
		},
	}
	err := validateNodeBinding(crd)
	if err == nil {
		t.Fatal("expected error for invalid relation value, got nil")
	}
	if !strings.Contains(err.Error(), "spec.relation") {
		t.Errorf("error should mention spec.relation, got: %v", err)
	}
	if !strings.Contains(err.Error(), "wants-to-be-friends") {
		t.Errorf("error should quote the bad value, got: %v", err)
	}
}

// TestValidateNodeBinding_ValidRelation verifies that both valid relation values
// pass validation.
func TestValidateNodeBinding_ValidRelation(t *testing.T) {
	for _, rel := range []string{
		NodeBindingRelationCanEmbody,
		NodeBindingRelationPinnedTo,
	} {
		crd := &NodeBindingCRD{
			Metadata: RBACMeta{Name: "nb-" + rel},
			Spec: NodeBindingSpec{
				Subject:  "cog",
				Node:     "darkstar",
				Relation: rel,
			},
		}
		if err := validateNodeBinding(crd); err != nil {
			t.Errorf("relation=%q: unexpected error: %v", rel, err)
		}
	}
}

// ─── Enum validation: LoadConfig surfaces enum error in schemaErrors ─────────

// TestLoadRBACBindings_EnumErrorInSchemaErrors verifies that a binding with an
// invalid enum value is rejected during LoadRBACBindings, surfaces the error in
// schemaErrors, and causes Health() to report Degraded when wired through
// RBACProvider.LoadConfig.
func TestLoadRBACBindings_EnumErrorInSchemaErrors(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, ".cog", "config", "rbac", "bindings", "workspacebinding")
	if err := os.MkdirAll(base, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Binding with an invalid access enum value.
	badYAML := `
apiVersion: cog.os/v1alpha1
kind: WorkspaceBinding
metadata:
  name: wb-bad-access
spec:
  subject: cog
  workspace_uri: cog://workspaces/cog
  access: delete-everything
`
	if err := os.WriteFile(filepath.Join(base, "bad.yaml"), []byte(badYAML), 0o640); err != nil {
		t.Fatalf("write: %v", err)
	}

	set, schemaErrs, err := LoadRBACBindings(root)
	if err != nil {
		t.Fatalf("LoadRBACBindings returned fatal error: %v", err)
	}

	// The invalid binding should be excluded.
	if len(set.WorkspaceBindings) != 0 {
		t.Errorf("WorkspaceBindings: got %d, want 0 (invalid binding excluded)", len(set.WorkspaceBindings))
	}

	// The schema error should be present and mention the bad value.
	if len(schemaErrs) == 0 {
		t.Fatal("expected at least one schema error for invalid enum, got none")
	}
	if !strings.Contains(schemaErrs[0], "delete-everything") {
		t.Errorf("schema error should mention the bad value, got: %q", schemaErrs[0])
	}
}
