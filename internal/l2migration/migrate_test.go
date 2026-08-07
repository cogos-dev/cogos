package l2migration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/cogblock"
	"github.com/myrgic/constellation"
	"gopkg.in/yaml.v3"
)

// writeFixtureLegacyIdentity writes a throwaway Layer-2 identity.json into a
// fresh temp dir, mirroring cog/.cog/identity.json's real shape but with
// entirely synthetic values -- no real key material anywhere in this file.
func writeFixtureLegacyIdentity(t *testing.T, workspaceRoot, nodeHash string) {
	t.Helper()
	cogDir := filepath.Join(workspaceRoot, ".cog")
	if err := os.MkdirAll(cogDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", cogDir, err)
	}
	body := `{
  "node_hash": "` + nodeHash + `",
  "public_key": "ed25519:ZmFrZS1wdWJsaWMta2V5LWJ5dGVzLWZvci10ZXN0",
  "role": "workspace",
  "genesis_timestamp": "2026-03-05T00:00:00Z",
  "eigenvalue": "sha256:deadbeef",
  "parent_hash": null
}
`
	path := filepath.Join(cogDir, "identity.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture identity: %v", err)
	}
}

func TestLoadLegacyIdentity(t *testing.T) {
	root := t.TempDir()
	writeFixtureLegacyIdentity(t, root, "sha256:fixturehash01")

	got, err := LoadLegacyIdentity(root)
	if err != nil {
		t.Fatalf("LoadLegacyIdentity: %v", err)
	}
	if got.NodeHash != "sha256:fixturehash01" {
		t.Errorf("NodeHash = %q, want sha256:fixturehash01", got.NodeHash)
	}
	if got.Role != "workspace" {
		t.Errorf("Role = %q, want workspace", got.Role)
	}
	if got.ParentHash != nil {
		t.Errorf("ParentHash = %v, want nil", got.ParentHash)
	}
}

func TestLoadLegacyIdentity_Missing(t *testing.T) {
	root := t.TempDir()
	if _, err := LoadLegacyIdentity(root); err == nil {
		t.Fatal("LoadLegacyIdentity: expected error for missing identity.json, got nil")
	}
}

func TestLoadLegacyIdentity_MissingNodeHash(t *testing.T) {
	root := t.TempDir()
	cogDir := filepath.Join(root, ".cog")
	if err := os.MkdirAll(cogDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cogDir, "identity.json"), []byte(`{"role":"workspace"}`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadLegacyIdentity(root); err == nil {
		t.Fatal("LoadLegacyIdentity: expected error for missing node_hash, got nil")
	}
}

func TestEnsureL1Identity_GeneratesWhenAbsent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")

	id, err := EnsureL1Identity(dir)
	if err != nil {
		t.Fatalf("EnsureL1Identity: %v", err)
	}
	if id.NodeID == "" {
		t.Fatal("EnsureL1Identity: empty NodeID")
	}
	keyPath := filepath.Join(dir, "node-key.pem")
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("expected key file at %s: %v", keyPath, err)
	}
}

func TestEnsureL1Identity_LoadsExisting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")

	first, err := EnsureL1Identity(dir)
	if err != nil {
		t.Fatalf("EnsureL1Identity (first call): %v", err)
	}

	second, err := EnsureL1Identity(dir)
	if err != nil {
		t.Fatalf("EnsureL1Identity (second call): %v", err)
	}

	if first.NodeID != second.NodeID {
		t.Errorf("NodeID changed across calls: %q != %q -- EnsureL1Identity should load, not regenerate", first.NodeID, second.NodeID)
	}
}

