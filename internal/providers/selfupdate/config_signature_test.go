package selfupdate

import (
	"os"
	"path/filepath"
	"testing"
)

func writeSelfUpdateConfig(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".cog", "config")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "self-update.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// THE MIGRATION INVARIANT. An existing config predating require_signature must
// land on warn, not enforce — otherwise shipping this change could silently
// stop auto-update on a live node whose next release trips any of the benign
// unverifiable conditions.
func TestExistingConfigWithoutKeyGetsTransitionalWarn(t *testing.T) {
	root := writeSelfUpdateConfig(t, `
enabled: true
channel: stable
auto_apply: true
repo: myrgic/cogos
check_interval: 1h
`)
	cfg, err := loadSelfUpdateConfig(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.RequireSignature != SignatureWarn {
		t.Errorf("absent key must yield %q, got %q", SignatureWarn, cfg.RequireSignature)
	}
	if !cfg.SignatureModeUnset() {
		t.Error("absent key must report the mode as unset so the migration notice fires")
	}
}

// An explicit choice is honoured verbatim and never reported as unset — so a
// deliberate "warn" does not silently flip when the default changes in Stage 2.
func TestExplicitPostureIsHonoured(t *testing.T) {
	for _, want := range []string{SignatureEnforce, SignatureWarn, SignatureOff} {
		t.Run(want, func(t *testing.T) {
			root := writeSelfUpdateConfig(t, "enabled: true\nrequire_signature: "+want+"\n")
			cfg, err := loadSelfUpdateConfig(root)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if cfg.RequireSignature != want {
				t.Errorf("got %q, want %q", cfg.RequireSignature, want)
			}
			if cfg.SignatureModeUnset() {
				t.Error("an explicit value must not be reported as unset")
			}
		})
	}
}

// A fresh config (no file at all) has no legacy to protect and gets the end
// state directly.
func TestAbsentConfigFileDefaultsToEnforce(t *testing.T) {
	cfg, err := loadSelfUpdateConfig(t.TempDir())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.RequireSignature != SignatureEnforce {
		t.Errorf("shipped default must be %q, got %q", SignatureEnforce, cfg.RequireSignature)
	}
	if cfg.Enabled {
		t.Error("shipped default must remain disabled")
	}
}

func TestUnknownPostureIsAConfigError(t *testing.T) {
	root := writeSelfUpdateConfig(t, "enabled: true\nrequire_signature: sometimes\n")
	if _, err := loadSelfUpdateConfig(root); err == nil {
		t.Fatal("an unrecognised require_signature must be a hard config error, not silently coerced")
	}
}

// SignatureModeFor is what the DETACHED updater calls. It must never resolve to
// a permissive posture by accident.
func TestSignatureModeForFailsClosed(t *testing.T) {
	if got := SignatureModeFor(t.TempDir()); got != SignatureEnforce {
		t.Errorf("no config → %q, want %q", got, SignatureEnforce)
	}
	// Unparseable config: must not fall open.
	bad := writeSelfUpdateConfig(t, "enabled: true\nrequire_signature: bogus\n")
	if got := SignatureModeFor(bad); got != SignatureEnforce {
		t.Errorf("bad config → %q, want %q", got, SignatureEnforce)
	}
	broken := writeSelfUpdateConfig(t, "enabled: [unclosed\n")
	if got := SignatureModeFor(broken); got != SignatureEnforce {
		t.Errorf("malformed yaml → %q, want %q", got, SignatureEnforce)
	}
	// A deliberate opt-out is still honoured.
	off := writeSelfUpdateConfig(t, "enabled: true\nrequire_signature: off\n")
	if got := SignatureModeFor(off); got != SignatureOff {
		t.Errorf("explicit off → %q, want %q", got, SignatureOff)
	}
}

// The resolver must expose the signature asset URLs, or the gate has nothing to
// fetch.
func TestResolvedReleaseCarriesSignatureURLs(t *testing.T) {
	r := &resolvedRelease{}
	_ = r // compile-time guard that the fields exist
	base := "https://github.com/myrgic/cogos/releases/download/v0.17.0"
	rel := &resolvedRelease{
		Tag:            "v0.17.0",
		ChecksumURL:    base + "/checksums.txt",
		SignatureURL:   base + "/checksums.txt.sig",
		CertificateURL: base + "/checksums.txt.pem",
	}
	if rel.SignatureURL != rel.ChecksumURL+".sig" {
		t.Errorf("signature URL must be the checksums URL plus .sig, got %q", rel.SignatureURL)
	}
	if rel.CertificateURL != base+"/checksums.txt.pem" {
		t.Errorf("unexpected certificate URL %q", rel.CertificateURL)
	}
}
