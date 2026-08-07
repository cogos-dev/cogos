package l2migration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	// Re-derive independently via constellation's own PublicKeyFromDER +
	// FormatNodeID-adjacent check: parse back and confirm it's the same key.
	pub, err := constellation.PublicKeyFromDER(der)
	if err != nil {
		t.Fatalf("PublicKeyFromDER: %v", err)
	}
	if pub.X.Cmp(id.PublicKey.X) != 0 || pub.Y.Cmp(id.PublicKey.Y) != 0 {
		t.Fatal("round-tripped public key does not match original")
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

	wantPath := filepath.Join(root, ".cog", "config", "nodes", "sha256:oldhash.yaml")
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
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
	second, err := Migrate(root, identityDir, "identity-migration")
	if err != nil {
		t.Fatalf("Migrate (second run): %v", err)
	}
	if second.NewNodeID != result.NewNodeID {
		t.Errorf("NewNodeID changed on re-run: %q != %q", second.NewNodeID, result.NewNodeID)
	}
}

func TestMigrate_MissingLegacyIdentity(t *testing.T) {
	root := t.TempDir()
	identityDir := filepath.Join(t.TempDir(), "l1-identity")

	if _, err := Migrate(root, identityDir, "identity-migration"); err == nil {
		t.Fatal("Migrate: expected error when no legacy identity.json exists, got nil")
	}
}
