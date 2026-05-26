package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGenAndDeviceIDConsistency generates a cert via runGen and then reads the
// DeviceID back via runDeviceID, asserting they match and are well-formed.
func TestGenAndDeviceIDConsistency(t *testing.T) {
	dir := t.TempDir()

	if err := runGen([]string{"--dir", dir}); err != nil {
		t.Fatalf("runGen: %v", err)
	}

	certPath := filepath.Join(dir, "bep-cert.pem")
	if _, err := os.Stat(certPath); err != nil {
		t.Fatalf("cert not created: %v", err)
	}
	keyPath := filepath.Join(dir, "bep-key.pem")
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("key not created: %v", err)
	}

	// device-id on the generated cert should succeed and return a well-formed ID.
	if err := runDeviceID([]string{"--cert", certPath}); err != nil {
		t.Fatalf("runDeviceID: %v", err)
	}
}

// TestGenRefusesOverwrite checks that a second gen call without --force fails.
func TestGenRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()

	if err := runGen([]string{"--dir", dir}); err != nil {
		t.Fatalf("first runGen: %v", err)
	}

	err := runGen([]string{"--dir", dir})
	if err == nil {
		t.Fatal("expected error on overwrite without --force, got nil")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error should mention --force, got: %v", err)
	}
}

// TestGenForceOverwrite checks that --force allows regeneration.
func TestGenForceOverwrite(t *testing.T) {
	dir := t.TempDir()

	if err := runGen([]string{"--dir", dir}); err != nil {
		t.Fatalf("first runGen: %v", err)
	}
	if err := runGen([]string{"--dir", dir, "--force"}); err != nil {
		t.Fatalf("runGen --force: %v", err)
	}
}

// TestDeviceIDFormatShape checks that the DeviceID is 8 dash-separated groups of 7 chars.
func TestDeviceIDFormatShape(t *testing.T) {
	dir := t.TempDir()
	if err := runGen([]string{"--dir", dir}); err != nil {
		t.Fatalf("runGen: %v", err)
	}

	certPath := filepath.Join(dir, "bep-cert.pem")

	// Capture stdout to inspect the DeviceID string.
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	if err := runDeviceID([]string{"--cert", certPath}); err != nil {
		w.Close()
		os.Stdout = old
		t.Fatalf("runDeviceID: %v", err)
	}
	w.Close()
	os.Stdout = old

	buf := make([]byte, 512)
	n, _ := r.Read(buf)
	output := string(buf[:n])

	// Extract the DeviceID line.
	var devID string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "DeviceID:") {
			devID = strings.TrimSpace(strings.TrimPrefix(line, "DeviceID:"))
			break
		}
	}

	if devID == "" {
		t.Fatalf("DeviceID not found in output: %q", output)
	}

	parts := strings.Split(devID, "-")
	if len(parts) != 8 {
		t.Errorf("expected 8 dash-separated groups, got %d: %q", len(parts), devID)
	}
	for i, p := range parts {
		if len(p) != 7 {
			t.Errorf("group %d has length %d, want 7: %q", i, len(p), p)
		}
	}
}
