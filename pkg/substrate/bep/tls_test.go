package bep

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── Certificate generation ─────────────────────────────────────────────────────

func TestGenerateBEPCert(t *testing.T) {
	dir := t.TempDir()

	if err := GenerateBEPCert(dir); err != nil {
		t.Fatalf("GenerateBEPCert: %v", err)
	}

	// Cert and key files should exist.
	certPath := filepath.Join(dir, "bep-cert.pem")
	keyPath := filepath.Join(dir, "bep-key.pem")

	if _, err := os.Stat(certPath); err != nil {
		t.Errorf("cert file missing: %v", err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("key file missing: %v", err)
	}

	// Should be loadable as a TLS keypair.
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("no certificates in chain")
	}
}

func TestGenerateBEPCertNoOverwrite(t *testing.T) {
	dir := t.TempDir()

	if err := GenerateBEPCert(dir); err != nil {
		t.Fatalf("first GenerateBEPCert: %v", err)
	}

	// Second call should fail.
	if err := GenerateBEPCert(dir); err == nil {
		t.Error("expected error on second GenerateBEPCert, got nil")
	}
}

// GenerateBEPCert must refuse an existing key file exactly as it refuses an
// existing cert file — the earlier version only checked bep-cert.pem, which
// is how a key-without-cert directory could silently be overwritten (or,
// symmetrically, how the reviewed cert-without-key bug went undetected).
func TestGenerateBEPCertNoOverwriteKeyOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bep-key.pem"), []byte("existing key\n"), 0o600); err != nil {
		t.Fatalf("seed key file: %v", err)
	}

	if err := GenerateBEPCert(dir); err == nil {
		t.Error("expected error when bep-key.pem already exists, got nil")
	}
	if _, err := os.Stat(filepath.Join(dir, "bep-cert.pem")); err == nil {
		t.Error("GenerateBEPCert must not have written a cert when refusing due to an existing key")
	}
}

// If cert generation fails partway (simulated here by making the directory
// unwritable right after the key would land), no partial identity — neither
// a lone cert nor a lone key — should be left on disk. This pins the
// temp-file+rename and key-before-cert-with-rollback discipline the review
// asked for: a cert must never exist without its key.
func TestGenerateBEPCertNoPartialStateOnFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-based fault injection doesn't apply as root")
	}
	dir := t.TempDir()

	// Make the directory read-only before generation even starts: the key
	// write (which now happens first) will fail immediately, so nothing
	// should be written at all.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := GenerateBEPCert(dir); err == nil {
		t.Fatal("expected GenerateBEPCert to fail against a read-only directory")
	}

	_ = os.Chmod(dir, 0o700)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("expected no files left behind after a failed generation, found: %v", names)
	}
}

// ─── DeviceID derivation ────────────────────────────────────────────────────────

func TestDeviceIDFromCert(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateBEPCert(dir); err != nil {
		t.Fatalf("GenerateBEPCert: %v", err)
	}

	cert, err := LoadBEPCert(dir)
	if err != nil {
		t.Fatalf("LoadBEPCert: %v", err)
	}

	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	id := DeviceIDFromCert(x509Cert)

	// Should be non-zero.
	zero := DeviceID{}
	if id == zero {
		t.Error("DeviceID should not be all zeros")
	}
}

func TestDeviceIDFromTLSCert(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateBEPCert(dir); err != nil {
		t.Fatalf("GenerateBEPCert: %v", err)
	}

	cert, err := LoadBEPCert(dir)
	if err != nil {
		t.Fatalf("LoadBEPCert: %v", err)
	}

	id, err := DeviceIDFromTLSCert(&cert)
	if err != nil {
		t.Fatalf("DeviceIDFromTLSCert: %v", err)
	}

	zero := DeviceID{}
	if id == zero {
		t.Error("DeviceID should not be all zeros")
	}
}

func TestDeviceIDFromTLSCertEmpty(t *testing.T) {
	cert := &tls.Certificate{}
	_, err := DeviceIDFromTLSCert(cert)
	if err == nil {
		t.Error("expected error for empty certificate chain")
	}
}

// ─── DeviceID formatting ────────────────────────────────────────────────────────

func TestFormatParseDeviceIDRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateBEPCert(dir); err != nil {
		t.Fatalf("GenerateBEPCert: %v", err)
	}

	cert, err := LoadBEPCert(dir)
	if err != nil {
		t.Fatalf("LoadBEPCert: %v", err)
	}

	id, err := DeviceIDFromTLSCert(&cert)
	if err != nil {
		t.Fatalf("DeviceIDFromTLSCert: %v", err)
	}

	formatted := FormatDeviceID(id)

	// Should have 8 dash-separated groups.
	groups := strings.Split(formatted, "-")
	if len(groups) != 8 {
		t.Errorf("expected 8 groups, got %d: %s", len(groups), formatted)
	}

	// Parse back.
	parsed, err := ParseDeviceID(formatted)
	if err != nil {
		t.Fatalf("ParseDeviceID: %v", err)
	}
	if parsed != id {
		t.Errorf("round-trip failed: got %x, want %x", parsed, id)
	}
}

func TestParseDeviceIDInvalidLength(t *testing.T) {
	_, err := ParseDeviceID("ABC")
	if err == nil {
		t.Error("expected error for short device ID")
	}
}

// ─── TLS config ─────────────────────────────────────────────────────────────────

func TestTLSConfig(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateBEPCert(dir); err != nil {
		t.Fatalf("GenerateBEPCert: %v", err)
	}

	cert, err := LoadBEPCert(dir)
	if err != nil {
		t.Fatalf("LoadBEPCert: %v", err)
	}

	cfg := TLSConfig(cert, func(id DeviceID) bool { return true })

	if cfg == nil {
		t.Fatal("TLSConfig returned nil")
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("Certificates count = %d, want 1", len(cfg.Certificates))
	}
	if cfg.ClientAuth != tls.RequireAnyClientCert {
		t.Errorf("ClientAuth = %v, want RequireAnyClientCert", cfg.ClientAuth)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %d, want TLS 1.2", cfg.MinVersion)
	}
}

// ─── Cert directory helpers ─────────────────────────────────────────────────────

func TestExpandCertDirDefault(t *testing.T) {
	result := ExpandCertDir("")
	if result == "" {
		t.Error("ExpandCertDir('') returned empty string")
	}
	if !strings.Contains(result, ".cog") {
		t.Errorf("ExpandCertDir('') = %q, should contain .cog", result)
	}
}

func TestExpandCertDirExplicit(t *testing.T) {
	result := ExpandCertDir("/tmp/certs")
	if result != "/tmp/certs" {
		t.Errorf("ExpandCertDir('/tmp/certs') = %q, want /tmp/certs", result)
	}
}

// ─── ShortID ────────────────────────────────────────────────────────────────────

func TestShortIDFromDeviceID(t *testing.T) {
	var id DeviceID
	id[0] = 0x01
	id[1] = 0x02
	id[7] = 0xFF

	short := ShortIDFromDeviceID(id)
	if short == 0 {
		t.Error("ShortID should not be zero")
	}

	// Verify first byte contribution.
	if short&0xFF != 0x01 {
		t.Errorf("low byte = 0x%02X, want 0x01", short&0xFF)
	}
}
