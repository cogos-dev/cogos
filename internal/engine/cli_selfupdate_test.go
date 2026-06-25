//go:build darwin

package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/myrgic/cogos/internal/providers/selfupdate"
)

// assetName is the platform asset name, shared with the selfupdate package.
func assetName() string { return selfupdate.AssetName() }

// suHashBytes returns the lowercase hex sha256 of b (engine.sha256Hex takes a string).
func suHashBytes(b []byte) string { return sha256Hex(string(b)) }

// ─── version-compare wrappers ────────────────────────────────────────────────

func TestVersionFieldEqual(t *testing.T) {
	if !versionFieldEqual("v0.16.5", "0.16.5") {
		t.Error("v0.16.5 should equal 0.16.5")
	}
	if versionFieldEqual("v0.16.5", "v0.16.4") {
		t.Error("v0.16.5 should not equal v0.16.4")
	}
}

func TestIsDowngrade(t *testing.T) {
	if !isDowngrade("v0.16.3", "v0.16.4") {
		t.Error("v0.16.3 from v0.16.4 is a downgrade")
	}
	if isDowngrade("v0.16.5", "v0.16.4") {
		t.Error("v0.16.5 from v0.16.4 is not a downgrade")
	}
	if isDowngrade("v0.16.3", "dev") {
		t.Error("dev running version yields no downgrade verdict")
	}
}

func TestParseVersionField(t *testing.T) {
	got := parseVersionField("cogos version=v0.16.5 build=2026-06-25T00:00:00Z")
	if got != "v0.16.5" {
		t.Errorf("parseVersionField = %q; want v0.16.5", got)
	}
	if parseVersionField("garbage output") != "" {
		t.Error("missing version= should yield empty string")
	}
}

// ─── checksum verify ─────────────────────────────────────────────────────────

func TestVerifyChecksum(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "cogos-darwin-arm64")
	content := []byte("fake binary bytes")
	if err := os.WriteFile(bin, content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := suHashBytes(content)

	// Match passes.
	sums := fmt.Sprintf("%s  cogos-darwin-arm64\n%s  cogos-linux-amd64\n", sum, suHashBytes([]byte("other")))
	if err := verifyChecksum(bin, "cogos-darwin-arm64", sums); err != nil {
		t.Errorf("expected match, got %v", err)
	}

	// Single-bit corruption fails.
	corrupt := sum[:len(sum)-1] + flipHexDigit(sum[len(sum)-1])
	badSums := fmt.Sprintf("%s  cogos-darwin-arm64\n", corrupt)
	if err := verifyChecksum(bin, "cogos-darwin-arm64", badSums); err == nil {
		t.Error("expected mismatch error for corrupted checksum")
	}

	// Missing entry fails.
	if err := verifyChecksum(bin, "cogos-darwin-arm64", "deadbeef  cogos-windows-amd64.exe\n"); err == nil {
		t.Error("expected missing-entry error")
	}

	// Malformed line tolerated; correct line still matches.
	tolerant := "this line is malformed\n" + fmt.Sprintf("%s  cogos-darwin-arm64\n", sum)
	if err := verifyChecksum(bin, "cogos-darwin-arm64", tolerant); err != nil {
		t.Errorf("malformed line should be tolerated, got %v", err)
	}
}

func flipHexDigit(c byte) string {
	if c == '0' {
		return "1"
	}
	return "0"
}

// ─── swap via seam (no live restart) ─────────────────────────────────────────

// newTestUpdater builds a selfUpdater pointed at a temp binDir with all network
// and launchd seams stubbed. The caller overrides individual seams per case.
func newTestUpdater(t *testing.T, toTag string) (*selfUpdater, string) {
	t.Helper()
	binDir := t.TempDir()
	runDir := t.TempDir()

	u := &selfUpdater{
		repo:           "myrgic/cogos",
		toTag:          toTag,
		root:           t.TempDir(),
		port:           6931,
		binDir:         binDir,
		runDirOverride: runDir, // keep the lockfile out of the real ~/.cog/run
		logf:           func(string, ...any) {},
	}
	// Lock lives in the temp run dir.
	u.kickstart = func() error { return nil }
	u.healthPoll = func(time.Duration) error { return nil }
	u.rollbackPoll = func(time.Duration, string) error { return nil }
	u.fetchText = func(ctx context.Context, url string) (string, error) { return "", nil }
	u.smokeTest = func(binPath string) (string, error) { return toTag, nil }

	// Write the current binary so backup/swap have something to copy. The
	// lockfile is redirected into runDir via u.runDirOverride above.
	writeFakeBinary(t, filepath.Join(binDir, "cogos"), "OLD")
	return u, binDir
}

func writeFakeBinary(t *testing.T, path, marker string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho cogos version="+marker+"\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
}

func newPayload(t *testing.T, tag string) ([]byte, string) {
	t.Helper()
	content := []byte("NEW BINARY " + tag)
	return content, suHashBytes(content)
}

