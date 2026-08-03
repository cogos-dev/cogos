// ontology_test.go — table-driven tests for L0/L1/L2 parse and enforcement.
//
// Covers:
//   1. L0 grammar version validation (accept known, reject unknown)
//   2. L1 required field validation
//   3. L2 mapping parse
//   4. LoadOntologyDir from a temp fixture dir
//   5. OntologyVersionCheck (id+major mismatch)
//   6. ComponentClass lookup (known/unknown)
//   7. majorVersion helper
//   8. component= URI param now accepted (not reserved error)
//   9. ontology= URI param now accepted (not reserved error)
package conversations

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── L0/L1 parse tests ───────────────────────────────────────────────────────

func TestParseL1Ontology_ValidDocument(t *testing.T) {
	yaml := `
ontology: cogos.ontology/v1
id: cogos.conversations
version: 1.0.0
entities:
  session:
    description: One conversation stream.
    keys: [source, session_id]
components:
  session.turn:
    description: A conversational turn.
    fields: { role: "string", text: string }
    required: [role, text]
relations:
  in_session:
    description: Component belongs to session.
    from: "*"
    to: session
    cardinality: "N:1"
`
	doc, err := ParseL1Ontology([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseL1Ontology: unexpected error: %v", err)
	}
	if doc.ID != "cogos.conversations" {
		t.Errorf("id: want cogos.conversations, got %q", doc.ID)
	}
	if doc.Version != "1.0.0" {
		t.Errorf("version: want 1.0.0, got %q", doc.Version)
	}
	if _, ok := doc.Components["session.turn"]; !ok {
		t.Error("expected session.turn component")
	}
}

func TestParseL1Ontology_UnknownL0Grammar(t *testing.T) {
	cases := []struct {
		name    string
		ontLine string
	}{
		{"wrong version", `ontology: cogos.ontology/v2`},
		{"unknown grammar", `ontology: some.other.grammar/v1`},
		{"missing ontology key", `id: foo\nversion: 1.0.0`},
		{"empty ontology", `ontology: ""\nid: foo\nversion: 1.0.0`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := tc.ontLine + "\nid: test\nversion: 1.0.0\n"
			_, err := ParseL1Ontology([]byte(yaml))
			if err == nil {
				t.Errorf("expected error for L0 grammar %q, got nil", tc.ontLine)
			}
		})
	}
}

func TestParseL1Ontology_MissingRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		yaml string
	}{
		{
			"missing id",
			"ontology: cogos.ontology/v1\nversion: 1.0.0\n",
		},
		{
			"missing version",
			"ontology: cogos.ontology/v1\nid: cogos.conversations\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseL1Ontology([]byte(tc.yaml))
			if err == nil {
				t.Errorf("%s: expected error, got nil", tc.name)
			}
		})
	}
}

func TestParseL1Ontology_InvalidYAML(t *testing.T) {
	_, err := ParseL1Ontology([]byte("{not valid yaml: ["))
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}

// ─── L2 mapping parse tests ──────────────────────────────────────────────────

func TestParseMappingDoc_Valid(t *testing.T) {
	yaml := `
mapping:
  id: claude-code-jsonl
  version: 1.0.0
  source: claude-code-jsonl
  ontology: "cogos.conversations@^1"
rules:
  - id: user-text-to-turn
    emit:
      component: session.turn
unmapped:
  - id: tool-result-blocks
    target_l1_class: tool.result
`
	doc, err := ParseMappingDoc([]byte(yaml))
	if err != nil {
		t.Fatalf("ParseMappingDoc: %v", err)
	}
	if doc.Mapping.ID != "claude-code-jsonl" {
		t.Errorf("mapping id: want claude-code-jsonl, got %q", doc.Mapping.ID)
	}
	if len(doc.Rules) != 1 {
		t.Errorf("rules: want 1, got %d", len(doc.Rules))
	}
	if len(doc.Unmapped) != 1 {
		t.Errorf("unmapped: want 1, got %d", len(doc.Unmapped))
	}
}

func TestParseMappingDoc_MissingID(t *testing.T) {
	yaml := `
mapping:
  version: 1.0.0
  ontology: "cogos.conversations@^1"
`
	_, err := ParseMappingDoc([]byte(yaml))
	if err == nil {
		t.Error("expected error for missing mapping.id, got nil")
	}
}

