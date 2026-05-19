// rbac_bindings_test.go
// Tests for RBAC binding CRD types: YAML round-trip for each CRD type.

package main

import (
	"os"
	"path/filepath"
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

	set, err := LoadRBACBindings(root)
	if err != nil {
		t.Fatalf("LoadRBACBindings: %v", err)
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
	set, err := LoadRBACBindings(root)
	if err != nil {
		t.Fatalf("LoadRBACBindings on empty workspace: %v", err)
	}
	if len(set.RoleBindings) != 0 || len(set.AccountBindings) != 0 ||
		len(set.NodeBindings) != 0 || len(set.WorkspaceBindings) != 0 {
		t.Errorf("expected empty sets for fresh workspace")
	}
}
