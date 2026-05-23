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
