package engine

import (
	"path/filepath"
	"testing"
)

func TestContainedJoin(t *testing.T) {
	base := filepath.FromSlash("/ws/.cog/mem")

	ok := map[string]string{
		"semantic/foo.cog.md": "/ws/.cog/mem/semantic/foo.cog.md",
		"foo.cog.md":          "/ws/.cog/mem/foo.cog.md",
		"a/../b.md":           "/ws/.cog/mem/b.md",       // resolves within base
		"":                    "/ws/.cog/mem",            // empty rel = base
		"/etc/passwd":         "/ws/.cog/mem/etc/passwd", // absolute input is absorbed/contained by Join, not an escape
	}
	for rel, want := range ok {
		got, err := containedJoin(base, rel)
		if err != nil {
			t.Errorf("containedJoin(base, %q) unexpected err: %v", rel, err)
			continue
		}
		if got != filepath.FromSlash(want) {
			t.Errorf("containedJoin(base, %q) = %q, want %q", rel, got, want)
		}
	}

	for _, rel := range []string{"../config/kernel.yaml", "../../etc/passwd", "a/../../escape"} {
		if got, err := containedJoin(base, rel); err == nil {
			t.Errorf("containedJoin(base, %q) = %q, want escape error", rel, got)
		}
	}
}

func TestValidPathComponent(t *testing.T) {
	for _, s := range []string{"bus_dashboard_chat", "bus_traces", "abc123", "a-b_c.d"} {
		if !validPathComponent(s) {
			t.Errorf("validPathComponent(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", ".", "..", "../config", "a/b", "a\\b", "..\\x", "x/..", string([]byte{0})} {
		if validPathComponent(s) {
			t.Errorf("validPathComponent(%q) = true, want false", s)
		}
	}
}

// TestResolveMemoryDocPath_Containment covers the cog_memory_index/cog_memory_toc
// gap the adversarial review found: absolute pass-through + unfiltered "..".
func TestResolveMemoryDocPath_Containment(t *testing.T) {
	root := t.TempDir()

	if got, err := resolveMemoryDocPath("/etc/passwd", root); err == nil {
		t.Errorf("absolute path resolved to %q, want error", got)
	}
	for _, p := range []string{"../../../etc/passwd", ".cog/mem/../../../etc/shadow"} {
		if got, err := resolveMemoryDocPath(p, root); err == nil {
			t.Errorf("resolveMemoryDocPath(%q) = %q, want escape error", p, got)
		}
	}
	got, err := resolveMemoryDocPath("semantic/foo.cog.md", root)
	if err != nil {
		t.Fatalf("legit path errored: %v", err)
	}
	if !pathWithin(filepath.Join(root, ".cog", "mem"), got) {
		t.Errorf("legit path = %q, want within .cog/mem", got)
	}
}
