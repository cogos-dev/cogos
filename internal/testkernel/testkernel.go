// Package testkernel provides an in-process kernel boot harness for
// daemon-level integration tests.
//
// ADR-101 Phase 1: thin wrapper over engine.Boot.
// ADR-101 Phase 2: WithIsolatedRegistry injects an explicit provider list so
// tests can exercise real plan/apply without touching the global registry.
// Phase 3 will add CallTool/HTTPClient; Phase 4 will add goroutine-leak
// detection.
//
// Typical usage:
//
//	func TestFoo(t *testing.T) {
//	    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
//	    defer cancel()
//
//	    k, err := testkernel.Boot(ctx, t)
//	    if err != nil {
//	        t.Fatalf("testkernel.Boot: %v", err)
//	    }
//	    t.Cleanup(func() {
//	        if err := k.Stop(); err != nil {
//	            t.Errorf("testkernel.Stop: %v", err)
//	        }
//	    })
//
//	    // Probe the HTTP surface.
//	    resp, err := http.Get(k.Endpoint() + "/health")
//	    // ...
//	}
package testkernel

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/myrgic/cogos/internal/engine"
	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

// Option is a functional option for Boot.
type Option func(*config)

// config holds resolved options derived from Option values.
type config struct {
	// workspace is the workspace root. Defaults to a t.TempDir() path.
	workspace string

	// port is the HTTP port. Defaults to 0 (OS-assigned ephemeral port).
	port int

	// pollInterval overrides the reconcile daemon tick. 0 = use daemon default.
	pollInterval time.Duration

	// providers, when non-nil, is forwarded to engine.WithIsolatedRegistry so
	// the daemon bypasses the global registry. nil = use global registry.
	providers []reconcile.Reconcilable
}

// WithWorkspace sets the workspace root for the kernel under test.
// Defaults to a temporary directory created at Boot time.
func WithWorkspace(path string) Option {
	return func(c *config) { c.workspace = path }
}

// WithIsolatedRegistry injects an explicit set of Reconcilable providers into
// the kernel's ReconcileDaemon, bypassing the global registry.
//
// Use this in integration tests that need to exercise real plan/apply cycles
// with specific providers without interference from globally-registered stubs.
// Any provider type NOT in the supplied list will never be touched by the
// daemon for the lifetime of this kernel.
//
// This is the ADR-101 Phase 2 test-isolation mechanism.
func WithIsolatedRegistry(providers ...reconcile.Reconcilable) Option {
	return func(c *config) { c.providers = providers }
}

// Kernel is an opaque handle to an in-process kernel instance started by Boot.
// Call Stop when the test finishes; the idiomatic pattern is t.Cleanup.
type Kernel struct {
	kernel   *engine.Kernel
	endpoint string
}

// Endpoint returns the base URL of the kernel's HTTP server,
// e.g. "http://127.0.0.1:54321".
func (k *Kernel) Endpoint() string {
	return k.endpoint
}

// WorkspaceRoot returns the workspace root path used by this kernel.
func (k *Kernel) WorkspaceRoot() string {
	return k.kernel.WorkspaceRoot()
}

// ReconcileDaemon returns the kernel's ReconcileDaemon so tests can call
// Trigger and inspect State without going through the HTTP surface.
func (k *Kernel) ReconcileDaemon() *engine.ReconcileDaemon {
	return k.kernel.ReconcileDaemon()
}

// Stop cancels the kernel's context and waits for all goroutines to exit.
// Safe to call multiple times.
func (k *Kernel) Stop() error {
	return k.kernel.Stop()
}

// Boot starts a kernel in-process with the given options.
// It creates an isolated temporary workspace (unless WithWorkspace is given),
// boots the kernel via engine.Boot, and blocks until the HTTP /health endpoint
// responds 200 or ctx expires.
//
// Boot does NOT call RegisterProviders or SetProvidersWorkspace. Tests that
// need real providers should pass WithIsolatedRegistry (Phase 2) rather than
// touching the global registry.
func Boot(ctx context.Context, t *testing.T, opts ...Option) (*Kernel, error) {
	t.Helper()

	cfg := &config{port: 0}
	for _, o := range opts {
		o(cfg)
	}

	// Create a minimal workspace if none was provided.
	workspace := cfg.workspace
	if workspace == "" {
		workspace = makeMinimalWorkspace(t)
	}

	engineCfg, err := engine.LoadConfig(workspace, cfg.port)
	if err != nil {
		return nil, fmt.Errorf("testkernel.Boot: load config: %w", err)
	}
	// In test paths, always use port 0 (OS-assigned ephemeral port) unless
	// the caller explicitly set a port via WithPort.
	// LoadConfig with port==0 leaves the default 6931; we override explicitly
	// here to avoid colliding with a running production daemon.
	if cfg.port == 0 {
		engineCfg.Port = 0
	}
	// Ensure loopback bind so the test can reach the server.
	if engineCfg.BindAddr == "" {
		engineCfg.BindAddr = "127.0.0.1"
	}

	// Build engine BootOptions from testkernel config.
	var bootOpts []engine.BootOption
	if cfg.providers != nil {
		bootOpts = append(bootOpts, engine.WithIsolatedRegistry(cfg.providers...))
	}

	k, err := engine.Boot(ctx, engineCfg, bootOpts...)
	if err != nil {
		return nil, fmt.Errorf("testkernel.Boot: engine.Boot: %w", err)
	}

	// Wait for the HTTP server to be accepting connections.
	if err := waitForHealth(ctx, k.Endpoint()); err != nil {
		_ = k.Stop()
		return nil, fmt.Errorf("testkernel.Boot: health check timeout: %w", err)
	}

	return &Kernel{kernel: k, endpoint: k.Endpoint()}, nil
}

// waitForHealth polls GET <endpoint>/health until a 200 is received or ctx
// expires. Uses a 250 ms poll interval. The server goroutine is already
// running inside engine.Boot, so the window is typically < 50 ms.
func waitForHealth(ctx context.Context, endpoint string) error {
	healthURL := endpoint + "/health"
	client := &http.Client{Timeout: 500 * time.Millisecond}

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// makeMinimalWorkspace creates the directory structure required by LoadNucleus.
// Mirrors the structure created by makeWorkspace in testhelper_test.go, but
// lives in an exported package so integration tests outside internal/engine can
// use it.
func makeMinimalWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	dirs := []string{
		filepath.Join(root, ".cog", "config"),
		filepath.Join(root, ".cog", "mem", "semantic"),
		filepath.Join(root, ".cog", "ledger"),
		filepath.Join(root, "projects", "cog_lab_package", "identities"),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("testkernel: makeMinimalWorkspace: mkdir %s: %v", d, err)
		}
	}

	identity := "# Test\nRole: test-kernel\n"
	identityFile := filepath.Join(root, "projects", "cog_lab_package", "identities", "identity_test.md")
	if err := os.WriteFile(identityFile, []byte(identity), 0644); err != nil {
		t.Fatalf("testkernel: write identity: %v", err)
	}

	idCfg := "default_identity: test\nidentity_directory: projects/cog_lab_package/identities\n"
	if err := os.WriteFile(filepath.Join(root, ".cog", "config", "identity.yaml"), []byte(idCfg), 0644); err != nil {
		t.Fatalf("testkernel: write identity.yaml: %v", err)
	}

	return root
}
