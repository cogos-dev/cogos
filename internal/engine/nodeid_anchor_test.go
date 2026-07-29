package engine

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// The confirmed cog-review finding on this PR: a cert-without-key directory
// (from a crash mid-generation, or two processes racing on first boot before
// this fix) must not be treated as "identity established". Recovery must be
// loud (a Warn log, reclaiming the orphan) and must actually produce a
// working device-anchored id on this boot, not silently and permanently fall
// back to a UUID.
func TestNodeID_RecoversFromCertWithoutKey(t *testing.T) {
	certDir := useTempCertDir(t)

	// Seed the exact broken state the review flagged: a cert with no
	// matching key, as GenerateBEPCert could previously leave behind if it
	// died after writing the cert but before the key.
	if err := os.WriteFile(filepath.Join(certDir, "bep-cert.pem"), []byte("not a real cert\n"), 0o644); err != nil {
		t.Fatalf("seed orphaned cert: %v", err)
	}
	if fileExists(filepath.Join(certDir, "bep-key.pem")) {
		t.Fatal("precondition: no key should exist yet")
	}

	got := loadOrCreateNodeID(writeNodeIDCfg(t))

	if !fileExists(filepath.Join(certDir, "bep-cert.pem")) {
		t.Fatal("expected a regenerated cert after recovery")
	}
	if !fileExists(filepath.Join(certDir, "bep-key.pem")) {
		t.Fatal("expected a regenerated key after recovery; orphaned cert-without-key must not be reported as success")
	}
	if _, err := bep.LoadBEPCert(certDir); err != nil {
		t.Fatalf("regenerated identity is not a usable TLS keypair: %v", err)
	}
	if _, err := bep.ParseDeviceID(got); err != nil {
		t.Fatalf("node id %q is not device-anchored after recovery (silent UUID fallback): %v", got, err)
	}
	if want := bepAnchoredNodeID(); got != want {
		t.Fatalf("node id %q != device id derived from recovered cert %q", got, want)
	}
}

// Symmetric partial state: a key with no matching cert must be reclaimed the
// same way, rather than assumed impossible because of write ordering.
func TestNodeID_RecoversFromKeyWithoutCert(t *testing.T) {
	certDir := useTempCertDir(t)

	if err := os.WriteFile(filepath.Join(certDir, "bep-key.pem"), []byte("not a real key\n"), 0o600); err != nil {
		t.Fatalf("seed orphaned key: %v", err)
	}
	if fileExists(filepath.Join(certDir, "bep-cert.pem")) {
		t.Fatal("precondition: no cert should exist yet")
	}

	got := loadOrCreateNodeID(writeNodeIDCfg(t))

	if !fileExists(filepath.Join(certDir, "bep-cert.pem")) || !fileExists(filepath.Join(certDir, "bep-key.pem")) {
		t.Fatal("expected a full regenerated pair after recovery")
	}
	if _, err := bep.ParseDeviceID(got); err != nil {
		t.Fatalf("node id %q is not device-anchored after recovery: %v", got, err)
	}
}

