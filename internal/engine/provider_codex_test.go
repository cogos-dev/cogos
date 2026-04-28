package engine

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

type fakeFileInfo struct {
	name string
	mode os.FileMode
	dir  bool
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return 1 }
func (f fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.dir }
func (f fakeFileInfo) Sys() any           { return nil }

func resetCodexResolverForTest(t *testing.T) {
	t.Helper()

	oldLookPath := codexLookPath
	oldStat := codexStat
	oldGOOS := codexGOOS
	t.Cleanup(func() {
		codexLookPath = oldLookPath
		codexStat = oldStat
		codexGOOS = oldGOOS
	})
}

func TestCodexResolveBinaryPrefersPath(t *testing.T) {
	resetCodexResolverForTest(t)

	codexGOOS = "darwin"
	codexLookPath = func(file string) (string, error) {
		if file != "codex" {
			t.Fatalf("LookPath called with %q, want codex", file)
		}
		return "/usr/local/bin/codex", nil
	}
	codexStat = func(path string) (os.FileInfo, error) {
		t.Fatalf("stat should not be called when PATH resolves, got %q", path)
		return nil, errors.New("unreachable")
	}

	p := NewCodexProvider("codex", ProviderConfig{})
	path, err := p.resolveBinary()
	if err != nil {
		t.Fatalf("resolveBinary returned error: %v", err)
	}
	if path != "/usr/local/bin/codex" {
		t.Fatalf("resolveBinary returned %q, want PATH result", path)
	}
}

func TestCodexResolveBinaryFallsBackToAppBundleOnDarwin(t *testing.T) {
	resetCodexResolverForTest(t)

	codexGOOS = "darwin"
	codexLookPath = func(file string) (string, error) {
		return "", errors.New("not found")
	}
	codexStat = func(path string) (os.FileInfo, error) {
		if path == codexAppBundleBinary {
			return fakeFileInfo{name: "codex", mode: 0o755}, nil
		}
		return nil, os.ErrNotExist
	}

	p := NewCodexProvider("codex", ProviderConfig{})
	path, err := p.resolveBinary()
	if err != nil {
		t.Fatalf("resolveBinary returned error: %v", err)
	}
	if path != codexAppBundleBinary {
		t.Fatalf("resolveBinary returned %q, want app bundle path", path)
	}
	if !p.Available(context.Background()) {
		t.Fatal("Available should use the app bundle fallback resolver")
	}
}

func TestCodexResolveBinarySkipsNonExecutableAppBundlePath(t *testing.T) {
	resetCodexResolverForTest(t)

	codexGOOS = "darwin"
	codexLookPath = func(file string) (string, error) {
		return "", errors.New("not found")
	}
	codexStat = func(path string) (os.FileInfo, error) {
		if path == codexAppBundleBinary {
			return fakeFileInfo{name: "codex", mode: 0o644}, nil
		}
		return nil, os.ErrNotExist
	}

	p := NewCodexProvider("codex", ProviderConfig{})
	if _, err := p.resolveBinary(); err == nil {
		t.Fatal("resolveBinary should fail when the app bundle path is not executable")
	}
	if p.Available(context.Background()) {
		t.Fatal("Available should be false when the app bundle path is not executable")
	}
}

func TestCodexResolveBinaryDoesNotFallbackForExplicitEndpoint(t *testing.T) {
	resetCodexResolverForTest(t)

	codexGOOS = "darwin"
	codexLookPath = func(file string) (string, error) {
		if file != "codex" {
			t.Fatalf("LookPath called with %q, want explicit endpoint", file)
		}
		return "", errors.New("not found")
	}
	codexStat = func(path string) (os.FileInfo, error) {
		t.Fatalf("stat should not be called for explicit endpoint, got %q", path)
		return nil, errors.New("unreachable")
	}

	p := NewCodexProvider("codex", ProviderConfig{Endpoint: "codex"})
	if _, err := p.resolveBinary(); err == nil {
		t.Fatal("resolveBinary should fail for an explicit endpoint that is not on PATH")
	}
	if p.Available(context.Background()) {
		t.Fatal("Available should be false for an unresolved explicit endpoint")
	}
}

func TestCodexResolveBinaryDoesNotFallbackForExplicitPath(t *testing.T) {
	resetCodexResolverForTest(t)

	codexGOOS = "darwin"
	codexLookPath = func(file string) (string, error) {
		if file != "/opt/codex/bin/codex" {
			t.Fatalf("LookPath called with %q, want explicit path", file)
		}
		return "", errors.New("not found")
	}
	codexStat = func(path string) (os.FileInfo, error) {
		t.Fatalf("stat should not be called for explicit path, got %q", path)
		return nil, errors.New("unreachable")
	}

	p := NewCodexProvider("codex", ProviderConfig{Endpoint: "/opt/codex/bin/codex"})
	if _, err := p.resolveBinary(); err == nil {
		t.Fatal("resolveBinary should fail for an explicit path that does not exist")
	}
}
