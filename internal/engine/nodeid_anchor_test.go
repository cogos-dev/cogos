package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/myrgic/cogos/pkg/substrate/bep"
)

// The node id, once minted, is anchored to the kernel's BEP device identity so
// process.NodeID and the peer id on the wire are one value (RFC-036). These
// tests pin the behaviors that make that safe: anchor-on-mint, mint-the-cert-
// when-absent (the dark-by-default ordering case), never-rewrite-an-existing-id,
// and clean fallback when the identity cannot be established.
//
// Every test is hermetic: nodeIDCertDir is redirected to a t.TempDir() so no
// test ever reads or writes the real ~/.cog/etc.

func writeNodeIDCfg(t *testing.T) *Config {
	t.Helper()
	return &Config{CogDir: t.TempDir()}
}

// useTempCertDir points node-id anchoring at an isolated cert dir for the
// duration of the test and returns that dir.
func useTempCertDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	prev := nodeIDCertDir
	nodeIDCertDir = func() string { return dir }
	t.Cleanup(func() { nodeIDCertDir = prev })
	return dir
}

// A freshly-minted node id equals the formatted BEP DeviceID when a cert is
// already on disk in the cert dir.
func TestNodeID_AnchoredToDeviceIDOnMint(t *testing.T) {
	certDir := useTempCertDir(t)
	if err := bep.GenerateBEPCert(certDir); err != nil {
		t.Fatalf("generate cert: %v", err)
	}

	want := bepAnchoredNodeID()
	if want == "" {
		t.Fatal("bepAnchoredNodeID returned empty with a valid cert on disk")
	}

	cfg := writeNodeIDCfg(t)
	got := loadOrCreateNodeID(cfg)
	if got != want {
		t.Fatalf("minted node id = %q, want device-anchored %q", got, want)
	}
	if _, err := bep.ParseDeviceID(got); err != nil {
		t.Fatalf("minted node id %q is not a parseable device id: %v", got, err)
	}
	// Persisted, and identical on a second load.
	if again := loadOrCreateNodeID(cfg); again != got {
		t.Fatalf("node id not stable across loads: %q then %q", got, again)
	}
}

// The ordering case the review flagged: on a node's first boot with
// cluster.enabled=false there is no cert yet, because `bep-cert gen` is a
// manual step Boot never runs. Minting must still produce a device-anchored id
// (creating the keypair), not a UUID — otherwise anchoring would never activate
// for any node that opts into clustering later, since ids are never rewritten.
func TestNodeID_MintsDeviceIdentityWhenNoCertExists(t *testing.T) {
	certDir := useTempCertDir(t)
	if fileExists(filepath.Join(certDir, "bep-cert.pem")) {
		t.Fatal("precondition: cert dir should start empty")
	}

	got := loadOrCreateNodeID(writeNodeIDCfg(t))

	if !fileExists(filepath.Join(certDir, "bep-cert.pem")) {
		t.Fatal("expected a BEP cert to be minted when none existed")
	}
	if !fileExists(filepath.Join(certDir, "bep-key.pem")) {
		t.Fatal("expected a BEP key alongside the minted cert")
	}
	if _, err := bep.ParseDeviceID(got); err != nil {
		t.Fatalf("node id %q is not device-anchored (expected minted DeviceID): %v", got, err)
	}
	// And it matches the identity now on disk.
	if want := bepAnchoredNodeID(); got != want {
		t.Fatalf("node id %q != device id derived from minted cert %q", got, want)
	}
}

// An already-persisted node id is authoritative and never rewritten, even to
// the (different) device-anchored value.
func TestNodeID_ExistingNeverRewritten(t *testing.T) {
	certDir := useTempCertDir(t)
	if err := bep.GenerateBEPCert(certDir); err != nil {
		t.Fatalf("generate cert: %v", err)
	}

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

// When the device identity cannot be established at all, minting degrades to a
// UUID rather than failing the boot.
func TestNodeID_FallsBackToUUIDWhenIdentityUnavailable(t *testing.T) {
	// An unwritable, non-existent cert dir: neither load nor mint can succeed.
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("not-a-dir\n"), 0o600); err != nil {
		t.Fatalf("seed blocker: %v", err)
	}
	prev := nodeIDCertDir
	nodeIDCertDir = func() string { return filepath.Join(blocked, "etc") }
	t.Cleanup(func() { nodeIDCertDir = prev })

	got := loadOrCreateNodeID(writeNodeIDCfg(t))
	if got == "" {
		t.Fatal("expected a UUID fallback, got empty node id")
	}
	if _, err := bep.ParseDeviceID(got); err == nil {
		t.Fatalf("expected UUID fallback, got a device-anchored id %q", got)
	}
}

// bepAnchoredNodeID must never panic and must return "" (triggering the mint /
// UUID path) when the cert dir has no cert.
func TestBepAnchoredNodeID_EmptyWithoutCert(t *testing.T) {
	useTempCertDir(t)
	if id := bepAnchoredNodeID(); id != "" {
		t.Fatalf("expected \"\" with no cert on disk, got %q", id)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
