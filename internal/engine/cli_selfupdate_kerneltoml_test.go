package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sampleKernelTOML mirrors the shipped .cog/conf/kernel.toml layout: header
// comments, a [kernel] table with a version + URL templates, and a
// [kernel.checksums] table with the current platform plus a comment.
const sampleKernelTOML = `# Single source of truth for the cogos kernel binary version + checksums.
# Set at initial install; the goal is for the self-update provider to keep this
# reconciled with the running daemon.

[kernel]
version = "v0.16.15"
release_url_template = "https://github.com/myrgic/cogos/releases/download/{version}/cogos-{os}-{arch}"
checksums_url_template = "https://github.com/myrgic/cogos/releases/download/{version}/checksums.txt"

[kernel.checksums]
darwin-arm64 = "32a4f1608d402ff316bfa8584a42fe1ca6e98a7c00ac87112c29ea9f6a9659b3"
linux-amd64 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
# Other platforms can be added by reading the full checksums.txt from the release.
`

func TestKernelTOMLPlatformKey(t *testing.T) {
	cases := []struct {
		asset  string
		want   string
		wantOK bool
	}{
		{"cogos-darwin-arm64", "darwin-arm64", true},
		{"cogos-linux-amd64", "linux-amd64", true},
		{"cogos-windows-amd64.exe", "windows-amd64", true},
		{"cogos-", "", false},
		{"notcogos-darwin-arm64", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := kernelTOMLPlatformKey(c.asset)
		if got != c.want || ok != c.wantOK {
			t.Errorf("kernelTOMLPlatformKey(%q) = (%q,%v); want (%q,%v)", c.asset, got, ok, c.want, c.wantOK)
		}
	}
}

func TestPatchKernelTOML_UpdatesVersionAndCurrentPlatform(t *testing.T) {
	out, err := patchKernelTOML([]byte(sampleKernelTOML), "v0.16.16", "darwin-arm64", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	if err != nil {
		t.Fatalf("patchKernelTOML: %v", err)
	}
	s := string(out)

	if !strings.Contains(s, `version = "v0.16.16"`) {
		t.Errorf("version not updated:\n%s", s)
	}
	if strings.Contains(s, `version = "v0.16.15"`) {
		t.Errorf("stale version still present:\n%s", s)
	}
	if !strings.Contains(s, `darwin-arm64 = "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"`) {
		t.Errorf("current-platform checksum not updated:\n%s", s)
	}
	// Other platform preserved verbatim (issue #442: never blank non-current).
	if !strings.Contains(s, `linux-amd64 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"`) {
		t.Errorf("other-platform checksum must be preserved:\n%s", s)
	}
	// URL templates and comments preserved.
	if !strings.Contains(s, "release_url_template = ") {
		t.Errorf("release_url_template must be preserved:\n%s", s)
	}
	if !strings.Contains(s, "# Single source of truth") {
		t.Errorf("header comment must be preserved:\n%s", s)
	}
	if !strings.Contains(s, "# Other platforms can be added") {
		t.Errorf("checksums-section comment must be preserved:\n%s", s)
	}
	// Trailing newline preserved.
	if !strings.HasSuffix(s, "\n") {
		t.Errorf("trailing newline must be preserved")
	}
}

func TestPatchKernelTOML_AppendsMissingPlatform(t *testing.T) {
	// windows-amd64 is not present in the sample; a write-ahead for it must add
	// the entry to [kernel.checksums] without touching the existing platforms.
	out, err := patchKernelTOML([]byte(sampleKernelTOML), "v0.16.16", "windows-amd64", "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe")
	if err != nil {
		t.Fatalf("patchKernelTOML: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `windows-amd64 = "cafebabecafebabecafebabecafebabecafebabecafebabecafebabecafebabe"`) {
		t.Errorf("new platform entry not appended:\n%s", s)
	}
	if !strings.Contains(s, `darwin-arm64 = "32a4f1`) {
		t.Errorf("existing darwin entry must survive:\n%s", s)
	}
	if !strings.Contains(s, `linux-amd64 = "aaaa`) {
		t.Errorf("existing linux entry must survive:\n%s", s)
	}
	// The new entry must land inside the [kernel.checksums] section, i.e. before
	// no later table header (there is none here) and after the header.
	secIdx := strings.Index(s, "[kernel.checksums]")
	winIdx := strings.Index(s, "windows-amd64 =")
	if secIdx < 0 || winIdx < secIdx {
		t.Errorf("windows entry must appear after the [kernel.checksums] header:\n%s", s)
	}
}

func TestPatchKernelTOML_CreatesMissingChecksumsSection(t *testing.T) {
	src := "[kernel]\nversion = \"v0.16.15\"\n"
	out, err := patchKernelTOML([]byte(src), "v0.16.16", "darwin-arm64", "beefbeef")
	if err != nil {
		t.Fatalf("patchKernelTOML: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, "[kernel.checksums]") {
		t.Errorf("missing [kernel.checksums] section must be created:\n%s", s)
	}
	if !strings.Contains(s, `darwin-arm64 = "beefbeef"`) {
		t.Errorf("checksum entry must be created:\n%s", s)
	}
	if !strings.Contains(s, `version = "v0.16.16"`) {
		t.Errorf("version must be updated:\n%s", s)
	}
}

func TestPatchKernelTOML_CreatesMissingVersion(t *testing.T) {
	src := "[kernel]\nrelease_url_template = \"x\"\n\n[kernel.checksums]\ndarwin-arm64 = \"old\"\n"
	out, err := patchKernelTOML([]byte(src), "v0.16.16", "darwin-arm64", "new")
	if err != nil {
		t.Fatalf("patchKernelTOML: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `version = "v0.16.16"`) {
		t.Errorf("version must be inserted under [kernel]:\n%s", s)
	}
	if !strings.Contains(s, `darwin-arm64 = "new"`) {
		t.Errorf("checksum must be updated:\n%s", s)
	}
	// Version must appear before the checksums section (i.e. inside [kernel]).
	if strings.Index(s, `version = "v0.16.16"`) > strings.Index(s, "[kernel.checksums]") {
		t.Errorf("inserted version must live under [kernel], not after checksums:\n%s", s)
	}
}

func TestPatchKernelTOML_SubtableLayout(t *testing.T) {
	src := "[kernel]\nversion = \"v0.16.15\"\n\n[kernel.checksums.darwin-arm64]\nsha256 = \"old\"\n"
	out, err := patchKernelTOML([]byte(src), "v0.16.16", "darwin-arm64", "newsum")
	if err != nil {
		t.Fatalf("patchKernelTOML: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `sha256 = "newsum"`) {
		t.Errorf("subtable sha256 must be updated:\n%s", s)
	}
	if strings.Contains(s, `sha256 = "old"`) {
		t.Errorf("stale subtable sha256 must be gone:\n%s", s)
	}
}

// TestPatchKernelTOML_QuotedKeyReplacedInPlace guards the review finding that a
// quoted checksum key (valid TOML for a hyphenated key) was not matched against
// the bare platform key, so the current-platform entry was replaced in place by
// a SECOND appended `darwin-arm64 = ...` line — duplicate-key TOML that strict
// parsers reject. Exactly one entry must survive, updated in place, quoted form
// preserved.
func TestPatchKernelTOML_QuotedKeyReplacedInPlace(t *testing.T) {
	src := "[kernel]\nversion = \"v0.16.15\"\n\n[kernel.checksums]\n\"darwin-arm64\" = \"OLD\"\n"
	out, err := patchKernelTOML([]byte(src), "v0.16.16", "darwin-arm64", "NEWSUM")
	if err != nil {
		t.Fatalf("patchKernelTOML: %v", err)
	}
	s := string(out)
	if n := strings.Count(s, "darwin-arm64"); n != 1 {
		t.Errorf("expected exactly one darwin-arm64 entry, got %d:\n%s", n, s)
	}
	if strings.Contains(s, "OLD") {
		t.Errorf("stale checksum must be replaced, not left behind:\n%s", s)
	}
	if !strings.Contains(s, `"darwin-arm64" = "NEWSUM"`) {
		t.Errorf("quoted key must be updated in place, quoting preserved:\n%s", s)
	}
}

// Single-quoted keys ('darwin-arm64') are also valid TOML and must match.
func TestPatchKernelTOML_SingleQuotedKeyReplacedInPlace(t *testing.T) {
	src := "[kernel]\nversion = \"v0.16.15\"\n\n[kernel.checksums]\n'darwin-arm64' = \"OLD\"\n"
	out, err := patchKernelTOML([]byte(src), "v0.16.16", "darwin-arm64", "NEWSUM")
	if err != nil {
		t.Fatalf("patchKernelTOML: %v", err)
	}
	s := string(out)
	if n := strings.Count(s, "darwin-arm64"); n != 1 {
		t.Errorf("expected exactly one darwin-arm64 entry, got %d:\n%s", n, s)
	}
	if !strings.Contains(s, `'darwin-arm64' = "NEWSUM"`) {
		t.Errorf("single-quoted key must be updated in place:\n%s", s)
	}
}

// TestPatchKernelTOML_BareVersionBeforeKernelErrors guards the finding that a
// top-level bare `version` before any [kernel] header caused a fresh
// [kernel].version to be prepended — duplicating the version key and stranding
// the stale value. The safe behaviour is to refuse (caller aborts write-ahead,
// running binary untouched) rather than emit invalid/misleading TOML.
func TestPatchKernelTOML_BareVersionBeforeKernelErrors(t *testing.T) {
	src := "version = \"v0.16.15\"\n\n[kernel]\nrelease_url_template = \"x\"\n\n[kernel.checksums]\ndarwin-arm64 = \"old\"\n"
	if _, err := patchKernelTOML([]byte(src), "v0.16.16", "darwin-arm64", "new"); err == nil {
		t.Error("expected an error for a top-level version before [kernel], got nil")
	}
}

// TestPatchKernelTOML_SubtableOnlyMissingCurrentErrors guards the finding that,
// when checksums are defined ONLY via [kernel.checksums.<other>] subtables and
// the current platform is absent, appending an inline [kernel.checksums] table
// after its child subtable is invalid TOML (parent redefined after child). The
// safe behaviour is to refuse rather than emit that layout.
func TestPatchKernelTOML_SubtableOnlyMissingCurrentErrors(t *testing.T) {
	src := "[kernel]\nversion = \"v0.16.15\"\n\n[kernel.checksums.linux-amd64]\nsha256 = \"old\"\n"
	if _, err := patchKernelTOML([]byte(src), "v0.16.16", "darwin-arm64", "new"); err == nil {
		t.Error("expected an error for subtable-only checksums missing the current platform, got nil")
	}
}

func TestWriteAheadKernelTOML_NoRootIsNoOp(t *testing.T) {
	snap, err := writeAheadKernelTOML("", "v0.16.16", "cogos-darwin-arm64", "abc")
	if err != nil {
		t.Fatalf("writeAheadKernelTOML: %v", err)
	}
	if snap.existed {
		t.Error("no-root write-ahead must be a no-op snapshot")
	}
	if err := rollbackKernelTOML(snap); err != nil {
		t.Errorf("rollback of a no-op snapshot must succeed: %v", err)
	}
}

func TestWriteAheadKernelTOML_AbsentFileIsNoOp(t *testing.T) {
	root := t.TempDir() // no .cog/conf/kernel.toml
	snap, err := writeAheadKernelTOML(root, "v0.16.16", "cogos-darwin-arm64", "abc")
	if err != nil {
		t.Fatalf("writeAheadKernelTOML: %v", err)
	}
	if snap.existed {
		t.Error("absent-file write-ahead must not synthesize a kernel.toml")
	}
	if _, statErr := os.Stat(filepath.Join(root, kernelTOMLRel)); !os.IsNotExist(statErr) {
		t.Error("write-ahead must NOT create kernel.toml when it was absent")
	}
}

func TestWriteAheadKernelTOML_WritesThenRollsBack(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, kernelTOMLRel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(sampleKernelTOML), 0o644); err != nil {
		t.Fatal(err)
	}

	snap, err := writeAheadKernelTOML(root, "v0.16.16", "cogos-darwin-arm64", "feedface")
	if err != nil {
		t.Fatalf("writeAheadKernelTOML: %v", err)
	}
	if !snap.existed {
		t.Fatal("snapshot must record that a kernel.toml existed")
	}

	// After write-ahead: version + darwin checksum updated on disk.
	afterWrite, _ := os.ReadFile(path)
	if !strings.Contains(string(afterWrite), `version = "v0.16.16"`) {
		t.Errorf("on-disk kernel.toml must show the new version after write-ahead:\n%s", afterWrite)
	}
	if !strings.Contains(string(afterWrite), `darwin-arm64 = "feedface"`) {
		t.Errorf("on-disk kernel.toml must show the new checksum after write-ahead:\n%s", afterWrite)
	}
	// File mode must be preserved across the write-ahead (atomicWriteConfigFile
	// otherwise narrows it to 0o600 via os.CreateTemp).
	if fi, serr := os.Stat(path); serr != nil {
		t.Fatalf("stat after write-ahead: %v", serr)
	} else if fi.Mode().Perm() != 0o644 {
		t.Errorf("write-ahead must preserve mode 0o644, got %o", fi.Mode().Perm())
	}

	// Rollback restores the exact prior bytes.
	if err := rollbackKernelTOML(snap); err != nil {
		t.Fatalf("rollbackKernelTOML: %v", err)
	}
	restored, _ := os.ReadFile(path)
	if string(restored) != sampleKernelTOML {
		t.Errorf("rollback must restore the exact prior bytes.\n got:\n%s\nwant:\n%s", restored, sampleKernelTOML)
	}
	// Rollback must restore the original mode too.
	if fi, serr := os.Stat(path); serr != nil {
		t.Fatalf("stat after rollback: %v", serr)
	} else if fi.Mode().Perm() != 0o644 {
		t.Errorf("rollback must restore mode 0o644, got %o", fi.Mode().Perm())
	}
}
