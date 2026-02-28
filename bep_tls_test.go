package main

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

	// Key file should have restricted permissions.
	info, err := os.Stat(keyPath)
	if err == nil && info.Mode().Perm()&0077 != 0 {
		t.Errorf("key file permissions too open: %v", info.Mode().Perm())
	}
}

func TestGenerateBEPCertNoOverwrite(t *testing.T) {
	dir := t.TempDir()

	if err := GenerateBEPCert(dir); err != nil {
		t.Fatalf("first GenerateBEPCert: %v", err)
	}

	err := GenerateBEPCert(dir)
	if err == nil {
		t.Fatal("expected error on second GenerateBEPCert, got nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("expected 'already exists' error, got: %v", err)
	}
}

// ─── Load certificate ───────────────────────────────────────────────────────────

func TestLoadBEPCert(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateBEPCert(dir); err != nil {
		t.Fatalf("GenerateBEPCert: %v", err)
	}

	cert, err := LoadBEPCert(dir)
	if err != nil {
		t.Fatalf("LoadBEPCert: %v", err)
	}

	if len(cert.Certificate) == 0 {
		t.Error("loaded cert has no certificate chain")
	}
}

func TestLoadBEPCertMissing(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadBEPCert(dir)
	if err == nil {
		t.Error("expected error loading nonexistent cert")
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
	allZero := true
	for _, b := range id {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("DeviceID is all zeros")
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

	// Deriving twice should produce the same result.
	id2, err := DeviceIDFromTLSCert(&cert)
	if err != nil {
		t.Fatalf("second derivation: %v", err)
	}
	if id != id2 {
		t.Error("DeviceID not deterministic")
	}
}

func TestDeviceIDFromTLSCertEmpty(t *testing.T) {
	empty := &tls.Certificate{}
	_, err := DeviceIDFromTLSCert(empty)
	if err == nil {
		t.Error("expected error for empty cert")
	}
}

// ─── DeviceID format / parse round-trip ─────────────────────────────────────────

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

	// Should have 8 groups of 7 separated by dashes.
	parts := strings.Split(formatted, "-")
	if len(parts) != 8 {
		t.Fatalf("formatted ID has %d groups, want 8: %q", len(parts), formatted)
	}
	for i, p := range parts {
		if len(p) != 7 {
			t.Errorf("group %d has length %d, want 7: %q", i, len(p), p)
		}
	}

	// Parse back.
	parsed, err := ParseDeviceID(formatted)
	if err != nil {
		t.Fatalf("ParseDeviceID(%q): %v", formatted, err)
	}

	if parsed != id {
		t.Error("round-trip failed: parsed != original")
	}
}

func TestParseDeviceIDWithoutDashes(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateBEPCert(dir); err != nil {
		t.Fatalf("GenerateBEPCert: %v", err)
	}

	cert, err := LoadBEPCert(dir)
	if err != nil {
		t.Fatalf("LoadBEPCert: %v", err)
	}

	id, _ := DeviceIDFromTLSCert(&cert)
	formatted := FormatDeviceID(id)

	// Strip dashes.
	noDashes := strings.ReplaceAll(formatted, "-", "")
	parsed, err := ParseDeviceID(noDashes)
	if err != nil {
		t.Fatalf("ParseDeviceID without dashes: %v", err)
	}
	if parsed != id {
		t.Error("parsing without dashes failed")
	}
}

func TestParseDeviceIDInvalidLength(t *testing.T) {
	_, err := ParseDeviceID("TOOSHORT")
	if err == nil {
		t.Error("expected error for short ID")
	}
}

// ─── Luhn check character ───────────────────────────────────────────────────────

func TestLuhnBase32Deterministic(t *testing.T) {
	input := "MFZWI3DPEHVQG"
	c1 := luhnBase32(input)
	c2 := luhnBase32(input)
	if c1 != c2 {
		t.Errorf("luhn not deterministic: %c vs %c", c1, c2)
	}

	// Should be a valid base32 character.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	found := false
	for _, ch := range alphabet {
		if byte(ch) == c1 {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("luhn char %c not in base32 alphabet", c1)
	}
}

// ─── TLS config ─────────────────────────────────────────────────────────────────

func TestBEPTLSConfigBasic(t *testing.T) {
	dir := t.TempDir()
	if err := GenerateBEPCert(dir); err != nil {
		t.Fatalf("GenerateBEPCert: %v", err)
	}

	cert, err := LoadBEPCert(dir)
	if err != nil {
		t.Fatalf("LoadBEPCert: %v", err)
	}

	cfg := BEPTLSConfig(cert, func(id DeviceID) bool { return true })

	if cfg == nil {
		t.Fatal("BEPTLSConfig returned nil")
	}
	if len(cfg.Certificates) != 1 {
		t.Errorf("expected 1 certificate, got %d", len(cfg.Certificates))
	}
	if cfg.ClientAuth != tls.RequireAnyClientCert {
		t.Error("expected RequireAnyClientCert")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Error("expected MinVersion TLS 1.2")
	}
}

// ─── CertDir helpers ────────────────────────────────────────────────────────────

func TestExpandCertDirTilde(t *testing.T) {
	result := ExpandCertDir("~/.cog/etc")
	if strings.HasPrefix(result, "~") {
		t.Errorf("tilde not expanded: %q", result)
	}
	if !strings.HasSuffix(result, ".cog/etc") {
		t.Errorf("path suffix wrong: %q", result)
	}
}

func TestExpandCertDirEmpty(t *testing.T) {
	result := ExpandCertDir("")
	if result == "" {
		t.Error("empty input should return default cert dir")
	}
}

func TestExpandCertDirAbsolute(t *testing.T) {
	result := ExpandCertDir("/custom/path")
	if result != "/custom/path" {
		t.Errorf("absolute path should pass through: got %q", result)
	}
}

// ─── ShortID ────────────────────────────────────────────────────────────────────

func TestShortIDFromDeviceID(t *testing.T) {
	var id DeviceID
	id[0] = 0x01
	id[1] = 0x02
	short := ShortIDFromDeviceID(id)
	if short == 0 {
		t.Error("short ID should not be zero for non-zero DeviceID")
	}

	// Same input → same output.
	short2 := ShortIDFromDeviceID(id)
	if short != short2 {
		t.Error("ShortID not deterministic")
	}
}