func TestEnsureL1Identity_NodeIDMatchesDerivation(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")

	id, err := EnsureL1Identity(dir)
	if err != nil {
		t.Fatalf("EnsureL1Identity: %v", err)
	}

	der, err := id.MarshalPublicKey()
	if err != nil {
		t.Fatalf("MarshalPublicKey: %v", err)
	}

	// Independently re-derive NodeID = hex(sha256(DER(pubkey))) -- ADR-099
	// step 3's exact formula -- and compare against id.NodeID. This is the
	// assertion the test name promises; a prior version of this test only
	// round-tripped the DER through x509 and never computed a hash, so it
	// would have passed even if constellation's derivation formula changed
	// underneath it.
	sum := sha256.Sum256(der)
	wantNodeID := hex.EncodeToString(sum[:])
	if id.NodeID != wantNodeID {
		t.Errorf("NodeID = %q, want hex(sha256(DER(pubkey))) = %q", id.NodeID, wantNodeID)
	}

	// Also confirm the DER round-trips to the same public key, as a sanity
	// check on MarshalPublicKey itself.
	pub, err := constellation.PublicKeyFromDER(der)
	if err != nil {
		t.Fatalf("PublicKeyFromDER: %v", err)
	}
	if pub.X.Cmp(id.PublicKey.X) != 0 || pub.Y.Cmp(id.PublicKey.Y) != 0 {
		t.Fatal("round-tripped public key does not match original")
	}
}

// TestEnsureL1Identity_ExistingUnloadableKeyIsNotDestroyed is the regression
// test for the destroy-on-any-load-error bug: EnsureL1Identity must not fall
// through to GenerateIdentity+SaveIdentity (which truncates node-key.pem)
// just because LoadIdentity failed. It must distinguish "absent" (generate)
// from "present but broken" (hard error, key left untouched).
func TestEnsureL1Identity_ExistingUnloadableKeyIsNotDestroyed(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	keyPath := filepath.Join(dir, "node-key.pem")

	// Simulate a crash mid-write: a partial, non-PEM file sitting at the
	// exact path constellation.LoadIdentity/SaveIdentity use. No real key
	// material anywhere in this fixture.
	corrupt := []byte("-----BEGIN EC PRIVATE KEY-----\ntruncated-not-real-pem-data")
	if err := os.WriteFile(keyPath, corrupt, 0o600); err != nil {
		t.Fatalf("write corrupt fixture key: %v", err)
	}

	_, err := EnsureL1Identity(dir)
	if err == nil {
		t.Fatal("EnsureL1Identity: expected an error for an existing-but-unloadable key, got nil")
	}

	// The load-bearing assertion: the broken file must be untouched, not
	// silently truncated and replaced by a freshly generated key.
	got, readErr := os.ReadFile(keyPath)
	if readErr != nil {
		t.Fatalf("key file vanished after failed EnsureL1Identity: %v", readErr)
	}
	if string(got) != string(corrupt) {
		t.Errorf("existing key file was modified; EnsureL1Identity must never overwrite a present-but-unloadable key.\n got: %q\nwant: %q", got, corrupt)
	}
}

func TestWriteNodeCRD(t *testing.T) {
	root := t.TempDir()
	ts := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	path, err := WriteNodeCRD(root, "sha256:oldhash", "newhexid", ts)
	if err != nil {
		t.Fatalf("WriteNodeCRD: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written CRD: %v", err)
	}

	var crd NodeCRD
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("unmarshal written CRD: %v", err)
	}
	if crd.Kind != "Node" {
		t.Errorf("Kind = %q, want Node", crd.Kind)
	}
	if crd.APIVersion != NodeCRDAPIVersion {
		t.Errorf("APIVersion = %q, want %q", crd.APIVersion, NodeCRDAPIVersion)
	}
	if crd.Spec.OldNodeHash != "sha256:oldhash" {
		t.Errorf("Spec.OldNodeHash = %q, want sha256:oldhash", crd.Spec.OldNodeHash)
	}
	if crd.Spec.NewNodeID != "newhexid" {
		t.Errorf("Spec.NewNodeID = %q, want newhexid", crd.Spec.NewNodeID)
	}
	if crd.Spec.MigrationTS != "2026-08-07T12:00:00Z" {
		t.Errorf("Spec.MigrationTS = %q, want 2026-08-07T12:00:00Z", crd.Spec.MigrationTS)
	}

	wantPath := filepath.Join(root, ".cog", "config", "nodes", "sha256%3Aoldhash.yaml")
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}
}

