// rbac_bindings.go — Zero-churn root alias shim. Canonical RBAC CRD types,
// loaders, and validators live in pkg/substrate/identity per ADR-100 P0
// extraction.
//
// Type aliases and forwarding functions let existing call sites
// (rbac_provider.go, rbac_bindings_test.go) compile unchanged. New code
// should prefer the pkg/substrate/identity import path.

package main

import (
	subidentity "github.com/myrgic/cogos/pkg/substrate/identity"
)

// ─── Type aliases ────────────────────────────────────────────────────────────

// RBACMeta is the standard metadata envelope for all RBAC binding CRDs.
// Canonical home: pkg/substrate/identity.RBACMeta.
type RBACMeta = subidentity.RBACMeta

// RoleBindingCRD binds a subject identity to a named role.
// Canonical home: pkg/substrate/identity.RoleBindingCRD.
type RoleBindingCRD = subidentity.RoleBindingCRD

// RoleBindingSpec declares the subject/role pairing.
// Canonical home: pkg/substrate/identity.RoleBindingSpec.
type RoleBindingSpec = subidentity.RoleBindingSpec

// AccountBindingCRD binds a substrate identity to a local OS account.
// Canonical home: pkg/substrate/identity.AccountBindingCRD.
type AccountBindingCRD = subidentity.AccountBindingCRD

// AccountBindingSpec declares the identity → node → OS-account triple.
// Canonical home: pkg/substrate/identity.AccountBindingSpec.
type AccountBindingSpec = subidentity.AccountBindingSpec

// NodeBindingCRD asserts a subject identity's relationship to a node.
// Canonical home: pkg/substrate/identity.NodeBindingCRD.
type NodeBindingCRD = subidentity.NodeBindingCRD

// NodeBindingSpec declares the subject → node relationship.
// Canonical home: pkg/substrate/identity.NodeBindingSpec.
type NodeBindingSpec = subidentity.NodeBindingSpec

// WorkspaceBindingCRD is the authoritative binding record for workspace access.
// Canonical home: pkg/substrate/identity.WorkspaceBindingCRD.
type WorkspaceBindingCRD = subidentity.WorkspaceBindingCRD

// WorkspaceBindingSpec declares the subject → workspace-URI → access triple.
// Canonical home: pkg/substrate/identity.WorkspaceBindingSpec.
type WorkspaceBindingSpec = subidentity.WorkspaceBindingSpec

// HarnessBindingCRD links a live harness session to the identity it embodies.
// Canonical home: pkg/substrate/identity.HarnessBindingCRD.
type HarnessBindingCRD = subidentity.HarnessBindingCRD

// HarnessBindingSpec declares the session → identity pairing.
// Canonical home: pkg/substrate/identity.HarnessBindingSpec.
type HarnessBindingSpec = subidentity.HarnessBindingSpec

// RBACBindingSet holds all loaded structural binding records.
// Canonical home: pkg/substrate/identity.RBACBindingSet.
type RBACBindingSet = subidentity.RBACBindingSet

// ─── Enum constant aliases ────────────────────────────────────────────────────

// WorkspaceBinding.Spec.Access valid values.
const (
	WorkspaceBindingAccessOwner     = subidentity.WorkspaceBindingAccessOwner
	WorkspaceBindingAccessRead      = subidentity.WorkspaceBindingAccessRead
	WorkspaceBindingAccessReadWrite = subidentity.WorkspaceBindingAccessReadWrite
)

// NodeBinding.Spec.Relation valid values.
const (
	NodeBindingRelationCanEmbody = subidentity.NodeBindingRelationCanEmbody
	NodeBindingRelationPinnedTo  = subidentity.NodeBindingRelationPinnedTo
)

// ─── Forwarding functions ─────────────────────────────────────────────────────

// LoadRBACBindings reads all structural binding YAML files from the
// .cog/config/rbac/bindings/ hierarchy.
// Canonical home: pkg/substrate/identity.LoadRBACBindings.
func LoadRBACBindings(root string) (set *RBACBindingSet, schemaErrors []string, err error) {
	return subidentity.LoadRBACBindings(root)
}

// rbacBindingsDir returns the root directory for structural RBAC binding YAML files.
// Canonical home: pkg/substrate/identity.RBACBindingsDir.
func rbacBindingsDir(root string) string {
	return subidentity.RBACBindingsDir(root)
}

// rbacKindDir returns the per-kind subdirectory path (lowercased kind name).
// Canonical home: pkg/substrate/identity.RBACKindDir.
func rbacKindDir(root, kind string) string {
	return subidentity.RBACKindDir(root, kind)
}

// validateRoleBinding delegates to pkg/substrate/identity.ValidateRoleBinding.
// Canonical home: pkg/substrate/identity.ValidateRoleBinding.
func validateRoleBinding(crd *RoleBindingCRD) error {
	return subidentity.ValidateRoleBinding(crd)
}

// validateAccountBinding delegates to pkg/substrate/identity.ValidateAccountBinding.
// Canonical home: pkg/substrate/identity.ValidateAccountBinding.
func validateAccountBinding(crd *AccountBindingCRD) error {
	return subidentity.ValidateAccountBinding(crd)
}

// validateNodeBinding delegates to pkg/substrate/identity.ValidateNodeBinding.
// Canonical home: pkg/substrate/identity.ValidateNodeBinding.
func validateNodeBinding(crd *NodeBindingCRD) error {
	return subidentity.ValidateNodeBinding(crd)
}

// validateWorkspaceBinding delegates to pkg/substrate/identity.ValidateWorkspaceBinding.
// Canonical home: pkg/substrate/identity.ValidateWorkspaceBinding.
func validateWorkspaceBinding(crd *WorkspaceBindingCRD) error {
	return subidentity.ValidateWorkspaceBinding(crd)
}

// validateHarnessBinding delegates to pkg/substrate/identity.ValidateHarnessBinding.
// Canonical home: pkg/substrate/identity.ValidateHarnessBinding.
func validateHarnessBinding(crd *HarnessBindingCRD) error {
	return subidentity.ValidateHarnessBinding(crd)
}
