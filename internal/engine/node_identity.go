// node_identity.go — machine-scoped resolution of the kernel's NodeID.
//
// # The bug this closes
//
// `cogos start` runs the child kernel in a container with the workspace bind-
// mounted at the SAME absolute path as on the host (daemon_lifecycle.go, the
// `-v WorkspaceRoot:WorkspaceRoot -w WorkspaceRoot` invocation). The resolved
// node id used to be cached at `<WorkspaceRoot>/.cog/run/node_id`, i.e. INSIDE
// that mount, and an already-persisted id is authoritative and never rewritten.
// So a containerized child read the host's file and adopted the host's identity
// verbatim — two kernels, one NodeID, both stamping the same SourceIdentity onto
// every CogBlock they emit. The file is also git-tracked, so the same clone
// followed the workspace to any machine that checked it out.
//
// Certificates never had this problem, and the contrast is the whole design.
// The BEP cert dir is $HOME-anchored (`bep.CertDir()` → `os.UserHomeDir()` +
// "/.cog/etc"), so it sits OUTSIDE the bind mount. A child container with a
// fresh $HOME finds no cert and mints its own. Identity failed OPEN by cloning;
// certs failed CLOSED by not being reachable at all. This file makes identity
// use the same anchor that already protected the certs.
//
// # Governing rulings
//
// RFC-036, operator ruling 2026-07-29 ("Seam B settled: node = hardware,
// workspace = the overlay"): NodeID is machine-scoped (L1). Two workspaces on
// one machine SHOULD resolve the SAME NodeID — same body, many overlays. The
// anchoring chain is: BEP device cert → DeviceID → NodeID (L1, machine)
// ← observed-by ← workspace overlay (L2, distributed).
//
// That ruling is why the fix is a CACHE-LOCATION change and not a new identity
// scheme. Minting stays exactly as cogos#474 built it — cert-anchored, with
// ensureBEPDeviceIdentity() creating the keypair when absent and a UUID only as
// last resort. Nothing here re-implements or bypasses that chain.
//
// # CONFLICT LOG (recorded, deliberately NOT resolved here)
//
// ADR-065 §7 places daemon runtime state at workspace-scoped
// `.cog/run/daemon/state.yaml` (implemented verbatim in daemon_lifecycle.go),
// and ADR-065 §9 ("Recursive Nesting") explicitly blesses a CogOS kernel running
// inside a container managed by another CogOS kernel — which is what makes the
// clone reachable. RFC-033 pushes the opposite direction: node-runtime state is
// machine-local, never in the shared workspace `.cog/`. This change moves
// IDENTITY only and leaves daemon state exactly where ADR-065 put it. Deciding
// that conflict, and whether the machine tier is `~/.cog/` or RFC-033's
// `~/.cogos/`, is the operator's call; when it lands it is a one-line change to
// defaultNodeIdentityDir below, and COG_NODE_DIR is already the migration seam.
//
// # INVARIANT
//
// Nothing in this file may join a path against cfg.WorkspaceRoot or cfg.CogDir
// except inside legacy adoption, which is READ-ONLY and gated. The kernel must
// never create or modify a node id inside the workspace. Static check:
//
//	grep -n 'WorkspaceRoot\|CogDir' internal/engine/node_identity.go
package engine

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/myrgic/cogos/pkg/substrate/bep"
	"gopkg.in/yaml.v3"
)

const (
	// nodeIDEnvVar pins the node id explicitly. Deliberately COG_NODE_ID and
	// NOT COGOS_NODE_ID: the latter is already taken by the harness presence
	// hooks (session-start.d/51-presence-started.py), where it is documented as
	// `$(hostname -s)` and returned verbatim as a hostname-shaped NAME. Honoring
	// that variable here would let an operator who exports COGOS_NODE_ID=node-a
	// to stabilize presence events silently replace the kernel's node id — and
	// therefore SourceIdentity on every emitted CogBlock — with the string
	// "node-a". Different namespace, different meaning, kept apart on purpose.
	nodeIDEnvVar = "COG_NODE_ID"

	// nodeDirEnvVar overrides the machine-local node dir. Kernel-owned and
	// distinct from the harness-owned COGOS_RUN_DIR (which points at
	// ~/.cogos/run and holds session-presence sentinels no Go code reads).
	nodeDirEnvVar = "COG_NODE_DIR"

	nodeIDFileName     = "node_id"
	nodeIDSourceFile   = "node_id.source"
	nodeRegistryFile   = "global.yaml"
	legacyNodeIDSubdir = "run"
)

// nodeIDDir resolves the machine-local node dir. It is a package var solely so
// tests can redirect it and stay hermetic; production always resolves the
// canonical $HOME-anchored dir.
var nodeIDDir = defaultNodeIdentityDir