// TestWriteNodeCRD_PathTraversal is the regression test for treating
// oldNodeHash as trusted input: it is read verbatim from a file
// (LoadLegacyIdentity only checks non-empty), so a traversal payload there
// must not escape .cog/config/nodes/.
func TestWriteNodeCRD_PathTraversal(t *testing.T) {
	root := t.TempDir()
	ts := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

	path, err := WriteNodeCRD(root, "../../../../etc/pwned", "newhexid", ts)
	if err != nil {
		t.Fatalf("WriteNodeCRD: %v", err)
	}

	// The sanitized name may legitimately CONTAIN literal ".." characters
	// (SanitizeComponent percent-escapes the "/" separators but leaves "."
	// alone, so "../.." becomes the single safe component "..%2F.."). What
	// must never happen is the result resolving to a DIFFERENT directory
	// than nodesDir -- i.e. it must be exactly one path component below it.
	nodesDir := filepath.Join(root, ".cog", "config", "nodes")
	if gotDir := filepath.Dir(path); gotDir != nodesDir {
		t.Errorf("WriteNodeCRD escaped %s: wrote to directory %s instead", nodesDir, gotDir)
	}
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("expected CRD written inside nodesDir, stat failed: %v", statErr)
	}
}

func TestEmitMigratedEvent(t *testing.T) {
	root := t.TempDir()

	if err := EmitMigratedEvent(root, "test-session", "sha256:oldhash", "newhexid"); err != nil {
		t.Fatalf("EmitMigratedEvent: %v", err)
	}

	eventsPath := filepath.Join(root, ".cog", "ledger", "test-session", "events.jsonl")
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read ledger events: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, `"type":"node.identity.migrated"`) {
		t.Errorf("event missing expected type field; got %s", got)
	}
	if !strings.Contains(got, `"old_node_hash":"sha256:oldhash"`) {
		t.Errorf("event missing old_node_hash; got %s", got)
	}
	if !strings.Contains(got, `"new_node_id":"newhexid"`) {
		t.Errorf("event missing new_node_id; got %s", got)
	}
}

func TestMakeLegacyIdentityReadOnly(t *testing.T) {
	root := t.TempDir()
	writeFixtureLegacyIdentity(t, root, "sha256:fixturehash02")

	if err := MakeLegacyIdentityReadOnly(root); err != nil {
		t.Fatalf("MakeLegacyIdentityReadOnly: %v", err)
	}

	path := legacyIdentityPath(root)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Errorf("mode = %v, want no write bits set", info.Mode().Perm())
	}

	// The file must still be there with its content intact -- "preserve,
	// never delete" is the whole point of step 6.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("identity.json unreadable after chmod: %v", err)
	}
	if !strings.Contains(string(data), "sha256:fixturehash02") {
		t.Errorf("identity.json content changed; got %s", data)
	}
}

func TestMigrate_EndToEnd(t *testing.T) {
	root := t.TempDir()
	identityDir := filepath.Join(t.TempDir(), "l1-identity")
	writeFixtureLegacyIdentity(t, root, "sha256:e2efixture")

	result, err := Migrate(root, identityDir, "identity-migration")
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if result.OldNodeHash != "sha256:e2efixture" {
		t.Errorf("OldNodeHash = %q, want sha256:e2efixture", result.OldNodeHash)
	}
	if result.NewNodeID == "" {
		t.Error("NewNodeID is empty")
	}
	if len(result.Warnings) == 0 {
		t.Error("Warnings is empty; the L1-identity-scope gap must be surfaced on every run")
	}

	// Step 4: CRD YAML exists and round-trips.
	crdData, err := os.ReadFile(result.CRDPath)
	if err != nil {
		t.Fatalf("read CRD: %v", err)
	}
	var crd NodeCRD
	if err := yaml.Unmarshal(crdData, &crd); err != nil {
		t.Fatalf("unmarshal CRD: %v", err)
	}
	if crd.Spec.NewNodeID != result.NewNodeID {
		t.Errorf("CRD new_node_id = %q, want %q", crd.Spec.NewNodeID, result.NewNodeID)
	}

	// Step 5: ledger event exists.
	eventsPath := filepath.Join(root, ".cog", "ledger", "identity-migration", "events.jsonl")
	if _, err := os.Stat(eventsPath); err != nil {
		t.Fatalf("expected ledger events file: %v", err)
	}

	// Step 6: legacy identity is still present and now read-only.
	legacyPath := legacyIdentityPath(root)
	info, err := os.Stat(legacyPath)
	if err != nil {
		t.Fatalf("legacy identity missing after migration: %v", err)
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Errorf("legacy identity mode = %v, want read-only", info.Mode().Perm())
	}

	// Re-running against the same identityDir must yield the same NewNodeID
	// (load, not regenerate) -- migration is idempotent on the L1 side.
	// Read-only permissions from step 6 don't block LoadLegacyIdentity (a
	// read), so no chmod is needed before this second run.
	preRunEvents, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read ledger events before re-run: %v", err)
	}

	second, err := Migrate(root, identityDir, "identity-migration")
	if err != nil {
		t.Fatalf("Migrate (second run): %v", err)
	}
	if second.NewNodeID != result.NewNodeID {
		t.Errorf("NewNodeID changed on re-run: %q != %q", second.NewNodeID, result.NewNodeID)
	}

	// Re-running must NOT append a second node.identity.migrated event to
	// the append-only ledger -- that would be an indistinguishable duplicate
	// provenance record with no way to tell it apart from a genuine second
	// migration. Steps 4-5 must be skipped when the CRD already exists.
	postRunEvents, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read ledger events after re-run: %v", err)
	}
	if string(postRunEvents) != string(preRunEvents) {
		t.Errorf("ledger events changed on re-run; expected steps 4-5 to be skipped as a no-op.\nbefore: %s\nafter:  %s", preRunEvents, postRunEvents)
	}
	if strings.Count(string(postRunEvents), `"type":"node.identity.migrated"`) != 1 {
		t.Errorf("expected exactly one node.identity.migrated event after two Migrate calls, got: %s", postRunEvents)
	}
}

