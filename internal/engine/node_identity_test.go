package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/myrgic/cogos/pkg/substrate/bep"
)

// These tests pin the machine-scoping of node identity.
//
// The bug: `cogos start` bind-mounts the workspace into the child container at
// the SAME absolute path (-v WorkspaceRoot:WorkspaceRoot), so a workspace-scoped
// node_id cache was readable from inside the container and the child adopted the
// host's identity. The fix anchors the cache to $HOME, which is the same anchor
// that already kept BEP certs outside the mount.
//
// Every test drives the REAL resolution path by setting HOME, rather than
// stubbing nodeIDDir — the whole property under test is "which anchor does this
// resolve against", so stubbing the anchor would test nothing.

// useHome points node identity at a throwaway $HOME and restores the production
// resolvers, so the test exercises defaultNodeIdentityDir and bep.ExpandCertDir
// for real.
func useHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv(nodeDirEnvVar, "")
	t.Setenv(nodeIDEnvVar, "")
	prevDir, prevCert := nodeIDDir, nodeIDCertDir
	nodeIDDir = defaultNodeIdentityDir
	nodeIDCertDir = func() string { return bep.ExpandCertDir("") }
	t.Cleanup(func() { nodeIDDir, nodeIDCertDir = prevDir, prevCert })
}

// nodeIdentWorkspace builds a workspace root carrying a legacy workspace-scoped
// node_id, exactly as every pre-existing workspace on disk does today.
func nodeIdentWorkspace(t *testing.T, legacyID string) *Config {
	t.Helper()
	root := t.TempDir()
	cogDir := filepath.Join(root, ".cog")
	runDir := filepath.Join(cogDir, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir workspace run dir: %v", err)
	}
	if legacyID != "" {
		if err := os.WriteFile(filepath.Join(runDir, "node_id"), []byte(legacyID+"\n"), 0o644); err != nil {
			t.Fatalf("seed legacy node_id: %v", err)
		}
	}
	return &Config{WorkspaceRoot: root, CogDir: cogDir}
}

// establishNodeTier makes a $HOME look like a machine that was already running
// CogOS before the node tier existed: ~/.cog/node/ present, and a real BEP cert
// in ~/.cog/etc. Both are the migration gate's probes.
func establishNodeTier(t *testing.T, home string, withCert bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(home, ".cog", "node"), 0o755); err != nil {
		t.Fatalf("mkdir node dir: %v", err)
	}
	if withCert {
		certDir := filepath.Join(home, ".cog", "etc")
		if err := os.MkdirAll(certDir, 0o700); err != nil {
			t.Fatalf("mkdir cert dir: %v", err)
		}
		if err := bep.GenerateBEPCert(certDir); err != nil {
			t.Fatalf("generate cert: %v", err)
		}
	}
}

// snapshotTree fingerprints every file under root by relative path and content.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sum := sha256.Sum256(data)
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return out
}

// The core proof. One workspace root, several $HOMEs — the exact shape of the
// bind mount, where host and child see the identical workspace path but have
// different homes. Distinct homes must yield distinct node ids, and the
// established home must keep the id it already had.
func TestResolveNodeID_TwoHomesOneWorkspace(t *testing.T) {
	const legacy = "3e264177-983d-4c2c-8775-72dd6d6813d7"

	// One workspace on disk, shared by every case below — this stands in for
	// the bind-mounted WorkspaceRoot.
	shared := nodeIdentWorkspace(t, legacy)

	resolve := func(t *testing.T, established bool, overrideNodeDir bool) string {
		t.Helper()
		home := t.TempDir()
		if established {
			establishNodeTier(t, home, true)
		}
		useHome(t, home)
		if overrideNodeDir {
			t.Setenv(nodeDirEnvVar, filepath.Join(home, "custom-node-dir"))
		}
		return resolveNodeID(shared)
	}

	var hostID, childID, overrideID string

	t.Run("established host adopts its existing id", func(t *testing.T) {
		hostID = resolve(t, true, false)
		if hostID != legacy {
			t.Fatalf("established host id = %q, want its pre-existing %q", hostID, legacy)
		}
	})

	t.Run("fresh home mints its own", func(t *testing.T) {
		childID = resolve(t, false, false)
		if childID == legacy {
			t.Fatal("container with a fresh $HOME cloned the host id through the bind mount")
		}
		if _, err := bep.ParseDeviceID(childID); err != nil {
			t.Fatalf("minted id %q is not device-anchored: %v", childID, err)
		}
	})

	t.Run("second fresh home mints a different one", func(t *testing.T) {
		overrideID = resolve(t, false, true)
		if overrideID == legacy {
			t.Fatal("COG_NODE_DIR case cloned the host id")
		}
	})

	// Two homes over one workspace root => two identities.
	if hostID == childID {
		t.Fatalf("two $HOMEs over one workspace resolved the same id %q", hostID)
	}
	if childID == overrideID {
		t.Fatalf("two fresh $HOMEs resolved the same id %q", childID)
	}
	if hostID == overrideID {
		t.Fatalf("established and override homes resolved the same id %q", hostID)
	}
}