func TestParseMappingDoc_MissingOntology(t *testing.T) {
	yaml := `
mapping:
  id: test-mapping
  version: 1.0.0
`
	_, err := ParseMappingDoc([]byte(yaml))
	if err == nil {
		t.Error("expected error for missing mapping.ontology, got nil")
	}
}

// ─── LoadOntologyDir tests ────────────────────────────────────────────────────

// writeOntologyFixtures sets up a minimal ontology dir with an L1 and two
// L2 mappings in the temp directory.
func writeOntologyFixtures(t *testing.T, dir string) {
	t.Helper()

	l1 := `ontology: cogos.ontology/v1
id: cogos.conversations
version: 1.0.0
entities:
  session:
    description: One conversation stream.
    keys: [source, session_id]
components:
  session.turn:
    description: A conversational turn.
    fields: { role: string, text: string, timestamp: rfc3339 }
    required: [role, text, timestamp]
    relations: { in_session: session }
  tool.call:
    description: A tool invocation.
    fields: { name: string, timestamp: rfc3339 }
    required: [name, timestamp]
relations:
  in_session:
    description: Component belongs to session.
    from: "*"
    to: session
    cardinality: "N:1"
`
	if err := os.WriteFile(filepath.Join(dir, "cogos.conversations.v1.yaml"), []byte(l1), 0o644); err != nil {
		t.Fatalf("write L1: %v", err)
	}

	// L0 meta-schema (should be skipped, not parsed as L1).
	l0 := `meta_schema: cogos.ontology/v1
version: 1.0.0
grammar:
  entity:
    fields:
      description: string
`
	if err := os.WriteFile(filepath.Join(dir, "cogos.ontology.v1.yaml"), []byte(l0), 0o644); err != nil {
		t.Fatalf("write L0: %v", err)
	}

	mappingsDir := filepath.Join(dir, "mappings")
	if err := os.Mkdir(mappingsDir, 0o755); err != nil {
		t.Fatalf("mkdir mappings: %v", err)
	}

	cc := `mapping:
  id: claude-code-jsonl
  version: 1.0.0
  source: claude-code-jsonl
  ontology: "cogos.conversations@^1"
rules:
  - id: user-text
    emit:
      component: session.turn
`
	if err := os.WriteFile(filepath.Join(mappingsDir, "claude-code-jsonl.v1.yaml"), []byte(cc), 0o644); err != nil {
		t.Fatalf("write CC mapping: %v", err)
	}

	hermes := `mapping:
  id: hermes-statedb.v1
  version: 1.0.0
  sources:
    - hermes-node-a
    - hermes-cog
  ontology: "cogos.conversations@^1"
rules:
  - id: text-user
    target_class: session.turn
`
	if err := os.WriteFile(filepath.Join(mappingsDir, "hermes-statedb.v1.yaml"), []byte(hermes), 0o644); err != nil {
		t.Fatalf("write Hermes mapping: %v", err)
	}
}

func TestLoadOntologyDir_LoadsL1AndL2(t *testing.T) {
	dir := t.TempDir()
	writeOntologyFixtures(t, dir)

	lo, err := LoadOntologyDir(dir)
	if err != nil {
		t.Fatalf("LoadOntologyDir: %v", err)
	}
	if lo.L1 == nil {
		t.Fatal("expected L1 to be loaded")
	}
	if lo.L1.ID != "cogos.conversations" {
		t.Errorf("L1.ID: want cogos.conversations, got %q", lo.L1.ID)
	}
	if lo.OntologyRef != "cogos.conversations@1.0.0" {
		t.Errorf("OntologyRef: want cogos.conversations@1.0.0, got %q", lo.OntologyRef)
	}

	// L2 should have 3 source entries: claude-code-jsonl, hermes-node-a, hermes-cog
	if _, ok := lo.L2["claude-code-jsonl"]; !ok {
		t.Error("expected claude-code-jsonl in L2")
	}
	if _, ok := lo.L2["hermes-node-a"]; !ok {
		t.Error("expected hermes-node-a in L2")
	}
	if _, ok := lo.L2["hermes-cog"]; !ok {
		t.Error("expected hermes-cog in L2")
	}
	// MappedComponents should include session.turn
	if _, ok := lo.MappedComponents["session.turn"]; !ok {
		t.Error("expected session.turn in MappedComponents")
	}
}

