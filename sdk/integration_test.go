//go:build integration

package sdk

import (
	"os"
	"path/filepath"
	"testing"
)

func TestConnect(t *testing.T) {
	// Use the actual workspace
	workspaceRoot := "../.."
	absRoot, _ := filepath.Abs(workspaceRoot)

	// Check that .cog exists
	if _, err := os.Stat(filepath.Join(absRoot, ".cog")); os.IsNotExist(err) {
		t.Skip("No .cog directory found - skipping integration test")
	}

	kernel, err := Connect(workspaceRoot)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer kernel.Close()

	if kernel.Root() != absRoot {
		t.Errorf("Root = %q, want %q", kernel.Root(), absRoot)
	}
}

func TestResolveSRC(t *testing.T) {
	workspaceRoot := "../.."
	if _, err := os.Stat(filepath.Join(workspaceRoot, ".cog")); os.IsNotExist(err) {
		t.Skip("No .cog directory found - skipping integration test")
	}

	kernel, err := Connect(workspaceRoot)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer kernel.Close()

	// Resolve SRC constants
	resource, err := kernel.Resolve("cog://src")
	if err != nil {
		t.Fatalf("Resolve(cog://src) failed: %v", err)
	}

	if resource.ContentType != ContentTypeJSON {
		t.Errorf("ContentType = %q, want %q", resource.ContentType, ContentTypeJSON)
	}

	// Project into struct
	var src SRCConstants
	if err := kernel.Project("cog://src", &src); err != nil {
		t.Fatalf("Project failed: %v", err)
	}

	if src.VarianceRatio != 6 {
		t.Errorf("VarianceRatio = %d, want 6", src.VarianceRatio)
	}
}

func TestResolveIdentity(t *testing.T) {
	workspaceRoot := "../.."
	if _, err := os.Stat(filepath.Join(workspaceRoot, ".cog")); os.IsNotExist(err) {
		t.Skip("No .cog directory found - skipping integration test")
	}

	kernel, err := Connect(workspaceRoot)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer kernel.Close()

	resource, err := kernel.Resolve("cog://identity")
	if err != nil {
		t.Fatalf("Resolve(cog://identity) failed: %v", err)
	}

	if resource.ContentType != ContentTypeCogdoc {
		t.Errorf("ContentType = %q, want %q", resource.ContentType, ContentTypeCogdoc)
	}

	if len(resource.Content) == 0 {
		t.Error("Content should not be empty")
	}
}

func TestResolveCoherence(t *testing.T) {
	workspaceRoot := "../.."
	if _, err := os.Stat(filepath.Join(workspaceRoot, ".cog")); os.IsNotExist(err) {
		t.Skip("No .cog directory found - skipping integration test")
	}

	kernel, err := Connect(workspaceRoot)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer kernel.Close()

	resource, err := kernel.Resolve("cog://coherence")
	if err != nil {
		t.Fatalf("Resolve(cog://coherence) failed: %v", err)
	}

	if resource.ContentType != ContentTypeJSON {
		t.Errorf("ContentType = %q, want %q", resource.ContentType, ContentTypeJSON)
	}
}

func TestResolveMemorySectors(t *testing.T) {
	workspaceRoot := "../.."
	if _, err := os.Stat(filepath.Join(workspaceRoot, ".cog")); os.IsNotExist(err) {
		t.Skip("No .cog directory found - skipping integration test")
	}

	kernel, err := Connect(workspaceRoot)
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer kernel.Close()

	resource, err := kernel.Resolve("cog://memory")
	if err != nil {
		t.Fatalf("Resolve(cog://memory) failed: %v", err)
	}

	if !resource.IsCollection() {
		t.Error("cog://memory should return a collection")
	}

	if len(resource.Children) == 0 {
		t.Error("Should have at least one memory sector")
	}
}

func TestFindWorkspaceRoot(t *testing.T) {
	// Start from SDK directory and find workspace
	root, err := FindWorkspaceRoot(".")
	if err != nil {
		t.Fatalf("FindWorkspaceRoot failed: %v", err)
	}

	// Verify .cog exists at found root
	cogDir := filepath.Join(root, ".cog")
	if _, err := os.Stat(cogDir); os.IsNotExist(err) {
		t.Errorf("No .cog at found root %q", root)
	}
}