// The backward-compatibility guarantee, in the shape of the real host: a $HOME
// with an established node tier and a cert, and a workspace whose node_id is the
// legacy UUID the live kernel reports today. That id must survive verbatim.
func TestResolveNodeID_HostLayoutPreserved(t *testing.T) {
	const legacy = "3e264177-983d-4c2c-8775-72dd6d6813d7"

	home := t.TempDir()
	establishNodeTier(t, home, true)
	useHome(t, home)
	cfg := nodeIdentWorkspace(t, legacy)

	// Precondition: the machine has a cert, so minting WOULD produce a
	// different, device-anchored id. Adoption has to beat it.
	certAnchored := bepAnchoredNodeID()
	if certAnchored == "" {
		t.Fatal("precondition: expected a usable cert in the established home")
	}
	if certAnchored == legacy {
		t.Fatal("precondition: cert-anchored id should differ from the legacy UUID")
	}

	got := resolveNodeID(cfg)
	if got != legacy {
		t.Fatalf("host identity changed: got %q, want preserved %q", got, legacy)
	}

	// Stable on every subsequent boot, and now served from machine-local state.
	if again := resolveNodeID(cfg); again != legacy {
		t.Fatalf("host identity unstable across boots: %q then %q", got, again)
	}
	cached := readNodeIDFile(filepath.Join(home, ".cog", "node", nodeIDFileName))
	if cached != legacy {
		t.Fatalf("machine-local cache = %q, want %q", cached, legacy)
	}

	// And the adoption is observable rather than an invisible coin flip.
	if marker := readNodeIDFile(filepath.Join(home, ".cog", "node", nodeIDSourceFile)); marker == "" {
		t.Fatal("expected a provenance marker recording where the adopted id came from")
	}
}

// The container case must be read-only with respect to the shared workspace.
// The old code called os.MkdirAll + os.WriteFile on <workspace>/.cog/run/node_id,
// so a child kernel with no id in the mount would MINT one into the host's
// workspace — seeding the very file the next child would clone.
func TestResolveNodeID_NeverWritesIntoWorkspace(t *testing.T) {
	const legacy = "3e264177-983d-4c2c-8775-72dd6d6813d7"

	for _, tc := range []struct {
		name   string
		legacy string
	}{
		{"workspace has a legacy id", legacy},
		{"workspace has no id at all", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useHome(t, t.TempDir()) // fresh $HOME == container
			cfg := nodeIdentWorkspace(t, tc.legacy)

			before := snapshotTree(t, cfg.WorkspaceRoot)
			got := resolveNodeID(cfg)
			after := snapshotTree(t, cfg.WorkspaceRoot)

			if !reflect.DeepEqual(before, after) {
				t.Fatalf("workspace was modified.\nbefore: %v\nafter:  %v", before, after)
			}
			if _, exists := after[filepath.Join(".cog", "run", "node_id")]; exists != (tc.legacy != "") {
				t.Fatalf("node_id presence in workspace changed unexpectedly: %v", after)
			}
			if tc.legacy != "" && got == tc.legacy {
				t.Fatal("child adopted the workspace id instead of minting its own")
			}
		})
	}
}

// Migration copies; it never moves or deletes. The workspace file must still be
// byte-identical afterwards, so the change is trivially rollback-able.
func TestResolveNodeID_MigrationIsCopyNotMove(t *testing.T) {
	const legacy = "3e264177-983d-4c2c-8775-72dd6d6813d7"

	home := t.TempDir()
	establishNodeTier(t, home, true)
	useHome(t, home)
	cfg := nodeIdentWorkspace(t, legacy)

	legacyPath := filepath.Join(cfg.CogDir, "run", "node_id")
	before, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatalf("read legacy node_id: %v", err)
	}

	if got := resolveNodeID(cfg); got != legacy {
		t.Fatalf("adopted %q, want %q", got, legacy)
	}

	after, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatalf("legacy node_id disappeared after migration: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("legacy node_id was rewritten: %q -> %q", before, after)
	}
}