// TestMigrate_RetryAfterEventEmitFailureStillEmitsEvent is the regression
// test for the CRD-as-proxy idempotency bug: if step 4 (WriteNodeCRD)
// succeeded on a prior attempt but step 5 (EmitMigratedEvent) never
// completed -- the run crashed, or hit a disk-full/permission error between
// the two steps -- a retry must still emit the ledger event. The original
// bug used the CRD file's mere existence as proof the whole migration
// (including the event) already happened, so a retry from exactly this
// state would return err == nil while silently never writing the event.
func TestMigrate_RetryAfterEventEmitFailureStillEmitsEvent(t *testing.T) {
	root := t.TempDir()
	identityDir := filepath.Join(t.TempDir(), "l1-identity")
	writeFixtureLegacyIdentity(t, root, "sha256:interleavefixture")

	// Establish the L1 identity the same way Migrate's steps 2-3 would, so
	// the CRD hand-written below records the same NewNodeID Migrate itself
	// will derive.
	l1, err := EnsureL1Identity(identityDir)
	if err != nil {
		t.Fatalf("EnsureL1Identity: %v", err)
	}

	// Simulate the exact interleaving under test: step 4 ran and succeeded
	// on a prior attempt, but step 5 never ran -- no ledger anywhere in this
	// fresh root.
	if _, err := WriteNodeCRD(root, "sha256:interleavefixture", l1.NodeID, time.Now().UTC()); err != nil {
		t.Fatalf("WriteNodeCRD (simulating completed step 4): %v", err)
	}
	ledgerDir := filepath.Join(root, ".cog", "ledger")
	if _, statErr := os.Stat(ledgerDir); !os.IsNotExist(statErr) {
		t.Fatalf("test setup invariant violated: ledger dir already exists before Migrate runs")
	}

	result, err := Migrate(root, identityDir, "identity-migration")
	if err != nil {
		t.Fatalf("Migrate (retry after simulated step-5 failure): %v", err)
	}

	// The load-bearing assertion: the event must actually be there. The
	// original bug returned err == nil here too, with the event silently
	// never written -- checking only the returned error would not have
	// caught it.
	eventsPath := filepath.Join(ledgerDir, "identity-migration", "events.jsonl")
	data, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("expected ledger events file after retry, got: %v", err)
	}
	got := string(data)
	if strings.Count(got, `"type":"node.identity.migrated"`) != 1 {
		t.Fatalf("expected exactly one node.identity.migrated event after retry, got: %s", got)
	}
	if !strings.Contains(got, `"old_node_hash":"sha256:interleavefixture"`) {
		t.Errorf("event missing old_node_hash; got %s", got)
	}
	if result.NewNodeID != l1.NodeID {
		t.Errorf("NewNodeID = %q, want %q", result.NewNodeID, l1.NodeID)
	}

	// A second retry, now that the event genuinely exists, must be a true
	// no-op: still exactly one event, not a duplicate.
	if _, err := Migrate(root, identityDir, "identity-migration"); err != nil {
		t.Fatalf("Migrate (second retry, event now exists): %v", err)
	}
	data2, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatalf("read ledger events after second retry: %v", err)
	}
	if strings.Count(string(data2), `"type":"node.identity.migrated"`) != 1 {
		t.Errorf("expected exactly one node.identity.migrated event after two genuine runs, got: %s", data2)
	}
}

