// Command bep-cert is a cross-platform CLI for managing BEP transport certificates.
// It lets each node self-generate its certificate locally (private key never leaves
// the node) and print the resulting DeviceID for peer exchange in cluster.yaml.
//
// Usage:
//
//	bep-cert gen [--dir DIR] [--force]
//	bep-cert device-id [--cert CERT_PATH]
//
// The DeviceID format is Syncthing-compatible Luhn-encoded base32 groups.
package main

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/myrgic/cogos/pkg/filelock"
	bep "github.com/myrgic/cogos/pkg/substrate/bep"
)

// bepCertLockTimeout matches internal/engine's ensureBEPDeviceIdentity: this
// CLI and the kernel's automatic boot-time mint both call bep.GenerateBEPCert
// against the same default cert dir, so they must single-flight through the
// same lock file rather than racing to interleave key/cert writes.
const bepCertLockTimeout = 5 * time.Second

const usage = `bep-cert — BEP transport certificate tool

SUBCOMMANDS
  gen         Generate a BEP cert+key into a directory
  device-id   Print the DeviceID of an existing cert

gen [--dir DIR] [--force]
  Generates bep-cert.pem and bep-key.pem into DIR (default ~/.cog/etc/).
  Refuses to overwrite an existing bep-cert.pem unless --force is given.
  On success, prints the DeviceID and the paths of the generated files.

  Flags:
    --dir DIR    Target directory (default ~/.cog/etc/)
    --force      Overwrite existing cert+key if present

device-id [--cert CERT_PATH]
  Loads a cert PEM file and prints its DeviceID using the same derivation
  the BEP transport uses, so the ID matches what peers pin.

  Flags:
    --cert PATH  Path to bep-cert.pem (default ~/.cog/etc/bep-cert.pem)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	switch os.Args[1] {
	case "gen":
		if err := runGen(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "bep-cert gen: %v\n", err)
			os.Exit(1)
		}
	case "device-id":
		if err := runDeviceID(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "bep-cert device-id: %v\n", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n\n%s", os.Args[1], usage)
		os.Exit(1)
	}
}

// runGen handles the `gen` subcommand.
func runGen(args []string) error {
	dir := ""
	force := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dir":
			if i+1 >= len(args) {
				return fmt.Errorf("--dir requires an argument")
			}
			i++
			dir = args[i]
		case "--force":
			force = true
		default:
			return fmt.Errorf("unknown flag %q", args[i])
		}
	}

	certDir := bep.ExpandCertDir(dir)
	certPath := filepath.Join(certDir, "bep-cert.pem")
	keyPath := filepath.Join(certDir, "bep-key.pem")

	if err := os.MkdirAll(certDir, 0700); err != nil {
		return fmt.Errorf("create cert dir: %w", err)
	}

	// Single-flight against internal/engine's ensureBEPDeviceIdentity, which
	// can run unattended at any node's first boot: without this lock, this
	// CLI's remove-then-GenerateBEPCert sequence below could interleave with
	// the kernel's own key/cert writes and leave a mismatched pair that
	// passes os.Stat but doesn't load as a valid identity.
	lock, err := filelock.Acquire(filepath.Join(certDir, "bep-cert.lock"), bepCertLockTimeout)
	if err != nil {
		return fmt.Errorf("acquire BEP cert lock: %w", err)
	}
	defer lock.Release()

	// Refuse to overwrite unless --force.
	if _, err := os.Stat(certPath); err == nil && !force {
		return fmt.Errorf("certificate already exists at %s\n       use --force to overwrite", certPath)
	}

	// If force, remove existing files so GenerateBEPCert (which uses O_EXCL) can proceed.
	if force {
		_ = os.Remove(certPath)
		_ = os.Remove(keyPath)
	}

	if err := bep.GenerateBEPCert(certDir); err != nil {
		return err
	}

	// Load to derive DeviceID.
	tlsCert, err := bep.LoadBEPCert(certDir)
	if err != nil {
		return fmt.Errorf("load cert for DeviceID: %w", err)
	}
	id, err := bep.DeviceIDFromTLSCert(&tlsCert)
	if err != nil {
		return fmt.Errorf("derive DeviceID: %w", err)
	}

	fmt.Printf("DeviceID:  %s\n", bep.FormatDeviceID(id))
	fmt.Printf("cert:      %s\n", certPath)
	fmt.Printf("key:       %s\n", keyPath)
	return nil
}

// runDeviceID handles the `device-id` subcommand.
func runDeviceID(args []string) error {
	certPath := ""

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--cert":
			if i+1 >= len(args) {
				return fmt.Errorf("--cert requires an argument")
			}
			i++
			certPath = args[i]
		default:
			return fmt.Errorf("unknown flag %q", args[i])
		}
	}

	// Default cert path: ~/.cog/etc/bep-cert.pem
	if certPath == "" {
		certDir := bep.ExpandCertDir("")
		certPath = filepath.Join(certDir, "bep-cert.pem")
	}

	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return fmt.Errorf("read cert %s: %w", certPath, err)
	}

	block, _ := pem.Decode(certPEM)
	if block == nil {
		return fmt.Errorf("no PEM block found in %s", certPath)
	}

	x509Cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse certificate: %w", err)
	}

	// Use the same derivation as the BEP transport: DeviceIDFromCert (SHA-256 of DER).
	id := bep.DeviceIDFromCert(x509Cert)

	fmt.Printf("DeviceID:  %s\n", bep.FormatDeviceID(id))
	fmt.Printf("cert:      %s\n", certPath)
	return nil
}
