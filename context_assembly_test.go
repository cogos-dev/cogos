// context_assembly_test.go — tests for foveated context assembly
package main

import (
	"os"
	"path/filepath"
	"testing"
)

// ── status filtering ──────────────────────────────────────────────────────────

func TestAssembleContextSkipsStaleStatuses(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	p := NewProcess(cfg, makeNucleus("T", "r"))

	memDir := filepath.Join(root, ".cog", "mem", "semantic")

	writeTestFile(t, filepath.Join(memDir, "active.cog.md"), "---\ntitle: Active Topic\nstatus: active\n---\n\nActive content.\n")
	writeTestFile(t, filepath.Join(memDir, "superseded.cog.md"), "---\ntitle: Superseded Topic\nstatus: superseded\n---\n\nStale content.\n")
	writeTestFile(t, filepath.Join(memDir, "deprecated.cog.md"), "---\ntitle: Deprecated Topic\nstatus: deprecated\n---\n\nDeprecated content.\n")
	writeTestFile(t, filepath.Join(memDir, "retired.cog.md"), "---\ntitle: Retired Topic\nstatus: retired\n---\n\nRetired content.\n")

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	p.indexMu.Lock()
	p.index = idx
	p.indexMu.Unlock()

	pkg, err := p.AssembleContext("topic", nil, 0)
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}

	stalePaths := map[string]bool{
		filepath.Join(memDir, "superseded.cog.md"): true,
		filepath.Join(memDir, "deprecated.cog.md"): true,
		filepath.Join(memDir, "retired.cog.md"):    true,
	}
	for _, doc := range pkg.FovealDocs {
		if stalePaths[doc.Path] {
			t.Errorf("stale doc injected into context: %s", filepath.Base(doc.Path))
		}
	}

	found := false
	for _, doc := range pkg.FovealDocs {
		if doc.Path == filepath.Join(memDir, "active.cog.md") {
			found = true
		}
	}
	if !found {
		t.Error("active doc should be in context but was missing")
	}
}

// ── archive/ path filtering ───────────────────────────────────────────────────

func TestAssembleContextSkipsArchiveDirs(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	p := NewProcess(cfg, makeNucleus("T", "r"))

	memDir := filepath.Join(root, ".cog", "mem")
	archiveDir := filepath.Join(memDir, "archive")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		t.Fatalf("mkdir archive: %v", err)
	}

	writeTestFile(t, filepath.Join(archiveDir, "old.cog.md"), "---\ntitle: Old Record\nstatus: active\n---\n\nArchived record content.\n")
	writeTestFile(t, filepath.Join(memDir, "semantic", "current.cog.md"), "---\ntitle: Current Record\nstatus: active\n---\n\nCurrent record content.\n")

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	p.indexMu.Lock()
	p.index = idx
	p.indexMu.Unlock()

	pkg, err := p.AssembleContext("record", nil, 0)
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}

	archivePath := filepath.Join(archiveDir, "old.cog.md")
	for _, doc := range pkg.FovealDocs {
		if doc.Path == archivePath {
			t.Errorf("archived doc should not appear in context: %s", doc.Path)
		}
	}
}

// ── conversation scoring ─────────────────────────────────────────────────────

func TestScoreConversationRecency(t *testing.T) {
	t.Parallel()
	history := []ProviderMessage{
		{Role: "user", Content: "oldest message"},
		{Role: "assistant", Content: "old reply"},
		{Role: "user", Content: "newest message"},
	}
	scored := scoreConversation(history, nil)
	if len(scored) != 3 {
		t.Fatalf("len = %d; want 3", len(scored))
	}
	// Newest should have highest recency.
	if scored[2].RecencyScore <= scored[0].RecencyScore {
		t.Errorf("newest recency %f should be > oldest %f", scored[2].RecencyScore, scored[0].RecencyScore)
	}
}

func TestScoreConversationRelevance(t *testing.T) {
	t.Parallel()
	history := []ProviderMessage{
		{Role: "user", Content: "tell me about eigenforms"},
		{Role: "assistant", Content: "the weather is nice today"},
	}
	keywords := extractKeywords("eigenforms")
	scored := scoreConversation(history, keywords)
	if scored[0].RelevanceScore <= scored[1].RelevanceScore {
		t.Errorf("eigenform message relevance %f should be > weather message %f",
			scored[0].RelevanceScore, scored[1].RelevanceScore)
	}
}

func TestScoreConversationEmpty(t *testing.T) {
	t.Parallel()
	scored := scoreConversation(nil, nil)
	if scored != nil {
		t.Errorf("empty history should return nil, got %d items", len(scored))
	}
}