// The gate decides whether adopting a workspace id is legitimate, and it must
// answer using $HOME-anchored evidence only. Inside a container the workspace is
// mounted and both legacy artifacts are readable — the gate has to say no anyway.
func TestMigrationGateUsesHomeAnchoredProbesOnly(t *testing.T) {
	const legacy = "3e264177-983d-4c2c-8775-72dd6d6813d7"

	for _, tc := range []struct {
		name      string
		nodeDir   bool
		cert      bool
		wantAdopt bool
	}{
		{"node dir and cert (real host)", true, true, true},
		{"node dir only", true, false, true},
		{"cert only", false, true, true},
		{"neither (container)", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			if tc.nodeDir {
				if err := os.MkdirAll(filepath.Join(home, ".cog", "node"), 0o755); err != nil {
					t.Fatalf("mkdir node dir: %v", err)
				}
			}
			if tc.cert {
				certDir := filepath.Join(home, ".cog", "etc")
				if err := os.MkdirAll(certDir, 0o700); err != nil {
					t.Fatalf("mkdir cert dir: %v", err)
				}
				if err := bep.GenerateBEPCert(certDir); err != nil {
					t.Fatalf("generate cert: %v", err)
				}
			}
			useHome(t, home)

			got := resolveNodeID(nodeIdentWorkspace(t, legacy))
			if adopted := got == legacy; adopted != tc.wantAdopt {
				t.Fatalf("adopted=%v (id %q), want adopted=%v", adopted, got, tc.wantAdopt)
			}
		})
	}
}

// Adoption must not depend on which workspace's kernel happens to boot first.
// This machine really does carry three workspaces with three different legacy
// ids, so "first boot seals the machine identity" would be a coin flip that
// could silently re-identify the live kernel.
func TestResolveNodeID_AdoptionSourceIsDeterministic(t *testing.T) {
	const registryID = "3e264177-983d-4c2c-8775-72dd6d6813d7"
	const otherID = "98daf3e5-4d9d-429d-9b0c-bf382c164225"

	// The workspace the node registry names as current, and an unrelated one.
	registryWS := nodeIdentWorkspace(t, registryID)
	otherWS := nodeIdentWorkspace(t, otherID)

	// The real host's registry records the workspace by its legacy alias path
	// (~/cog-workspace), which is a SYMLINK to the canonical location. Cover
	// that shape explicitly rather than only the plain-directory one.
	linked := filepath.Join(t.TempDir(), "cog-workspace-alias")
	if err := os.Symlink(registryWS.WorkspaceRoot, linked); err != nil {
		t.Fatalf("symlink workspace alias: %v", err)
	}

	for _, tc := range []struct {
		name         string
		boot         *Config
		registryPath string
	}{
		{"boots the registry's current workspace", registryWS, registryWS.WorkspaceRoot},
		{"boots some other workspace", otherWS, registryWS.WorkspaceRoot},
		{"registry path is a symlink, as on the real host", otherWS, linked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			establishNodeTier(t, home, true)
			registry := "version: \"1.0\"\n" +
				"current-workspace: cog-workspace\n" +
				"workspaces:\n" +
				"    cog-workspace:\n" +
				"        path: " + tc.registryPath + "\n" +
				"        name: cog-workspace\n"
			if err := os.WriteFile(filepath.Join(home, ".cog", "node", nodeRegistryFile), []byte(registry), 0o644); err != nil {
				t.Fatalf("seed registry: %v", err)
			}
			useHome(t, home)

			if got := resolveNodeID(tc.boot); got != registryID {
				t.Fatalf("resolved %q, want the registry's current-workspace id %q "+
					"(machine identity must not depend on boot order)", got, registryID)
			}
		})
	}
}

