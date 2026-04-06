package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRunCoherenceAllPass(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	nucleus := makeNucleus("Test", "tester")

	report := RunCoherence(cfg, nucleus)

	if !report.Pass {
		t.Errorf("expected all layers to pass; failed results: %+v", report.Results)
	}
	if len(report.Results) != 4 {
		t.Errorf("result count = %d; want 4 (one per layer)", len(report.Results))
	}
	if report.Timestamp == "" {
		t.Error("Timestamp is empty")
	}
}

// ── Layer 1: Schema ───────────────────────────────────────────────────────

func TestValidateSchemaPass(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)

	result := validateSchema(cfg, nil)
	if !result.Pass {
		t.Errorf("schema validation failed: %+v", result.Diagnostic)
	}
	if result.Layer != "schema" {
		t.Errorf("Layer = %q; want schema", result.Layer)
	}
}

func TestValidateSchemaMissingIdentityConfig(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// No .cog/ structure at all.
	cfg := &Config{
		WorkspaceRoot: root,
		CogDir:        filepath.Join(root, ".cog"),
	}

	result := validateSchema(cfg, nil)
	if result.Pass {
		t.Error("expected schema validation to fail for missing identity config")
	}
	if result.Diagnostic == nil {
		t.Error("expected a diagnostic")
	}
	if result.Diagnostic.Severity != "error" {
		t.Errorf("Severity = %q; want error", result.Diagnostic.Severity)
	}
}

func TestValidateSchemaMissingMemDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".cog", "config"), 0755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(root, ".cog", "config", "identity.yaml"), "default_identity: test\n")
	// .cog/mem is missing.
	cfg := &Config{WorkspaceRoot: root, CogDir: filepath.Join(root, ".cog")}

	result := validateSchema(cfg, nil)
	if result.Pass {
		t.Error("expected schema validation to fail for missing mem dir")
	}
}

// ── Layer 2: Invariants ───────────────────────────────────────────────────

func TestValidateInvariantsPass(t *testing.T) {
	t.Parallel()
	nucleus := makeNucleus("Cog", "guardian")
	result := validateInvariants(nil, nucleus)
	if !result.Pass {
		t.Errorf("invariants failed: %+v", result.Diagnostic)
	}
}

func TestValidateInvariantsNilNucleus(t *testing.T) {
	t.Parallel()
	result := validateInvariants(nil, nil)
	if result.Pass {
		t.Error("expected failure for nil nucleus")
	}
	if result.Diagnostic.Rule != "I1.nucleus_loaded" {
		t.Errorf("Rule = %q; want I1.nucleus_loaded", result.Diagnostic.Rule)
	}
}

func TestValidateInvariantsEmptyName(t *testing.T) {
	t.Parallel()
	n := makeNucleus("", "some-role")
	result := validateInvariants(nil, n)
	if result.Pass {
		t.Error("expected failure for empty nucleus name")
	}
	if result.Diagnostic.Rule != "I2.nucleus_name" {
		t.Errorf("Rule = %q; want I2.nucleus_name", result.Diagnostic.Rule)
	}
}

// ── Layers 3 & 4: stubs ───────────────────────────────────────────────────

func TestValidatePolicyAlwaysPasses(t *testing.T) {
	t.Parallel()
	result := validatePolicy(nil, nil)
	if !result.Pass {
		t.Error("policy stub should always pass")
	}
	if result.Layer != "policy" {
		t.Errorf("Layer = %q; want policy", result.Layer)
	}
}

func TestValidateConsistencyNilIndexPasses(t *testing.T) {
	t.Parallel()
	// With no index supplied, consistency check is a no-op.
	result := validateConsistency(nil, nil, nil)
	if !result.Pass {
		t.Error("consistency with nil index should always pass")
	}
	if result.Layer != "consistency" {
		t.Errorf("Layer = %q; want consistency", result.Layer)
	}
}

func TestValidateConsistencyDeadRef(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)

	// Build an index with a document that has a ref pointing to a nonexistent file.
	const content = `---
title: "Broken Ref"
type: insight
refs:
  - uri: cog://mem/semantic/does-not-exist.cog.md
    rel: related
---

Body text.
`
	writeTestFile(t, filepath.Join(root, ".cog", "mem", "semantic", "broken-ref.cog.md"), content)

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}

	result := validateConsistency(cfg, nil, idx)
	if result.Pass {
		t.Error("expected consistency to fail with dead ref")
	}
	if result.Diagnostic == nil {
		t.Fatal("expected diagnostic")
	}
	if result.Diagnostic.Rule != "C1.dead_references" {
		t.Errorf("Rule = %q; want C1.dead_references", result.Diagnostic.Rule)
	}
}

func TestValidateConsistencyAllRefsAlive(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)

	// Create the referenced file so the ref resolves.
	writeTestFile(t, filepath.Join(root, ".cog", "mem", "semantic", "target.cog.md"), "# Target\n")
	const content = `---
title: "Good Ref"
type: insight
refs:
  - uri: cog://mem/semantic/target.cog.md
    rel: related
---

Body.
`
	writeTestFile(t, filepath.Join(root, ".cog", "mem", "semantic", "source.cog.md"), content)

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}

	result := validateConsistency(cfg, nil, idx)
	if !result.Pass {
		t.Errorf("expected consistency to pass; diagnostic: %+v", result.Diagnostic)
	}
}

// ── Full report with broken state ─────────────────────────────────────────

func TestRunCoherenceNilNucleus(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)

	report := RunCoherence(cfg, nil)

	if report.Pass {
		t.Error("expected report to fail with nil nucleus")
	}
	// At least invariants layer should fail.
	var invariantsFailed bool
	for _, r := range report.Results {
		if r.Layer == "invariants" && !r.Pass {
			invariantsFailed = true
		}
	}
	if !invariantsFailed {
		t.Error("expected invariants layer to fail")
	}
}
