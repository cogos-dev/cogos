// Package l2migration implements the six-step Layer-2 retirement procedure
// from ADR-099 ("Migration guidance for Layer 2 (future wave)",
// docs/adrs/099-node-identity-layering.md):
//
//  1. Load the existing `.cog/identity.json` (Ed25519 NodeIdentity).
//  2. Find-or-generate the ECDSA P-256 identity.
//  3. Derive the new NodeID (hex(sha256(DER(pubkey)))).
//  4. Write a `kind: Node` CRD YAML recording the old/new ID mapping.
//  5. Emit a `node.identity.migrated` ledger event carrying both IDs.
//  6. Preserve the old identity read-only; never delete it.
//
// This package is deliberately NOT wired into any automatic path: nothing
// in this repo calls it from a startup hook, a service, or a default-run
// CLI command. It exists so the procedure ADR-099 describes is tested,
// callable code, ready for a future wave's `cogos node migrate-identity`
// command (ADR-099's second and third gating preconditions, still open) to
// invoke.
//
// The Layer-2 artifact (`<workspaceRoot>/.cog/identity.json`) belongs to
// the separate cog-workspace-CLI module (github.com/myrgic's `cog`
// binary, package main) and cannot be imported here. LegacyIdentity below
// is an independent struct over the same JSON shape, matching the pattern
// the cog workspace CLI's own node_card.go already uses for the same
// reason (identity.json is read by two independent parsers today).
package l2migration

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/myrgic/cogos/pkg/cogblock"
	"github.com/myrgic/constellation"
	"gopkg.in/yaml.v3"
)

// EventType is the bus/ledger event type step 5 emits.
const EventType = "node.identity.migrated"

// LegacyIdentity mirrors the JSON shape of the Layer-2 workspace-CLI's
// `.cog/identity.json` (cog/.cog/node_identity.go NodeIdentity struct).
type LegacyIdentity struct {
	NodeHash         string  `json:"node_hash"`
	PublicKey        string  `json:"public_key"`
	Role             string  `json:"role"`
	GenesisTimestamp string  `json:"genesis_timestamp"`
	Eigenvalue       string  `json:"eigenvalue"`
	ParentHash       *string `json:"parent_hash"`
}

// legacyIdentityPath returns the path to the Layer-2 identity file.
func legacyIdentityPath(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".cog", "identity.json")
}

// LoadLegacyIdentity implements step 1: load the existing Ed25519 identity
// from <workspaceRoot>/.cog/identity.json. Read-only.
func LoadLegacyIdentity(workspaceRoot string) (*LegacyIdentity, error) {
	path := legacyIdentityPath(workspaceRoot)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read legacy identity %q: %w", path, err)
	}

	var id LegacyIdentity
	if err := json.Unmarshal(data, &id); err != nil {
		return nil, fmt.Errorf("parse legacy identity %q: %w", path, err)
	}
	if id.NodeHash == "" {
		return nil, fmt.Errorf("legacy identity %q: missing node_hash", path)
	}
	return &id, nil
}

// EnsureL1Identity implements steps 2-3: find-or-generate the ECDSA P-256
// identity at <identityDir>/node-key.pem (constellation's on-disk format).
// The returned NodeIdentity.NodeID is already hex(sha256(DER(pubkey))) --
// constellation derives it as part of Generate/Load, so there is no
// separate derivation step here.
func EnsureL1Identity(identityDir string) (*constellation.NodeIdentity, error) {
	if id, err := constellation.LoadIdentity(identityDir); err == nil {
		return id, nil
	}

	id, err := constellation.GenerateIdentity()
	if err != nil {
		return nil, fmt.Errorf("generate L1 identity: %w", err)
	}
	if err := constellation.SaveIdentity(id, identityDir); err != nil {
		return nil, fmt.Errorf("save L1 identity to %q: %w", identityDir, err)
	}
	return id, nil
}

// NodeCRD is the `kind: Node` migration-provenance record ADR-099 step 4
// describes. It is a one-time provenance record, not a live reconciled
// resource -- it does not collide with the kernel's `kind: Identity` CRD
// (pkg/substrate/identity), which is a distinct, unrelated schema for
// principal (agent/human/service) identity.
type NodeCRD struct {
	APIVersion string      `yaml:"apiVersion"`
	Kind       string      `yaml:"kind"`
	Metadata   NodeCRDMeta `yaml:"metadata"`
	Spec       NodeCRDSpec `yaml:"spec"`
}

// NodeCRDMeta matches the standard CRD metadata shape.
type NodeCRDMeta struct {
	Name string `yaml:"name"`
}

// NodeCRDSpec holds the old-to-new identity mapping ADR-099 step 4 lists.
type NodeCRDSpec struct {
	OldNodeHash string `yaml:"old_node_hash"`
	NewNodeID   string `yaml:"new_node_id"`
	MigrationTS string `yaml:"migration_ts"`
}