func TestLoadOntologyDir_MissingDir_NoError(t *testing.T) {
	lo, err := LoadOntologyDir("/nonexistent/path/that/does/not/exist")
	if err != nil {
		t.Errorf("expected nil error for missing dir, got %v", err)
	}
	if lo == nil {
		t.Fatal("expected non-nil LoadedOntology even for missing dir")
	}
	if lo.L1 != nil {
		t.Error("expected nil L1 for missing dir")
	}
}

func TestLoadOntologyDir_InvalidL1_ReturnsError(t *testing.T) {
	dir := t.TempDir()
	// Write an L1 file with an unknown L0 grammar version.
	bad := `ontology: cogos.ontology/v99
id: bad
version: 1.0.0
`
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte(bad), 0o644); err != nil {
		t.Fatalf("write bad L1: %v", err)
	}
	_, err := LoadOntologyDir(dir)
	if err == nil {
		t.Error("expected error for invalid L1 grammar, got nil")
	}
}

// ─── SourceFingerprint tests ──────────────────────────────────────────────────

// TestSourceFingerprint_StableAcrossIdenticalLoads asserts that two separate
// LoadOntologyDir calls over byte-identical ontology/mapping files produce the
// same fingerprint for the same source — the baseline correctness property
// the coverage cache relies on to distinguish "unchanged" from "edited".
func TestSourceFingerprint_StableAcrossIdenticalLoads(t *testing.T) {
	dir := t.TempDir()
	writeOntologyFixtures(t, dir)

	lo1, err := LoadOntologyDir(dir)
	if err != nil {
		t.Fatalf("LoadOntologyDir (1st load): %v", err)
	}
	lo2, err := LoadOntologyDir(dir)
	if err != nil {
		t.Fatalf("LoadOntologyDir (2nd load): %v", err)
	}

	for _, source := range []string{"claude-code-jsonl", "hermes-node-a", "hermes-cog"} {
		fp1 := lo1.SourceFingerprint(source)
		fp2 := lo2.SourceFingerprint(source)
		if fp1 == "" {
			t.Errorf("SourceFingerprint(%q): empty fingerprint from a loaded ontology", source)
		}
		if fp1 != fp2 {
			t.Errorf("SourceFingerprint(%q) not stable across identical loads: %q vs %q", source, fp1, fp2)
		}
	}
}

// TestSourceFingerprint_L1Edit_ChangesFingerprint asserts that editing the L1
// ontology file in place — without touching any L2 mapping or bumping the
// declared version — changes SourceFingerprint for every source, since every
// source's fingerprint incorporates l1Hash.
func TestSourceFingerprint_L1Edit_ChangesFingerprint(t *testing.T) {
	dir := t.TempDir()
	writeOntologyFixtures(t, dir)

	before, err := LoadOntologyDir(dir)
	if err != nil {
		t.Fatalf("LoadOntologyDir (before): %v", err)
	}
	fpBefore := before.SourceFingerprint("claude-code-jsonl")

	// In-place edit to the L1 file: add a field to an existing component
	// without bumping `version:`. This is exactly the class of edit
	// isIngestDrift cannot see (it never reads the ontology dir at all).
	l1Path := filepath.Join(dir, "cogos.conversations.v1.yaml")
	data, err := os.ReadFile(l1Path)
	if err != nil {
		t.Fatalf("read L1: %v", err)
	}
	edited := strings.Replace(string(data), "tool.call:\n    description: A tool invocation.",
		"tool.call:\n    description: A tool invocation (edited).", 1)
	if edited == string(data) {
		t.Fatal("test fixture drifted: expected replacement string not found in L1 fixture")
	}
	if err := os.WriteFile(l1Path, []byte(edited), 0o644); err != nil {
		t.Fatalf("rewrite L1: %v", err)
	}

	after, err := LoadOntologyDir(dir)
	if err != nil {
		t.Fatalf("LoadOntologyDir (after): %v", err)
	}
	fpAfter := after.SourceFingerprint("claude-code-jsonl")

	if fpBefore == fpAfter {
		t.Errorf("SourceFingerprint unchanged after in-place L1 edit: %q", fpBefore)
	}
}