// defaultNodeIdentityDir returns the machine-local node dir, or an error.
//
// It NEVER returns a relative path. The pre-existing defaultNodeDir() in
// uri_registry.go falls back to "." when os.UserHomeDir() fails, and in both
// deployment shapes cwd IS the workspace root (the launchd plist sets
// WorkingDirectory to it; NerdctlRuntime.Start passes `-w WorkspaceRoot`). A "."
// fallback here would therefore write the minted id straight back into the
// shared workspace and recreate the clone one level up. An error is the only
// safe answer: the caller degrades to an ephemeral id for that boot and says so.
func defaultNodeIdentityDir() (string, error) {
	if v := strings.TrimSpace(os.Getenv(nodeDirEnvVar)); v != "" {
		if !filepath.IsAbs(v) {
			return "", fmt.Errorf("%s must be an absolute path, got %q", nodeDirEnvVar, v)
		}
		return filepath.Clean(v), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	if strings.TrimSpace(home) == "" {
		return "", fmt.Errorf("resolve home dir: empty")
	}
	return filepath.Join(home, ".cog", "node"), nil
}

// resolveNodeID returns this MACHINE's node id, resolved in a fixed order:
//
//  1. COG_NODE_ID, when it parses as a UUID or a BEP DeviceID.
//  2. <nodeDir>/node_id — the machine-local cache. Steady state, zero writes.
//  3. Gated, one-shot adoption of a pre-existing workspace-scoped id, so that
//     upgrading a real host preserves the identity already in its ledger.
//  4. Mint, cert-anchored, exactly as cogos#474 does it.
//
// Only step 3 reads anything inside the workspace, and it only ever reads.
func resolveNodeID(cfg *Config) string {
	if id, ok := pinnedNodeID(); ok {
		return id
	}

	dir, err := nodeIDDir()
	if err != nil {
		// Cannot anchor to the machine. Minting an ephemeral id is strictly
		// better than falling back to the workspace: a per-boot id is merely
		// unstable, whereas a workspace-scoped id is WRONG (it is another
		// machine's, and it propagates).
		id := mintNodeID()
		slog.Warn("nodeid: no machine-local node dir; using an ephemeral id for this boot",
			"err", fmt.Sprintf("%v", err), "node_id", id,
			"hint", "set "+nodeDirEnvVar+" to an absolute path to make this stable")
		return id
	}

	path := filepath.Join(dir, nodeIDFileName)
	if id := readNodeIDFile(path); id != "" {
		warnOnLegacyDivergence(cfg, id)
		return id
	}

	// Probe the migration gate BEFORE minting. mintNodeID may call
	// ensureBEPDeviceIdentity, which CREATES the cert — and the cert's presence
	// is one of the two gate probes. Probing afterwards would open the gate on
	// the very container the gate exists to keep closed.
	priorTier := machineHasPriorNodeTier(dir)

	var id, source string
	if priorTier {
		id, source = adoptLegacyNodeID(cfg)
		if id != "" {
			slog.Info("nodeid: adopted pre-existing workspace node id as this machine's identity",
				"node_id", id, "source", source, "node_dir", dir)
		}
	}
	if id == "" {
		id = mintNodeID()
		source = "minted"
		if !priorTier {
			slog.Info("nodeid: no prior machine node tier; minted a fresh machine identity",
				"node_id", id, "node_dir", dir)
		}
	}

	persistNodeID(dir, id, source)
	warnOnLegacyDivergence(cfg, id)
	return id
}

// pinnedNodeID reads COG_NODE_ID, validating its shape. An unparseable value is
// rejected loudly rather than becoming the node's identity: this variable ends
// up on every ledger block, so a typo is not a thing to absorb silently.
func pinnedNodeID() (string, bool) {
	v := strings.TrimSpace(os.Getenv(nodeIDEnvVar))
	if v == "" {
		return "", false
	}
	if _, err := uuid.Parse(v); err == nil {
		return v, true
	}
	if _, err := bep.ParseDeviceID(v); err == nil {
		return v, true
	}
	slog.Error("nodeid: ignoring malformed "+nodeIDEnvVar+"; expected a UUID or a BEP device id",
		"value", v)
	return "", false
}

// machineHasPriorNodeTier reports whether THIS MACHINE was already running
// CogOS before the node tier existed — the only situation in which adopting a
// workspace-scoped id is correct.
//
// Both probes are $HOME-anchored, which is the entire point: inside a container
// with a fresh $HOME they answer false even though the host's workspace is bind-
// mounted at the identical absolute path and both legacy artifacts are sitting
// right there, readable. This is deliberately the same anchor that already makes
// certificates fail closed, rather than a second, weaker mechanism.
//
// Note it does NOT branch on COG_DAEMON_MODE=container: that would only cover
// cogos-launched containers, would miss a hand-rolled `docker run`, and would
// make the safety property depend on a launcher-supplied env var instead of on
// the filesystem.
func machineHasPriorNodeTier(dir string) bool {
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		return true
	}
	return nodeFileExists(filepath.Join(nodeIDCertDir(), "bep-cert.pem"))
}