// COG_NODE_ID is the operator escape hatch and the clean way to provision a
// child. COGOS_NODE_ID is a DIFFERENT, pre-existing variable owned by the
// presence hooks, where it holds `$(hostname -s)` — honoring it here would let
// exporting COGOS_NODE_ID=darkstar silently rewrite SourceIdentity on every
// emitted block to the string "darkstar".
func TestResolveNodeID_EnvPin(t *testing.T) {
	const legacy = "3e264177-983d-4c2c-8775-72dd6d6813d7"
	pin := uuid.NewString()

	t.Run("valid pin wins over every on-disk source", func(t *testing.T) {
		home := t.TempDir()
		establishNodeTier(t, home, true)
		useHome(t, home)
		t.Setenv(nodeIDEnvVar, pin)

		if got := resolveNodeID(nodeIdentWorkspace(t, legacy)); got != pin {
			t.Fatalf("resolved %q, want pinned %q", got, pin)
		}
	})

	t.Run("malformed pin is rejected, not adopted", func(t *testing.T) {
		home := t.TempDir()
		establishNodeTier(t, home, true)
		useHome(t, home)
		t.Setenv(nodeIDEnvVar, "darkstar")

		got := resolveNodeID(nodeIdentWorkspace(t, legacy))
		if got == "darkstar" {
			t.Fatal("a malformed COG_NODE_ID became the node identity")
		}
		if got != legacy {
			t.Fatalf("resolved %q, want fall-through to %q", got, legacy)
		}
	})

	t.Run("COGOS_NODE_ID is not honored", func(t *testing.T) {
		home := t.TempDir()
		establishNodeTier(t, home, true)
		useHome(t, home)
		t.Setenv("COGOS_NODE_ID", "darkstar")

		got := resolveNodeID(nodeIdentWorkspace(t, legacy))
		if got == "darkstar" {
			t.Fatal("the hooks' COGOS_NODE_ID (a hostname) was used as the kernel node id")
		}
		if got != legacy {
			t.Fatalf("resolved %q, want %q", got, legacy)
		}
	})
}

// defaultNodeIdentityDir must never hand back a relative path. In both real
// deployment shapes cwd IS the workspace root (the launchd plist sets
// WorkingDirectory to it; the container runs with -w WorkspaceRoot), so a "."
// fallback would write the minted id back into the shared workspace and
// recreate the clone one level up.
func TestDefaultNodeIdentityDir_NeverRelative(t *testing.T) {
	t.Run("rejects a relative COG_NODE_DIR", func(t *testing.T) {
		t.Setenv(nodeDirEnvVar, "relative/node")
		if dir, err := defaultNodeIdentityDir(); err == nil {
			t.Fatalf("accepted relative COG_NODE_DIR, returned %q", dir)
		}
	})

	t.Run("errors rather than falling back when HOME is unset", func(t *testing.T) {
		t.Setenv(nodeDirEnvVar, "")
		t.Setenv("HOME", "")
		dir, err := defaultNodeIdentityDir()
		if err == nil {
			t.Fatalf("expected an error with no HOME, got dir %q", dir)
		}
		if dir != "" && !filepath.IsAbs(dir) {
			t.Fatalf("returned a relative path %q", dir)
		}
	})
}

// When the machine anchor is unavailable the kernel degrades to an ephemeral id
// for that boot. It must NOT fall back to the workspace: an unstable id is
// merely inconvenient, whereas a workspace-scoped one is another machine's.
func TestResolveNodeID_NoHomeDegradesEphemerallyNotToWorkspace(t *testing.T) {
	const legacy = "3e264177-983d-4c2c-8775-72dd6d6813d7"

	certDir := useTempCertDir(t)
	if err := bep.GenerateBEPCert(certDir); err != nil {
		t.Fatalf("generate cert: %v", err)
	}
	prev := nodeIDDir
	nodeIDDir = defaultNodeIdentityDir
	t.Cleanup(func() { nodeIDDir = prev })
	t.Setenv(nodeDirEnvVar, "")
	t.Setenv(nodeIDEnvVar, "")
	t.Setenv("HOME", "")

	cfg := nodeIdentWorkspace(t, legacy)
	before := snapshotTree(t, cfg.WorkspaceRoot)

	got := resolveNodeID(cfg)
	if got == "" {
		t.Fatal("expected an ephemeral id, got empty")
	}
	if got == legacy {
		t.Fatal("degraded to the workspace-scoped id instead of an ephemeral one")
	}
	if after := snapshotTree(t, cfg.WorkspaceRoot); !reflect.DeepEqual(before, after) {
		t.Fatalf("workspace written to on the degraded path: %v -> %v", before, after)
	}
}