// TestSourceFingerprint_L2Edit_ScopedToServedSources asserts that editing one
// L2 mapping file changes SourceFingerprint only for the source(s) that
// mapping serves, leaving sources served by a different, untouched mapping
// file unaffected. The fixture's hermes-statedb.v1.yaml serves both
// hermes-node-a and hermes-cog (a single file, multiple declared sources) and
// is independent of claude-code-jsonl.v1.yaml.
func TestSourceFingerprint_L2Edit_ScopedToServedSources(t *testing.T) {
	dir := t.TempDir()
	writeOntologyFixtures(t, dir)

	before, err := LoadOntologyDir(dir)
	if err != nil {
		t.Fatalf("LoadOntologyDir (before): %v", err)
	}
	fpHermesNodeABefore := before.SourceFingerprint("hermes-node-a")
	fpHermesCogBefore := before.SourceFingerprint("hermes-cog")
	fpCCBefore := before.SourceFingerprint("claude-code-jsonl")

	// In-place edit to ONLY the hermes mapping: add a quality flag to its rule.
	hermesPath := filepath.Join(dir, "mappings", "hermes-statedb.v1.yaml")
	data, err := os.ReadFile(hermesPath)
	if err != nil {
		t.Fatalf("read hermes mapping: %v", err)
	}
	edited := strings.Replace(string(data), "target_class: session.turn\n",
		"target_class: session.turn\n    quality: degenerate\n", 1)
	if edited == string(data) {
		t.Fatal("test fixture drifted: expected replacement string not found in hermes mapping fixture")
	}
	if err := os.WriteFile(hermesPath, []byte(edited), 0o644); err != nil {
		t.Fatalf("rewrite hermes mapping: %v", err)
	}

	after, err := LoadOntologyDir(dir)
	if err != nil {
		t.Fatalf("LoadOntologyDir (after): %v", err)
	}

	if got := after.SourceFingerprint("hermes-node-a"); got == fpHermesNodeABefore {
		t.Errorf("SourceFingerprint(hermes-node-a) unchanged after editing the mapping that serves it: %q", got)
	}
	if got := after.SourceFingerprint("hermes-cog"); got == fpHermesCogBefore {
		t.Errorf("SourceFingerprint(hermes-cog) unchanged after editing the mapping that serves it: %q", got)
	}
	if got := after.SourceFingerprint("claude-code-jsonl"); got != fpCCBefore {
		t.Errorf("SourceFingerprint(claude-code-jsonl) changed after editing an UNRELATED mapping (hermes-statedb): before=%q after=%q", fpCCBefore, got)
	}
}

// TestSourceFingerprint_NilReceiver_Stable asserts the documented nil-receiver
// contract: a nil *LoadedOntology (enforcement disabled) must not panic and
// must yield a stable fingerprint across calls and across sources, so callers
// (the ApplyPlan coverage-cache check) never need to special-case it.
func TestSourceFingerprint_NilReceiver_Stable(t *testing.T) {
	var lo *LoadedOntology

	fp1 := lo.SourceFingerprint("claude-code-jsonl")
	fp2 := lo.SourceFingerprint("claude-code-jsonl")
	fp3 := lo.SourceFingerprint("some-other-source")

	if fp1 == "" {
		t.Error("nil-receiver SourceFingerprint returned an empty string")
	}
	if fp1 != fp2 {
		t.Errorf("nil-receiver SourceFingerprint not stable across calls: %q vs %q", fp1, fp2)
	}
	if fp1 != fp3 {
		t.Errorf("nil-receiver SourceFingerprint varies by source (it should not, there is no loaded ontology): %q vs %q", fp1, fp3)
	}
}

// ─── OntologyVersionCheck tests ──────────────────────────────────────────────

func TestOntologyVersionCheck(t *testing.T) {
	lo := &LoadedOntology{
		L1: &OntologyDoc{
			ID:      "cogos.conversations",
			Version: "1.0.0",
		},
		OntologyRef: "cogos.conversations@1.0.0",
	}

	cases := []struct {
		requested string
		wantErr   bool
	}{
		{"cogos.conversations@1.0.0", false},
		{"cogos.conversations@^1", false},
		{"cogos.conversations@1", false},
		{"cogos.conversations", false},             // no version = id match only
		{"cogos.conversations@2.0.0", true},        // major mismatch
		{"cogos.conversations@^2", true},           // major mismatch (caret)
		{"cogos.other@1.0.0", true},                // id mismatch
		{"something.totally.different@1.0.0", true},
	}

	for _, tc := range cases {
		err := lo.OntologyVersionCheck(tc.requested)
		if tc.wantErr && err == nil {
			t.Errorf("OntologyVersionCheck(%q): expected error, got nil", tc.requested)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("OntologyVersionCheck(%q): unexpected error: %v", tc.requested, err)
		}
	}
}