func TestMigrate_MissingLegacyIdentity(t *testing.T) {
	root := t.TempDir()
	identityDir := filepath.Join(t.TempDir(), "l1-identity")

	if _, err := Migrate(root, identityDir, "identity-migration"); err == nil {
		t.Fatal("Migrate: expected error when no legacy identity.json exists, got nil")
	}
}

// writeRawLedgerLine writes a single JSONL line directly into
// <root>/.cog/ledger/<sessionID>/events.jsonl, bypassing cogblock.AppendEvent
// so the tests below can control line size precisely without depending on
// cogblock's own hash-chaining reads.
func writeRawLedgerLine(t *testing.T, root, sessionID string, envelope *cogblock.EventEnvelope) {
	t.Helper()
	line, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal fixture ledger line: %v", err)
	}
	dir := filepath.Join(root, ".cog", "ledger", sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "events.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// migratedEventEnvelope builds a node.identity.migrated event envelope for
// test fixtures. paddingBytes inflates an unrelated data field so the
// marshaled JSONL line reaches a target size, without affecting the fields
// scanForMigratedEvent actually reads.
func migratedEventEnvelope(oldNodeHash, newNodeID string, paddingBytes int) *cogblock.EventEnvelope {
	env := &cogblock.EventEnvelope{
		HashedPayload: cogblock.EventPayload{
			Type:      EventType,
			Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
			SessionID: "identity-migration",
			Data: map[string]interface{}{
				"old_node_hash": oldNodeHash,
				"new_node_id":   newNodeID,
			},
		},
	}
	if paddingBytes > 0 {
		env.HashedPayload.Data["padding"] = strings.Repeat("x", paddingBytes)
	}
	return env
}

// TestScanForMigratedEvent_LineOverDefaultBufSizeIsFound is the lower-bound
// regression test for the idempotency scan's token-cap bug: a real
// node.identity.migrated ledger line over bufio's default 64 KiB
// MaxScanTokenSize -- observed on the operator's live workspace at up to
// 253,902 bytes -- must still be found, not fail with bufio.ErrTooLong.
func TestScanForMigratedEvent_LineOverDefaultBufSizeIsFound(t *testing.T) {
	env := migratedEventEnvelope("sha256:largelinefixture", "newid-fixture", 100*1024)
	line, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	if len(line) <= 64*1024 {
		t.Fatalf("test fixture line is %d bytes, want > 64KiB to exercise the raised cap", len(line))
	}

	found, err := scanForMigratedEvent(strings.NewReader(string(line)+"\n"), "sha256:largelinefixture")
	if err != nil {
		t.Fatalf("scanForMigratedEvent: %v", err)
	}
	if found == nil {
		t.Fatal("scanForMigratedEvent: expected event to be found, got nil")
	}
	if got, _ := found.HashedPayload.Data["old_node_hash"].(string); got != "sha256:largelinefixture" {
		t.Errorf("old_node_hash = %q, want sha256:largelinefixture", got)
	}
}

// TestMigratedEventForHash_UnrelatedOversizedLineDoesNotAbortScan reproduces
// the exact real-workspace failure mode: os.ReadDir returns session
// directories in lexical order, and a single oversized line in an unrelated
// session anywhere in that order must not prevent the scan from reaching a
// later session that holds the real match.
func TestMigratedEventForHash_UnrelatedOversizedLineDoesNotAbortScan(t *testing.T) {
	root := t.TempDir()

	// "aaa-unrelated" sorts before "zzz-target" -- ReadDir hits the oversized,
	// non-matching line first.
	unrelated := migratedEventEnvelope("sha256:someoneelse", "someone-elses-id", 200*1024)
	if len(mustMarshal(t, unrelated)) <= 64*1024 {
		t.Fatal("unrelated fixture line is not actually oversized")
	}
	writeRawLedgerLine(t, root, "aaa-unrelated", unrelated)

	target := migratedEventEnvelope("sha256:target", "target-newid", 0)
	writeRawLedgerLine(t, root, "zzz-target", target)

	found, err := migratedEventForHash(root, "sha256:target")
	if err != nil {
		t.Fatalf("migratedEventForHash: %v", err)
	}
	if found == nil {
		t.Fatal("migratedEventForHash: expected match in zzz-target, got nil")
	}
}

// TestMigrate_SucceedsWithLargeUnrelatedLedgerLine is the end-to-end
// regression test for the blocking review finding: Migrate must complete
// against a workspace whose ledger contains an oversized JSONL line in some
// unrelated session, exactly as the operator's live workspace does. Before
// the scanner.Buffer fix, this failed with "bufio.Scanner: token too long"
// before Migrate ever reached step 4.
func TestMigrate_SucceedsWithLargeUnrelatedLedgerLine(t *testing.T) {
	root := t.TempDir()
	identityDir := filepath.Join(t.TempDir(), "l1-identity")
	writeFixtureLegacyIdentity(t, root, "sha256:largeledgerfixture")

	// Lexically before "identity-migration", so ReadDir walks it first.
	unrelated := migratedEventEnvelope("sha256:unrelated-elsewhere", "unrelated-newid", 200*1024)
	writeRawLedgerLine(t, root, "aaa-unrelated-session", unrelated)

	result, err := Migrate(root, identityDir, "identity-migration")
	if err != nil {
		t.Fatalf("Migrate: %v (an oversized unrelated ledger line must not block migration)", err)
	}
	if result.OldNodeHash != "sha256:largeledgerfixture" {
		t.Errorf("OldNodeHash = %q, want sha256:largeledgerfixture", result.OldNodeHash)
	}
	if _, err := os.Stat(result.CRDPath); err != nil {
		t.Fatalf("CRD not written: %v", err)
	}
}

// TestScanForMigratedEvent_LineExceedingRaisedCapStillErrors is the
// upper-bound counterpart: a line that exceeds even the raised 16 MiB cap
// must still surface a hard error rather than being silently treated as "no
// match found". Swallowing this error would let Migrate conclude the
// migration never happened and re-emit a duplicate node.identity.migrated
// event.
func TestScanForMigratedEvent_LineExceedingRaisedCapStillErrors(t *testing.T) {
	env := migratedEventEnvelope("sha256:toolarge", "toolarge-newid", 16*1024*1024+1024)

	found, err := scanForMigratedEvent(strings.NewReader(string(mustMarshal(t, env))+"\n"), "sha256:toolarge")
	if err == nil {
		t.Fatal("scanForMigratedEvent: expected an error for a line exceeding the raised cap, got nil")
	}
	if found != nil {
		t.Errorf("scanForMigratedEvent: expected nil event alongside the error, got %+v", found)
	}
}

// TestMigratedEventForHash_LineExceedingRaisedCapIsFatal confirms the error
// from the scan above propagates out of migratedEventForHash as an error,
// not as a (nil, nil) "not found" result -- the distinction the false-
// positive-duplicate-event bound depends on.
func TestMigratedEventForHash_LineExceedingRaisedCapIsFatal(t *testing.T) {
	root := t.TempDir()
	env := migratedEventEnvelope("sha256:toolarge", "toolarge-newid", 16*1024*1024+1024)
	writeRawLedgerLine(t, root, "identity-migration", env)

	found, err := migratedEventForHash(root, "sha256:toolarge")
	if err == nil {
		t.Fatal("migratedEventForHash: expected an error for a line exceeding the raised cap, got nil")
	}
	if found != nil {
		t.Errorf("migratedEventForHash: expected nil event alongside the error, got %+v", found)
	}
}

func mustMarshal(t *testing.T, envelope *cogblock.EventEnvelope) []byte {
	t.Helper()
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return data
}