// ── eviction ────────────────────────────────────────────────────────────────

func TestEvictForBudgetFitsAll(t *testing.T) {
	t.Parallel()
	docs := []FovealDoc{
		{Path: "/dev/null", Title: "A", Salience: 1.0},
	}
	conv := []ScoredMessage{
		{Role: "user", Content: "hi", Tokens: 1},
	}
	// Huge budget — everything fits.
	keptDocs, keptConv := evictForBudget(docs, conv, 100000, t.TempDir())
	if len(keptConv) != 1 {
		t.Errorf("conv len = %d; want 1", len(keptConv))
	}
	// Docs won't load from /dev/null, so keptDocs may be empty — that's fine.
	_ = keptDocs
}

func TestEvictForBudgetDropsOldestConversation(t *testing.T) {
	t.Parallel()
	conv := []ScoredMessage{
		{Role: "user", Content: "old message that is fairly long", Tokens: 8},
		{Role: "assistant", Content: "old reply that is also long", Tokens: 7},
		{Role: "user", Content: "new", Tokens: 1},
		{Role: "assistant", Content: "new reply", Tokens: 2},
	}
	// Budget only fits the newest turns.
	_, keptConv := evictForBudget(nil, conv, 5, t.TempDir())
	// Should keep newest turns, drop oldest.
	if len(keptConv) > 2 {
		t.Errorf("expected eviction, got %d turns", len(keptConv))
	}
	// Newest turn should be present.
	if len(keptConv) > 0 && keptConv[len(keptConv)-1].Content != "new reply" {
		t.Errorf("last kept message = %q; want 'new reply'", keptConv[len(keptConv)-1].Content)
	}
}

func TestEvictForBudgetZero(t *testing.T) {
	t.Parallel()
	docs, conv := evictForBudget(nil, nil, 0, t.TempDir())
	if docs != nil || conv != nil {
		t.Error("zero budget should return nil slices")
	}
}

func TestEvictForBudgetManifestModeUsesSummary(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	memDir := filepath.Join(root, ".cog", "mem", "semantic")
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(memDir, "manifest-test.cog.md")
	writeTestFile(t, path, "---\ntitle: Manifest Test\ndescription: concise summary\ntype: insight\ntags: [alpha]\n---\n\nLonger body content that should not be injected in manifest mode.\n")

	keptDocs, _ := evictForBudgetMode([]FovealDoc{{Path: path, Salience: 0.87}}, nil, 1000, root, true)
	if len(keptDocs) != 1 {
		t.Fatalf("docs len = %d; want 1", len(keptDocs))
	}
	if keptDocs[0].Content != "" {
		t.Errorf("Content = %q; want empty in manifest mode", keptDocs[0].Content)
	}
	if keptDocs[0].Summary != "concise summary" {
		t.Errorf("Summary = %q; want description", keptDocs[0].Summary)
	}
	if len(keptDocs[0].SchemaIssues) != 0 {
		t.Errorf("SchemaIssues = %v; want empty", keptDocs[0].SchemaIssues)
	}
	if keptDocs[0].Tokens <= 0 {
		t.Errorf("Tokens = %d; want > 0", keptDocs[0].Tokens)
	}
}

// ── FormatForProvider ───────────────────────────────────────────────────────

func TestFormatForProviderStabilityOrder(t *testing.T) {
	t.Parallel()
	pkg := &ContextPackage{
		NucleusText:  "I am Cog.",
		ClientSystem: "You are helpful.",
		FovealDocs: []FovealDoc{
			{Title: "Doc A", Content: "Content A"},
		},
		Conversation: []ScoredMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi there"},
		},
		CurrentMessage: &ProviderMessage{Role: "user", Content: "what is an eigenform?"},
	}

	sys, msgs := pkg.FormatForProvider()

	// System prompt should contain nucleus first, then client system, then docs.
	if !contains(sys, "I am Cog.") {
		t.Error("system prompt missing nucleus")
	}
	if !contains(sys, "You are helpful.") {
		t.Error("system prompt missing client system")
	}
	if !contains(sys, "Doc A") {
		t.Error("system prompt missing CogDoc")
	}

	// Messages should be: conversation history + current message.
	if len(msgs) != 3 {
		t.Fatalf("msgs len = %d; want 3", len(msgs))
	}
	if msgs[0].Content != "hello" {
		t.Errorf("msgs[0] = %q; want 'hello'", msgs[0].Content)
	}
	if msgs[2].Content != "what is an eigenform?" {
		t.Errorf("msgs[2] = %q; want 'what is an eigenform?'", msgs[2].Content)
	}
}