// NodeCRD apiVersion/kind constants, validated the same way the Identity
// CRD's are (see pkg/substrate/identity.APIVersion / .Kind).
const (
	NodeCRDAPIVersion = "cog.os/v1alpha1"
	NodeCRDKind       = "Node"
)

// nodeCRDPath returns the ADR-099 step-4 path:
// <root>/.cog/config/nodes/<oldNodeHash>.yaml.
func nodeCRDPath(root, oldNodeHash string) string {
	return filepath.Join(root, ".cog", "config", "nodes", oldNodeHash+".yaml")
}

// WriteNodeCRD implements step 4: write the `kind: Node` CRD YAML.
func WriteNodeCRD(workspaceRoot, oldNodeHash, newNodeID string, migrationTS time.Time) (string, error) {
	path := nodeCRDPath(workspaceRoot, oldNodeHash)
	crd := NodeCRD{
		APIVersion: NodeCRDAPIVersion,
		Kind:       NodeCRDKind,
		Metadata:   NodeCRDMeta{Name: oldNodeHash},
		Spec: NodeCRDSpec{
			OldNodeHash: oldNodeHash,
			NewNodeID:   newNodeID,
			MigrationTS: migrationTS.UTC().Format(time.RFC3339),
		},
	}

	data, err := yaml.Marshal(&crd)
	if err != nil {
		return "", fmt.Errorf("marshal node CRD: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create node CRD dir: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write node CRD %q: %w", path, err)
	}
	return path, nil
}

// EmitMigratedEvent implements step 5: append a node.identity.migrated
// event carrying both IDs to the workspace ledger, via the same
// pkg/cogblock.AppendEvent path the Layer-2 identity's own genesis event
// (cog/.cog/node_identity.go EmitGenesisEvent) already uses -- the
// substrate's canonical hash-chained event log serves as the bus here.
func EmitMigratedEvent(workspaceRoot, sessionID, oldNodeHash, newNodeID string) error {
	envelope := cogblock.NewEventEnvelope(EventType, sessionID).
		WithSource("l2migration").
		WithData("old_node_hash", oldNodeHash).
		WithData("new_node_id", newNodeID)

	if err := cogblock.AppendEvent(workspaceRoot, sessionID, envelope); err != nil {
		return fmt.Errorf("append %s event: %w", EventType, err)
	}
	return nil
}

// MakeLegacyIdentityReadOnly implements step 6: preserve
// `.cog/identity.json` as a read-only artifact. This package has no
// delete path for it anywhere -- this chmod is the only step-6 action, and
// it is idempotent and content-preserving.
func MakeLegacyIdentityReadOnly(workspaceRoot string) error {
	path := legacyIdentityPath(workspaceRoot)
	if err := os.Chmod(path, 0o444); err != nil {
		return fmt.Errorf("chmod legacy identity read-only %q: %w", path, err)
	}
	return nil
}

// Result summarizes a completed migration run.
type Result struct {
	OldNodeHash string
	NewNodeID   string
	CRDPath     string
	MigratedAt  time.Time
}

// Migrate runs ADR-099's six-step Layer-2 retirement procedure end to end
// against workspaceRoot, using identityDir as the ECDSA P-256 identity
// directory (ADR-099 step 2: "~/.cog/node/identity/" in production; a
// parameter here, never hardcoded, so tests run against a throwaway temp
// dir and this function never touches a real machine identity).
//
// Exported and fully tested, but -- per this package's doc comment --
// deliberately called from nowhere else in this repo. The intended caller
// is a future `cogos node migrate-identity` command.
func Migrate(workspaceRoot, identityDir, sessionID string) (*Result, error) {
	legacy, err := LoadLegacyIdentity(workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("step 1 (load legacy identity): %w", err)
	}

	l1, err := EnsureL1Identity(identityDir)
	if err != nil {
		return nil, fmt.Errorf("steps 2-3 (ensure L1 identity): %w", err)
	}

	migratedAt := time.Now().UTC()
	crdPath, err := WriteNodeCRD(workspaceRoot, legacy.NodeHash, l1.NodeID, migratedAt)
	if err != nil {
		return nil, fmt.Errorf("step 4 (write node CRD): %w", err)
	}

	if err := EmitMigratedEvent(workspaceRoot, sessionID, legacy.NodeHash, l1.NodeID); err != nil {
		return nil, fmt.Errorf("step 5 (emit migrated event): %w", err)
	}

	if err := MakeLegacyIdentityReadOnly(workspaceRoot); err != nil {
		return nil, fmt.Errorf("step 6 (preserve legacy identity read-only): %w", err)
	}

	return &Result{
		OldNodeHash: legacy.NodeHash,
		NewNodeID:   l1.NodeID,
		CRDPath:     crdPath,
		MigratedAt:  migratedAt,
	}, nil
}