// adoptLegacyNodeID returns the pre-existing workspace-scoped id this machine
// should keep, and the path it came from.
//
// Source order is deterministic and independent of which workspace happens to
// boot first. That matters: this machine has THREE workspaces carrying three
// different legacy ids, and "whichever kernel starts first seals the machine
// identity" would be a coin flip that could silently re-identify the live
// kernel. The node-local registry (~/.cog/node/global.yaml) already names the
// machine's current workspace, is $HOME-anchored, and is therefore consulted
// first; the booting workspace is only the fallback.
func adoptLegacyNodeID(cfg *Config) (string, string) {
	for _, cand := range legacyNodeIDCandidates(cfg) {
		if id := readNodeIDFile(cand); id != "" {
			return id, cand
		}
	}
	return "", ""
}

// legacyNodeIDCandidates lists legacy node_id paths in adoption priority order,
// de-duplicated. Read-only paths, every one.
func legacyNodeIDCandidates(cfg *Config) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}

	if root := registryCurrentWorkspaceRoot(); root != "" {
		add(filepath.Join(root, ".cog", legacyNodeIDSubdir, nodeIDFileName))
	}
	if cfg != nil && strings.TrimSpace(cfg.CogDir) != "" {
		add(filepath.Join(cfg.CogDir, legacyNodeIDSubdir, nodeIDFileName))
	}
	return out
}

// registryCurrentWorkspaceRoot resolves ~/.cog/node/global.yaml's
// current-workspace to a filesystem root, or "" when unavailable. Best-effort by
// design: a missing, stale, or unparseable registry simply yields no candidate
// and adoption falls through to the booting workspace.
func registryCurrentWorkspaceRoot() string {
	dir, err := nodeIDDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, nodeRegistryFile))
	if err != nil {
		return ""
	}
	var reg struct {
		CurrentWorkspace string `yaml:"current-workspace"`
		Workspaces       map[string]struct {
			Path string `yaml:"path"`
		} `yaml:"workspaces"`
	}
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return ""
	}
	entry, ok := reg.Workspaces[strings.TrimSpace(reg.CurrentWorkspace)]
	if !ok {
		return ""
	}
	return strings.TrimSpace(entry.Path)
}

// mintNodeID mints a fresh identity. This is cogos#474's chain, unchanged:
// prefer the node's BEP device identity so that process.NodeID and the peer id
// presented on the wire are ONE value (RFC-036), creating the keypair when none
// exists, and degrading to a UUID only when a device identity cannot be
// established at all.
func mintNodeID() string {
	id := bepAnchoredNodeID()
	if id == "" {
		if err := ensureBEPDeviceIdentity(); err == nil {
			id = bepAnchoredNodeID()
		} else {
			slog.Debug("nodeid: could not mint BEP device identity; using UUID",
				"err", fmt.Sprintf("%v", err))
		}
	}
	if id == "" {
		id = uuid.NewString()
	}
	return id
}

// persistNodeID caches the id machine-locally, alongside a marker recording
// where it came from so an adoption is observable after the fact rather than
// being an invisible one-time coin flip.
//
// A persistence failure is warned about and swallowed: the caller keeps the id
// for this boot. It must never reopen the workspace as a fallback location.
func persistNodeID(dir, id, source string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		slog.Warn("nodeid: cannot create machine node dir; identity is ephemeral this boot",
			"dir", dir, "err", fmt.Sprintf("%v", err))
		return
	}
	if err := os.WriteFile(filepath.Join(dir, nodeIDFileName), []byte(id+"\n"), 0o644); err != nil {
		slog.Warn("nodeid: cannot persist machine node id; identity is ephemeral this boot",
			"dir", dir, "err", fmt.Sprintf("%v", err))
		return
	}
	if source == "" {
		return
	}
	marker := fmt.Sprintf("source: %s\nnode_id: %s\n", source, id)
	if err := os.WriteFile(filepath.Join(dir, nodeIDSourceFile), []byte(marker), 0o644); err != nil {
		slog.Debug("nodeid: could not write provenance marker",
			"dir", dir, "err", fmt.Sprintf("%v", err))
	}
}

// warnOnLegacyDivergence surfaces the case where the booting workspace still
// carries a different id than the machine now uses. Two files that can disagree
// indefinitely is exactly the ambiguity this change exists to remove, and the
// workspace copy is git-tracked, so it keeps shipping to every clone. Loud
// enough to act on, non-fatal because the machine-local value is authoritative.
func warnOnLegacyDivergence(cfg *Config, active string) {
	if cfg == nil || strings.TrimSpace(cfg.CogDir) == "" {
		return
	}
	legacyPath := filepath.Join(cfg.CogDir, legacyNodeIDSubdir, nodeIDFileName)
	legacy := readNodeIDFile(legacyPath)
	if legacy == "" || legacy == active {
		return
	}
	slog.Warn("nodeid: workspace carries a stale node id that is no longer used; node identity is machine-local",
		"workspace_node_id", legacy, "machine_node_id", active, "stale_path", legacyPath,
		"hint", "the workspace copy is ignored and safe to remove from version control")
}

// readNodeIDFile returns the trimmed id at path, or "" for any error/empty file.
func readNodeIDFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// nodeFileExists reports whether path exists (of any type).
func nodeFileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
