// bep_tls.go — TLS certificate generation, DeviceID derivation, and mutual TLS
// configuration for BEP transport. DeviceIDs are SHA-256 of DER certificate,
// formatted as Luhn-encoded base32 groups (Syncthing-compatible).

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DeviceID is the SHA-256 hash of a node's DER-encoded certificate.
type DeviceID [32]byte

// ─── Certificate generation ─────────────────────────────────────────────────────

// GenerateBEPCert creates an ECDSA P-256 TLS certificate for BEP transport.
// Writes cert and key to certDir/bep-cert.pem and certDir/bep-key.pem.
// Returns an error if cert files already exist.
func GenerateBEPCert(certDir string) error {
	certPath := filepath.Join(certDir, "bep-cert.pem")
	keyPath := filepath.Join(certDir, "bep-key.pem")

	// Don't overwrite existing certs.
	if _, err := os.Stat(certPath); err == nil {
		return fmt.Errorf("certificate already exists at %s", certPath)
	}

	if err := os.MkdirAll(certDir, 0700); err != nil {
		return fmt.Errorf("create cert dir: %w", err)
	}

	// Generate ECDSA P-256 key.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}

	// Self-signed certificate, 20-year validity.
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return fmt.Errorf("generate serial: %w", err)
	}

	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "cogos-bep"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(20 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return fmt.Errorf("create certificate: %w", err)
	}

	// Write cert PEM.
	certFile, err := os.OpenFile(certPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return fmt.Errorf("create cert file: %w", err)
	}
	defer certFile.Close()
	if err := pem.Encode(certFile, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		return fmt.Errorf("encode cert: %w", err)
	}

	// Write key PEM.
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	keyFile, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create key file: %w", err)
	}
	defer keyFile.Close()
	if err := pem.Encode(keyFile, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}); err != nil {
		return fmt.Errorf("encode key: %w", err)
	}

	return nil
}

// LoadBEPCert loads the BEP TLS certificate and key from certDir.
func LoadBEPCert(certDir string) (tls.Certificate, error) {
	certPath := filepath.Join(certDir, "bep-cert.pem")
	keyPath := filepath.Join(certDir, "bep-key.pem")
	return tls.LoadX509KeyPair(certPath, keyPath)
}

// ─── DeviceID derivation ────────────────────────────────────────────────────────

// DeviceIDFromCert derives a DeviceID from a certificate's raw DER bytes.
func DeviceIDFromCert(cert *x509.Certificate) DeviceID {
	return sha256.Sum256(cert.Raw)
}

// DeviceIDFromTLSCert derives a DeviceID from a tls.Certificate.
func DeviceIDFromTLSCert(cert *tls.Certificate) (DeviceID, error) {
	if len(cert.Certificate) == 0 {
		return DeviceID{}, fmt.Errorf("no certificate in chain")
	}
	x509Cert, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return DeviceID{}, fmt.Errorf("parse certificate: %w", err)
	}
	return DeviceIDFromCert(x509Cert), nil
}

// ─── DeviceID formatting (Syncthing-compatible Luhn base32) ─────────────────────

var base32Enc = base32.StdEncoding.WithPadding(base32.NoPadding)

// FormatDeviceID formats a DeviceID as Luhn-encoded base32 groups.
// Output: 8 groups of 7 characters separated by dashes (56 chars + 7 dashes).
// Example: MFZWI3D-BORSXI-LTHKMQD-ATKCJH-APEKEK-NOFLIF-G5SKQB-DCEKST
func FormatDeviceID(id DeviceID) string {
	// Base32 encode the 32 bytes → 52 characters.
	raw := base32Enc.EncodeToString(id[:])

	// Insert Luhn check characters after every 13 base32 chars → 4 groups of 13+1.
	var withLuhn strings.Builder
	for i := 0; i < len(raw); i += 13 {
		end := i + 13
		if end > len(raw) {
			end = len(raw)
		}
		chunk := raw[i:end]
		withLuhn.WriteString(chunk)
		if end <= len(raw) {
			withLuhn.WriteByte(luhnBase32(chunk))
		}
	}

	// Split into 8 groups of 7 characters, joined by dashes.
	full := withLuhn.String()
	var groups []string
	for i := 0; i < len(full); i += 7 {
		end := i + 7
		if end > len(full) {
			end = len(full)
		}
		groups = append(groups, full[i:end])
	}
	return strings.Join(groups, "-")
}

// ParseDeviceID parses a formatted DeviceID string back to bytes.
// Accepts with or without dashes, strips Luhn check characters.
func ParseDeviceID(s string) (DeviceID, error) {
	var id DeviceID

	// Remove dashes and spaces.
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	s = strings.ToUpper(s)

	if len(s) != 56 {
		return id, fmt.Errorf("invalid device ID length: got %d, want 56", len(s))
	}

	// Strip Luhn check characters (positions 13, 27, 41, 55 — 0-indexed).
	var raw strings.Builder
	for i := 0; i < len(s); i++ {
		pos := i % 14
		if pos == 13 {
			continue // skip Luhn check char
		}
		raw.WriteByte(s[i])
	}

	decoded, err := base32Enc.DecodeString(raw.String())
	if err != nil {
		return id, fmt.Errorf("base32 decode: %w", err)
	}
	if len(decoded) != 32 {
		return id, fmt.Errorf("decoded length %d, want 32", len(decoded))
	}
	copy(id[:], decoded)
	return id, nil
}

// luhnBase32 computes a Luhn mod 32 check character for a base32 string.
func luhnBase32(s string) byte {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	factor := 1
	sum := 0
	n := 32

	for i := len(s) - 1; i >= 0; i-- {
		codepoint := strings.IndexByte(alphabet, s[i])
		if codepoint < 0 {
			codepoint = 0
		}
		addend := factor * codepoint
		factor = 3 - factor // alternates 1, 2, 1, 2, ...
		addend = addend/n + addend%n
		sum += addend
	}
	remainder := sum % n
	checkCodepoint := (n - remainder) % n
	return alphabet[checkCodepoint]
}

// ─── TLS configuration ─────────────────────────────────────────────────────────

// BEPTLSConfig creates a mutual TLS config for BEP connections.
// The config requires client certificates and verifies peer DeviceIDs
// against the trusted set.
func BEPTLSConfig(cert tls.Certificate, verifyPeer func(DeviceID) bool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAnyClientCert,
		// We do our own verification via DeviceID, not CA chains.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("no peer certificate")
			}
			peerCert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("parse peer cert: %w", err)
			}
			peerID := DeviceIDFromCert(peerCert)
			if verifyPeer != nil && !verifyPeer(peerID) {
				return fmt.Errorf("untrusted peer: %s", FormatDeviceID(peerID))
			}
			return nil
		},
		MinVersion: tls.VersionTLS12,
	}
}

// CertDir returns the default certificate directory (~/.cog/etc).
func CertDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".cog", "etc")
	}
	return filepath.Join(home, ".cog", "etc")
}

// ExpandCertDir resolves certDir, handling ~ prefix and default.
func ExpandCertDir(certDir string) string {
	if certDir == "" {
		return CertDir()
	}
	if strings.HasPrefix(certDir, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, certDir[2:])
		}
	}
	return certDir
}