func TestRunApplySuccessLeavesNewAndRemovesBak(t *testing.T) {
	u, binDir := newTestUpdater(t, "v0.16.5")
	payload, sum := newPayload(t, "v0.16.5")

	u.download = func(ctx context.Context, url, dst string) error {
		return os.WriteFile(dst, payload, 0o644)
	}
	u.fetchText = func(ctx context.Context, url string) (string, error) {
		return fmt.Sprintf("%s  %s\n", sum, assetName()), nil
	}
	// resolveAssetURLs hits the network; stub it via the seam used internally is
	// not possible (it is a free function), so we exercise runApply with a tag
	// whose URLs we don't actually fetch — download/fetchText are stubbed, but
	// resolveAssetURLs still runs. Bypass by calling the swap directly.
	if err := runApplyWithStubResolve(u); err != nil {
		t.Fatalf("runApply: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(binDir, "cogos"))
	if err != nil {
		t.Fatalf("read swapped binary: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("swapped binary content = %q; want new payload", got)
	}
	if _, err := os.Stat(filepath.Join(binDir, "cogos.bak")); !os.IsNotExist(err) {
		t.Error("backup should be removed on success")
	}
}

func TestRunApplyChecksumFailLeavesOriginal(t *testing.T) {
	u, binDir := newTestUpdater(t, "v0.16.5")
	payload, _ := newPayload(t, "v0.16.5")
	origContent, _ := os.ReadFile(filepath.Join(binDir, "cogos"))

	u.download = func(ctx context.Context, url, dst string) error {
		return os.WriteFile(dst, payload, 0o644)
	}
	// Wrong checksum.
	u.fetchText = func(ctx context.Context, url string) (string, error) {
		return fmt.Sprintf("%s  %s\n", suHashBytes([]byte("different")), assetName()), nil
	}
	err := runApplyWithStubResolve(u)
	if err == nil {
		t.Fatal("expected checksum-fail error")
	}
	got, _ := os.ReadFile(filepath.Join(binDir, "cogos"))
	if string(got) != string(origContent) {
		t.Error("original binary must be untouched on checksum failure")
	}
	if _, err := os.Stat(filepath.Join(binDir, "cogos.bak")); !os.IsNotExist(err) {
		t.Error("no .bak should exist when we abort before backup")
	}
}

func TestRunApplyHealthFailRollsBack(t *testing.T) {
	u, binDir := newTestUpdater(t, "v0.16.5")
	payload, sum := newPayload(t, "v0.16.5")
	origContent, _ := os.ReadFile(filepath.Join(binDir, "cogos"))

	u.download = func(ctx context.Context, url, dst string) error {
		return os.WriteFile(dst, payload, 0o644)
	}
	u.fetchText = func(ctx context.Context, url string) (string, error) {
		return fmt.Sprintf("%s  %s\n", sum, assetName()), nil
	}
	// Health never converges after the swap → rollback.
	u.healthPoll = func(time.Duration) error { return fmt.Errorf("never healthy") }
	// Rollback's own re-poll succeeds (the restored old binary comes back healthy).
	u.rollbackPoll = func(time.Duration, string) error { return nil }
	err := runApplyWithStubResolve(u)
	if err == nil {
		t.Fatal("expected health-fail to surface as error (rolled back)")
	}
	got, _ := os.ReadFile(filepath.Join(binDir, "cogos"))
	if string(got) != string(origContent) {
		t.Errorf("rollback must restore original binary; got %q", got)
	}
}

func TestRunApplyRenameFailRestoresFromBak(t *testing.T) {
	u, binDir := newTestUpdater(t, "v0.16.5")
	payload, sum := newPayload(t, "v0.16.5")
	origContent, _ := os.ReadFile(filepath.Join(binDir, "cogos"))

	u.download = func(ctx context.Context, url, dst string) error {
		// Write the new file as a directory-named path so rename will fail? Simpler:
		// after download+verify+backup, remove the new file to force rename ENOENT.
		if err := os.WriteFile(dst, payload, 0o644); err != nil {
			return err
		}
		return nil
	}
	u.fetchText = func(ctx context.Context, url string) (string, error) {
		return fmt.Sprintf("%s  %s\n", sum, assetName()), nil
	}
	// Force the rename to fail by deleting newPath right before swap via a hook
	// on smokeTest (runs immediately before backup+rename).
	u.smokeTest = func(binPath string) (string, error) {
		// binPath here is the .new file; delete it so the subsequent os.Rename fails.
		_ = os.Remove(binPath)
		return "v0.16.5", nil
	}
	err := runApplyWithStubResolve(u)
	if err == nil {
		t.Fatal("expected rename failure")
	}
	got, _ := os.ReadFile(filepath.Join(binDir, "cogos"))
	if string(got) != string(origContent) {
		t.Errorf("rename-fail path must restore original from .bak; got %q", got)
	}
}

// runApplyWithStubResolve runs the swap core but bypasses the network release
// resolution by injecting the asset URLs the production code would have fetched.
// It mirrors runApply exactly except resolveAssetURLs is replaced by a stub.
func runApplyWithStubResolve(u *selfUpdater) error {
	prev := resolveAssetURLsFn
	resolveAssetURLsFn = func(ctx context.Context, repo, tag string) (*assetURLs, error) {
		return &assetURLs{
			AssetName:   assetName(),
			AssetURL:    "https://example.invalid/" + assetName(),
			ChecksumURL: "https://example.invalid/checksums.txt",
		}, nil
	}
	defer func() { resolveAssetURLsFn = prev }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return u.runApply(ctx)
}
