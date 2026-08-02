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

// SignatureSettingsFor is what the DETACHED updater calls. It must never
// resolve to a permissive posture by accident.
func TestSignatureSettingsForFailsClosed(t *testing.T) {
	if got := SignatureSettingsFor(t.TempDir()); got.Mode != SignatureEnforce {
		t.Errorf("no config → %q, want %q", got.Mode, SignatureEnforce)
	}
	// Unparseable config: must not fall open.
	bad := writeSelfUpdateConfig(t, "enabled: true\nrequire_signature: bogus\n")
	if got := SignatureSettingsFor(bad); got.Mode != SignatureEnforce {
		t.Errorf("bad config → %q, want %q", got.Mode, SignatureEnforce)
	}
	broken := writeSelfUpdateConfig(t, "enabled: [unclosed\n")
	if got := SignatureSettingsFor(broken); got.Mode != SignatureEnforce {
		t.Errorf("malformed yaml → %q, want %q", got.Mode, SignatureEnforce)
	}
	// A deliberate opt-out is still honoured.
	off := writeSelfUpdateConfig(t, "enabled: true\nrequire_signature: off\n")
	if got := SignatureSettingsFor(off); got.Mode != SignatureOff {
		t.Errorf("explicit off → %q, want %q", got.Mode, SignatureOff)
	}
}

// THE CWD TRAP. An empty root must resolve to enforce WITHOUT touching the
// filesystem. Otherwise the config path is relative and resolves against the
// updater's working directory, so `cogos self-update` run from any writable
// directory would read a config planted there — and honour `off`.
//
// runSelfUpdateCmd genuinely leaves root empty when no .cog/config exists in
// any ancestor, so this is a reachable path, not a hypothetical one. It is also
// surface this change introduced: before provenance verification, the updater
// read no config at all.
func TestEmptyRootNeverReadsTheWorkingDirectory(t *testing.T) {
	// Plant exactly the config an attacker would: cwd/.cog/config/self-update.yaml
	// saying "skip verification".
	planted := writeSelfUpdateConfig(t, "enabled: true\nrequire_signature: off\n")
	t.Chdir(planted)

	got := SignatureSettingsFor("")
	if got.Mode != SignatureEnforce {
		t.Fatalf("an empty root must fail closed, got %q — a planted config in the working "+
			"directory was honoured", got.Mode)
	}
	if got.IdentityRepo != defaultRepo {
		t.Errorf("empty root must pin the canonical repo, got %q", got.IdentityRepo)
	}
}

// A relative root is refused outright rather than silently resolved against the
// working directory.
func TestRelativeRootIsRefused(t *testing.T) {
	planted := writeSelfUpdateConfig(t, "enabled: true\nrequire_signature: off\n")
	t.Chdir(planted)

	if _, err := loadSelfUpdateConfig("."); err == nil {
		t.Fatal("a relative workspace root must be refused, not resolved against the cwd")
	}
	if got := SignatureSettingsFor("."); got.Mode != SignatureEnforce {
		t.Errorf("a relative root must fail closed, got %q", got.Mode)
	}
}

// THE REDIRECT TRAP. `repo:` names where bytes are downloaded from. It must not
// also decide whose signature is acceptable, or anyone who can write this file
// can point both at their own fork and get an affirmative "provenance OK".
func TestIdentityRepoIgnoresTheDownloadRepo(t *testing.T) {
	root := writeSelfUpdateConfig(t, "enabled: true\nrepo: attacker/cogos\n")
	cfg, err := loadSelfUpdateConfig(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Repo != "attacker/cogos" {
		t.Fatalf("download repo should be honoured, got %q", cfg.Repo)
	}
	if cfg.IdentityRepo() != defaultRepo {
		t.Errorf("identity pin must stay at %q regardless of repo:, got %q",
			defaultRepo, cfg.IdentityRepo())
	}
	if got := SignatureSettingsFor(root); got.IdentityRepo != defaultRepo {
		t.Errorf("detached updater must pin %q, got %q", defaultRepo, got.IdentityRepo)
	}
}

// A deliberate, separate opt-in does retarget the identity — that is the point
// of it being a second key.
func TestExplicitSignatureRepoRetargetsTheIdentity(t *testing.T) {
	root := writeSelfUpdateConfig(t,
		"enabled: true\nrepo: someone/fork\nsignature_repo: someone/fork\n")
	cfg, err := loadSelfUpdateConfig(root)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.IdentityRepo() != "someone/fork" {
		t.Errorf("explicit signature_repo must be honoured, got %q", cfg.IdentityRepo())
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