func TestFormatForProviderNoConversation(t *testing.T) {
	t.Parallel()
	pkg := &ContextPackage{
		NucleusText:    "Identity.",
		CurrentMessage: &ProviderMessage{Role: "user", Content: "hi"},
	}

	sys, msgs := pkg.FormatForProvider()
	if sys != "Identity." {
		t.Errorf("system = %q; want 'Identity.'", sys)
	}
	if len(msgs) != 1 || msgs[0].Content != "hi" {
		t.Errorf("msgs = %v; want single 'hi' message", msgs)
	}
}

func TestFormatForProviderManifestOutput(t *testing.T) {
	t.Parallel()
	pkg := &ContextPackage{
		NucleusText: "Identity.",
		FovealDocs: []FovealDoc{
			{
				URI:          "cog://mem/semantic/architecture/spec.cog.md",
				Title:        "Spec",
				Summary:      "foveated context architecture overview",
				Salience:     0.87,
				SchemaIssues: []string{"missing_tags", "missing_type"},
			},
		},
		CurrentMessage: &ProviderMessage{Role: "user", Content: "hi"},
	}

	sys, _ := pkg.FormatForProvider()
	if !contains(sys, "# Workspace Context (1 relevant CogDocs)") {
		t.Errorf("system prompt missing manifest heading: %q", sys)
	}
	if !contains(sys, "Use cog_read_cogdoc to access full content when needed") {
		t.Error("system prompt missing retrieval hint")
	}
	if !contains(sys, "cog://mem/semantic/architecture/spec.cog.md — foveated context architecture overview [salience: 0.87]") {
		t.Error("system prompt missing manifest entry")
	}
	if !contains(sys, "## Schema Notes") {
		t.Error("system prompt missing schema notes")
	}
	if !contains(sys, "missing: tags, type") {
		t.Error("system prompt missing schema issue details")
	}
}

// ── full assembly with conversation ──────────────────────────────────────────

func TestAssembleContextWithConversation(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	p := NewProcess(cfg, makeNucleus("Cog", "eigenform"))

	clientMsgs := []ProviderMessage{
		{Role: "system", Content: "You are a helpful assistant."},
		{Role: "user", Content: "what is an eigenform?"},
		{Role: "assistant", Content: "An eigenform is a fixed point of a recursive operation."},
		{Role: "user", Content: "how does that relate to identity?"},
	}

	pkg, err := p.AssembleContext("identity eigenform", clientMsgs, 0)
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}

	// Client system prompt should be extracted.
	if pkg.ClientSystem != "You are a helpful assistant." {
		t.Errorf("ClientSystem = %q; want client system prompt", pkg.ClientSystem)
	}

	// Current message should be the last user message.
	if pkg.CurrentMessage == nil || pkg.CurrentMessage.Content != "how does that relate to identity?" {
		t.Errorf("CurrentMessage = %v; want last user message", pkg.CurrentMessage)
	}

	// Conversation should contain the middle turns.
	if len(pkg.Conversation) != 2 {
		t.Errorf("Conversation len = %d; want 2 (first user + assistant)", len(pkg.Conversation))
	}

	// FormatForProvider should produce valid output.
	sys, msgs := pkg.FormatForProvider()
	if sys == "" {
		t.Error("system prompt should not be empty")
	}
	// Should have: 2 conversation turns + 1 current message = 3 messages.
	if len(msgs) != 3 {
		t.Errorf("msgs len = %d; want 3", len(msgs))
	}
}

func TestAssembleContextManifestMode(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	p := NewProcess(cfg, makeNucleus("Cog", "eigenform"))

	memDir := filepath.Join(root, ".cog", "mem", "semantic")
	writeTestFile(t, filepath.Join(memDir, "manifested.cog.md"), "---\ntitle: Manifested\ndescription: short summary\ntype: insight\ntags: [manifest]\nstatus: active\n---\n\nThis is the full body that should stay out of the prompt.\n")

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	p.indexMu.Lock()
	p.index = idx
	p.indexMu.Unlock()

	pkg, err := p.AssembleContext("manifested", []ProviderMessage{{Role: "user", Content: "manifested"}}, 0, WithManifestMode(true))
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}
	if len(pkg.FovealDocs) == 0 {
		t.Fatal("expected at least one manifest doc")
	}
	if pkg.FovealDocs[0].Content != "" {
		t.Errorf("Content = %q; want empty in manifest mode", pkg.FovealDocs[0].Content)
	}
	if pkg.FovealDocs[0].Summary != "short summary" {
		t.Errorf("Summary = %q; want description", pkg.FovealDocs[0].Summary)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
