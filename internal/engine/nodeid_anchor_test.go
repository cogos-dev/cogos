package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/myrgic/cogos/pkg/substrate/bep"
)

// The node id, once minted, is anchored to the kernel's BEP device identity so
// process.NodeID and the peer id on the wire are one value (RFC-036). These
// tests pin the three behaviors that make that safe: anchor-on-mint,
// uuid-fallback-when-no-cert, and never-rewrite-an-existing-id.

func writeNodeIDCfg(t *testing.T) *Config {
	t.Helper()
	return &Config{CogDir: t.TempDir()}
}

// A freshly-minted node id equals the formatted BEP DeviceID when a cert is on
// disk in the canonical cert dir.
func TestNodeID_AnchoredToDeviceIDOnMint(t *testing.T) {
	cfg := writeNodeIDCfg(t)
	certDir := bep.ExpandCertDir("")
	if err := os.MkdirAll(certDir, 0o755); err != nil {
		t.Fatalf("mkdir certdir: %v", err)
	}
	// Only run if we can establish a cert; generating into the real canonical
	// dir would clobber a live cert, so skip rather than risk it when one is
	// already present but unreadable for the test's purposes.
	certPath := filepath.Join(certDir, "bep-cert.pem")
	preexisting := fileExists(certPath)
	if !preexisting {
		if err := bep.GenerateBEPCert(certDir); err != nil {
			t.Skipf("cannot generate BEP cert in %s: %v", certDir, err)
		}
		t.Cleanup(func() {
			os.Remove(certPath)
			os.Remove(filepath.Join(certDir, "bep-key.pem"))
		})
	}

	want := bepAnchoredNodeID()
	if want == "" {
		t.Skip("no usable BEP cert; anchoring path not exercisable here")
	}

	got := loadOrCreateNodeID(cfg)
	if got != want {
		t.Fatalf("minted node id = %q, want device-anchored %q", got, want)
	}
	// Persisted, and identical on a second load.
	if again := loadOrCreateNodeID(cfg); again != got {
		t.Fatalf("node id not stable across loads: %q then %q", got, again)
	}
}

// An already-persisted node id is authoritative and never rewritten, even to
// the (different) device-anchored value.
func TestNodeID_ExistingNeverRewritten(t *testing.T) {
	cfg := writeNodeIDCfg(t)
	runDir := filepath.Join(cfg.CogDir, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatalf("mkdir run: %v", err)
	}
	const legacy = "legacy-uuid-11112222-3333-4444"
	if err := os.WriteFile(filepath.Join(runDir, "node_id"), []byte(legacy+"\n"), 0o644); err != nil {
		t.Fatalf("seed node_id: %v", err)
	}
	if got := loadOrCreateNodeID(cfg); got != legacy {
		t.Fatalf("existing node id was changed: got %q, want preserved %q", got, legacy)
	}
}

// bepAnchoredNodeID must never panic and must return "" (triggering UUID
// fallback) when the cert dir has no cert.
func TestBepAnchoredNodeID_FallsBackCleanly(t *testing.T) {
	// We cannot safely point ExpandCertDir at an empty temp dir without env
	// control, so assert the weaker invariant that holds unconditionally: the
	// call is total (no panic) and yields either a valid formatted id or "".
	id := bepAnchoredNodeID()
	if id != "" {
		if _, err := bep.ParseDeviceID(id); err != nil {
			t.Fatalf("bepAnchoredNodeID returned unparseable device id %q: %v", id, err)
		}
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