// Round-2 cog-review finding on this PR: os.Stat-only presence checks are
// not enough. A cert and key can both exist and both parse as valid PEM
// individually while not forming a matching pair — a restored backup that
// mixes files from two different nodes, or disk corruption. That must be
// treated exactly like the cert-without-key orphan case above: reclaimed
// loudly (a Warn log, broken files backed up aside rather than silently
// dropped) and regenerated into a working device-anchored id, never a
// silent permanent UUID fallback.
func TestNodeID_RecoversFromCorruptOrMismatchedPair(t *testing.T) {
	certDir := useTempCertDir(t)

	// Seed a mismatched-but-well-formed pair: a genuine cert from one
	// identity paired with a genuine key from a different one. Both files
	// individually parse as valid PEM/DER; only cross-checking that the key
	// matches the cert (what tls.LoadX509KeyPair, and therefore
	// bep.LoadBEPCert, does) reveals the break.
	dirA, dirB := t.TempDir(), t.TempDir()
	if err := bep.GenerateBEPCert(dirA); err != nil {
		t.Fatalf("generate pair A: %v", err)
	}
	if err := bep.GenerateBEPCert(dirB); err != nil {
		t.Fatalf("generate pair B: %v", err)
	}
	certBytes, err := os.ReadFile(filepath.Join(dirA, "bep-cert.pem"))
	if err != nil {
		t.Fatalf("read pair A cert: %v", err)
	}
	keyBytes, err := os.ReadFile(filepath.Join(dirB, "bep-key.pem"))
	if err != nil {
		t.Fatalf("read pair B key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "bep-cert.pem"), certBytes, 0o644); err != nil {
		t.Fatalf("seed mismatched cert: %v", err)
	}
	if err := os.WriteFile(filepath.Join(certDir, "bep-key.pem"), keyBytes, 0o600); err != nil {
		t.Fatalf("seed mismatched key: %v", err)
	}

	// Precondition: both files exist (os.Stat would wrongly call this done)
	// but do not form a usable identity.
	if !fileExists(filepath.Join(certDir, "bep-cert.pem")) || !fileExists(filepath.Join(certDir, "bep-key.pem")) {
		t.Fatal("precondition: both files should exist")
	}
	if id := bepAnchoredNodeID(); id != "" {
		t.Fatalf("precondition: mismatched pair should not load, got id %q", id)
	}

	// Capture logs: the recovery must be loud (Warn), not the silent
	// fallback path (which only logs at Debug, and only on a returned
	// error -- this recovery must succeed and must still have logged).
	var logBuf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	got := loadOrCreateNodeID(writeNodeIDCfg(t))

	slog.SetDefault(prevLogger)
	logOut := logBuf.String()
	if !strings.Contains(logOut, "level=WARN") {
		t.Fatalf("expected a loud (Warn-level) log for the corrupt/mismatched pair, got:\n%s", logOut)
	}
	if !strings.Contains(logOut, "do not load as a valid matching identity") {
		t.Fatalf("expected the corrupt-pair recovery log message, got:\n%s", logOut)
	}

	// The broken files must be backed up aside, not silently dropped.
	entries, err := os.ReadDir(certDir)
	if err != nil {
		t.Fatalf("read cert dir: %v", err)
	}
	brokenFound := false
	for _, e := range entries {
		if strings.Contains(e.Name(), ".broken-") {
			brokenFound = true
			break
		}
	}
	if !brokenFound {
		t.Fatalf("expected the broken cert/key to be backed up (a .broken-* file), found: %v", entries)
	}

	// And recovery must actually produce a working device-anchored id, not
	// a silent, permanent UUID fallback.
	if _, err := bep.LoadBEPCert(certDir); err != nil {
		t.Fatalf("regenerated identity is not a usable TLS keypair: %v", err)
	}
	if _, err := bep.ParseDeviceID(got); err != nil {
		t.Fatalf("node id %q is not device-anchored after recovery (silent UUID fallback): %v", got, err)
	}
	if want := bepAnchoredNodeID(); got != want {
		t.Fatalf("node id %q != device id derived from recovered cert %q", got, want)
	}
}

// ensureBEPDeviceIdentity is the automatic, unattended entrypoint that fires
// from every NewProcess call on a node with no persisted node_id yet — unlike
// the manual `bep-cert gen` CLI, several independent boot paths can invoke it
// concurrently against the same cert dir. It must be single-flighted (via
// pkg/filelock, matching the pattern used elsewhere in this codebase) so a
// concurrent race can never produce anything other than exactly one valid
// cert/key pair.
func TestEnsureBEPDeviceIdentity_ConcurrentCallsAreSingleFlighted(t *testing.T) {
	certDir := useTempCertDir(t)

	const n = 8
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- ensureBEPDeviceIdentity()
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Errorf("ensureBEPDeviceIdentity: %v", err)
		}
	}

	if !fileExists(filepath.Join(certDir, "bep-cert.pem")) || !fileExists(filepath.Join(certDir, "bep-key.pem")) {
		t.Fatal("expected exactly one cert/key pair to exist after concurrent generation")
	}
	if _, err := bep.LoadBEPCert(certDir); err != nil {
		t.Fatalf("resulting identity is not a usable TLS keypair: %v", err)
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
