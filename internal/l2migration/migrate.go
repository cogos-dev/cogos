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
// KNOWN GAP a future caller must resolve before wiring this up: the L1
// NodeID this package mints/loads (constellation ECDSA-P256-DER, per
// ADR-099's 2026-05-18 text) is not confirmed to be the kernel's live,
// operative NodeID, which has been machine-scoped and BEP-device-cert-
// anchored since RFC-036's 2026-07-29 ruling (internal/engine/
// node_identity.go). Migrate's Result.Warnings flags this on every call;
// see EnsureL1Identity's doc comment and this ADR's "Conflict log" section
// for the detail. Do not treat spec.new_node_id in the CRD this package
// writes as automatically equal to what the kernel puts on the wire.
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
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/myrgic/cogos/pkg/cogblock"
	"github.com/myrgic/cogos/pkg/pathsafe"
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
//
// ADR-099 step 2 is an EXISTENCE check ("Check if a node-key.pem already
// exists; if not, generate one"), not a loadability check. This function
// honors that literally: it stats the key file first, and only generates
// when the file is genuinely absent. A file that exists but fails to load
// (partial write from an interrupted prior run, wrong PEM block type,
// unreadable permissions) is a hard error, never a generate trigger --
// constellation.SaveIdentity truncates node-key.pem
// (O_WRONLY|O_CREATE|O_TRUNC) before writing, so silently falling through to
// generate+save on any load error would irrecoverably destroy an existing
// L1 private key and hand the node a brand-new NodeID with no record that
// the old one was ever there.
//
// KNOWN GAP, not resolved here: the value this function mints/loads is
// constellation's ECDSA-P256-DER-derived NodeID, which is the ID ADR-099
// (2026-05-18) specifies. It is NOT necessarily the kernel's live,
// operative NodeID as of RFC-036's 2026-07-29 ruling -- see this file's
// package doc comment and this package's ADR-099 addendum for the details.
// Migrate's Result.Warnings surfaces this gap on every run so no caller can
// treat spec.new_node_id as automatically wired to what the kernel puts on
// the wire.
func EnsureL1Identity(identityDir string) (*constellation.NodeIdentity, error) {
	keyPath := filepath.Join(identityDir, "node-key.pem")

	switch _, statErr := os.Stat(keyPath); {
	case statErr == nil:
		// The key file exists. It must load -- generating a replacement here
		// would truncate and destroy it.
		id, err := constellation.LoadIdentity(identityDir)
		if err != nil {
			return nil, fmt.Errorf("L1 identity key exists at %q but failed to load; refusing to regenerate and overwrite it: %w", keyPath, err)
		}
		return id, nil
	case errors.Is(statErr, fs.ErrNotExist):
		// Genuinely absent: fall through to generate.
	default:
		// Some other stat failure (e.g. EACCES on the containing directory).
		// Absent and unreachable are not the same thing -- do not generate.
		return nil, fmt.Errorf("stat L1 identity key %q: %w", keyPath, statErr)
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
//
// NewNodeID is the constellation ECDSA-P256-DER-derived NodeID ADR-099
// specifies, not a value independently confirmed to be what the kernel
// currently stamps as SourceIdentity on the wire -- see EnsureL1Identity's
// "KNOWN GAP" doc comment. Treat this record as provenance for the L2->L1
// migration ADR-099 describes, not as proof the new ID is the node's live
// operative identity.
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
//
// oldNodeHash is untrusted, file-sourced content (LoadLegacyIdentity only
// checks it is non-empty) and its real-world shape ("sha256:<hex>") always
// contains a colon, which is illegal in an NTFS path component -- and an
// unsanitized value could equally smuggle a ".." traversal segment. Run it
// through pathsafe.SanitizeComponent, the package this repo already uses at
// every other seam that joins a caller-supplied identifier into a path
// (pkg/cogblock/ledger.go, internal/engine/uri.go), before it becomes a
// filename.
func nodeCRDPath(root, oldNodeHash string) string {
	return filepath.Join(root, ".cog", "config", "nodes", pathsafe.SanitizeComponent(oldNodeHash)+".yaml")
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

	// Warnings carries known, unresolved gaps a caller must not ignore.
	// Populated on every run, success or not (see the field's contents
	// below) -- this is not an error path, it is a "read this before you
	// trust the record" signal for whatever wires up
	// `cogos node migrate-identity` against this package's output.
	Warnings []string
}

// l1IdentityScopeWarning is the standing, unresolved gap this package
// cannot close on its own: EnsureL1Identity mints/loads the constellation
// ECDSA-P256-DER-derived NodeID ADR-099 (2026-05-18) specifies, but the
// kernel's live, operative NodeID has been machine-scoped and
// BEP-device-cert-anchored since RFC-036's 2026-07-29 operator ruling
// (internal/engine/node_identity.go). Nothing in this package checks
// whether the two coincide, and there is no evidence they do. Resolving
// which value is canonical is the operator decision this ADR's own
// "Conflict log" section defers, not an implementation detail this package
// can paper over.
const l1IdentityScopeWarning = "L1 NodeID minted/loaded here (constellation ECDSA-P256-DER) is not confirmed to match the kernel's live BEP-anchored NodeID (RFC-036, 2026-07-29 ruling); resolving that is an open operator decision, see ADR-099's Conflict log"

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

	// Steps 4-5 are made idempotent on re-run: if a CRD for this legacy hash
	// was already written by a prior Migrate call, re-emitting the CRD write
	// and the ledger event would append a second, indistinguishable
	// node.identity.migrated record to the hash-chained (append-only, no
	// dedup) ledger -- a false "migrated twice" signal with no way to tell
	// it apart from a real second migration. Detect the existing CRD and
	// skip straight to returning the already-recorded mapping.
	crdPath := nodeCRDPath(workspaceRoot, legacy.NodeHash)
	migratedAt := time.Now().UTC()
	warnings := []string{l1IdentityScopeWarning}

	if existing, statErr := os.Stat(crdPath); statErr == nil && !existing.IsDir() {
		if err := MakeLegacyIdentityReadOnly(workspaceRoot); err != nil {
			return nil, fmt.Errorf("step 6 (preserve legacy identity read-only): %w", err)
		}
		return &Result{
			OldNodeHash: legacy.NodeHash,
			NewNodeID:   l1.NodeID,
			CRDPath:     crdPath,
			MigratedAt:  existing.ModTime().UTC(),
			Warnings:    append(warnings, "CRD for this legacy hash already existed; steps 4-5 were skipped on this run to avoid a duplicate ledger event"),
		}, nil
	}

	crdPath, err = WriteNodeCRD(workspaceRoot, legacy.NodeHash, l1.NodeID, migratedAt)
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
		Warnings:    warnings,
	}, nil
}