func TestOntologyVersionCheck_NilOntology(t *testing.T) {
	var lo *LoadedOntology
	err := lo.OntologyVersionCheck("cogos.conversations@1.0.0")
	if err == nil {
		t.Error("expected error for nil ontology, got nil")
	}
}

// ─── ComponentClass tests ─────────────────────────────────────────────────────

func TestComponentClass(t *testing.T) {
	lo := &LoadedOntology{
		L1: &OntologyDoc{
			Components: map[string]ComponentDecl{
				"session.turn": {},
				"tool.call":    {},
				"tool.result":  {},
			},
		},
	}

	cases := []struct {
		class   string
		wantErr bool
	}{
		{"session.turn", false},
		{"tool.call", false},
		{"tool.result", false},
		{"unknown.class", true},
		{"", true},
	}
	for _, tc := range cases {
		err := lo.ComponentClass(tc.class)
		if tc.wantErr && err == nil {
			t.Errorf("ComponentClass(%q): expected error, got nil", tc.class)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("ComponentClass(%q): unexpected error: %v", tc.class, err)
		}
	}
}

// ─── majorVersion helper tests ────────────────────────────────────────────────

func TestMajorVersion(t *testing.T) {
	cases := []struct {
		semver string
		want   int
	}{
		{"1.0.0", 1},
		{"2.3.4", 2},
		{"10.0.0", 10},
		{"0.1.0", 0},
		{"", 0},
		{"v1.0.0", 0},  // 'v' prefix is not numeric → returns 0
		{"1", 1},
		{"abc", 0},
	}
	for _, tc := range cases {
		got := majorVersion(tc.semver)
		if got != tc.want {
			t.Errorf("majorVersion(%q) = %d, want %d", tc.semver, got, tc.want)
		}
	}
}

// ─── URI param activation tests (component= and ontology= no longer reserved) ─

func TestURIParams_ComponentAndOntologyActivated(t *testing.T) {
	// These should no longer return ErrURIReservedParam — they are active params.
	cases := []struct {
		name string
		uri  string
	}{
		{
			"component= accepted",
			"cog:conversations?component=session.turn",
		},
		{
			"ontology= accepted",
			"cog:conversations?ontology=cogos.conversations@1.0.0",
		},
		{
			"both accepted",
			"cog:conversations?component=session.turn&ontology=cogos.conversations@1.0.0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			uq, err := ParseConversationURI(tc.uri)
			if err != nil {
				t.Errorf("ParseConversationURI(%q): unexpected error: %v", tc.uri, err)
				return
			}
			_ = uq
		})
	}
}

func TestURIParam_ComponentValue(t *testing.T) {
	uq, err := ParseConversationURI("cog:conversations?component=session.turn")
	if err != nil {
		t.Fatalf("ParseConversationURI: %v", err)
	}
	if uq.ComponentClass != "session.turn" {
		t.Errorf("ComponentClass: want session.turn, got %q", uq.ComponentClass)
	}
}

func TestURIParam_OntologyValue(t *testing.T) {
	uq, err := ParseConversationURI("cog:conversations?ontology=cogos.conversations@1.0.0")
	if err != nil {
		t.Fatalf("ParseConversationURI: %v", err)
	}
	if uq.OntologyVersion != "cogos.conversations@1.0.0" {
		t.Errorf("OntologyVersion: want cogos.conversations@1.0.0, got %q", uq.OntologyVersion)
	}
}

func TestURIParam_ComponentEmptyValue_Error(t *testing.T) {
	_, err := ParseConversationURI("cog:conversations?component=")
	if err == nil {
		t.Error("expected error for empty component= value, got nil")
	}
}

func TestURIParam_OntologyEmptyValue_Error(t *testing.T) {
	_, err := ParseConversationURI("cog:conversations?ontology=")
	if err == nil {
		t.Error("expected error for empty ontology= value, got nil")
	}
}
