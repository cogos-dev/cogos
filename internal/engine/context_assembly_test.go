// context_assembly_test.go — tests for foveated context assembly
package engine

import (
	"os"
	"path/filepath"
	"strings"
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

func TestEstTokensPreciseHigherForJSON(t *testing.T) {
	t.Parallel()
	jsonText := `{"user":{"id":123,"name":"Ada","roles":["admin","editor"],"active":true},"items":[{"k":"v1"},{"k":"v2"}],"count":2}`

	fast := estTokens(jsonText)
	precise := estTokensPrecise(jsonText)
	if precise <= fast {
		t.Fatalf("precise = %d; want > fast = %d", precise, fast)
	}
}

func TestEstTokensPreciseHigherForCJK(t *testing.T) {
	t.Parallel()
	cjkText := strings.Repeat("漢字かなカナ混在テキスト", 16)

	fast := estTokens(cjkText)
	precise := estTokensPrecise(cjkText)
	if precise <= fast {
		t.Fatalf("precise = %d; want > fast = %d", precise, fast)
	}
}

func TestEstTokensPreciseMatchesEnglishWithinTenPercent(t *testing.T) {
	t.Parallel()
	englishText := strings.Repeat("plain english prose with ordinary words ", 64)

	fast := estTokens(englishText)
	precise := estTokensPrecise(englishText)
	if precise < fast {
		t.Fatalf("precise = %d; want >= fast = %d", precise, fast)
	}
	if float64(precise-fast) > float64(fast)*0.10 {
		t.Fatalf("precise = %d; want within 10%% of fast = %d", precise, fast)
	}
}

func TestAssembleContextUsesPreciseEstimatorUnderHighIrisPressure(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	p := NewProcess(cfg, makeNucleus("T", "r"))

	messages := []ProviderMessage{{Role: "user", Content: `{"payload":{"id":123,"flags":[true,false,true],"meta":{"nested":"value"}}}`}}

	lowPressure, err := p.AssembleContext("payload", messages, 0, WithIrisSignal(irisSignal{Size: 1000, Used: 700}))
	if err != nil {
		t.Fatalf("AssembleContext low pressure: %v", err)
	}
	highPressure, err := p.AssembleContext("payload", messages, 0, WithIrisSignal(irisSignal{Size: 1000, Used: 900}))
	if err != nil {
		t.Fatalf("AssembleContext high pressure: %v", err)
	}

	if highPressure.TotalTokens <= lowPressure.TotalTokens {
		t.Fatalf("high pressure total = %d; want > low pressure total = %d", highPressure.TotalTokens, lowPressure.TotalTokens)
	}
}

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

func TestEvictForBudgetKeepsTurnPairsAndStandaloneMessages(t *testing.T) {
	t.Parallel()
	conv := []ScoredMessage{
		{Role: "system", Content: "system note", Tokens: 1},
		{Role: "user", Content: "old prompt", Tokens: 4},
		{Role: "assistant", Content: "old answer", Tokens: 2},
		{Role: "tool_result", Content: "tool output", Tokens: 1},
		{Role: "user", Content: "new prompt", Tokens: 1},
		{Role: "assistant", Content: "new answer", Tokens: 2},
	}
	// Budget fits the newest user/assistant pair, fits standalone messages,
	// but only the old assistant would fit on its own.
	_, keptConv := evictForBudget(nil, conv, 7, t.TempDir())

	if len(keptConv) != 4 {
		t.Fatalf("kept conv len = %d; want 4", len(keptConv))
	}

	got := []string{
		keptConv[0].Content,
		keptConv[1].Content,
		keptConv[2].Content,
		keptConv[3].Content,
	}
	want := []string{"system note", "tool output", "new prompt", "new answer"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("keptConv[%d] = %q; want %q", i, got[i], want[i])
		}
	}

	for _, msg := range keptConv {
		if msg.Content == "old prompt" || msg.Content == "old answer" {
			t.Fatalf("old turn pair should be dropped together, kept %q", msg.Content)
		}
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

	// System prompt should contain nucleus first, then client system — and ONLY
	// those (the session-stable half of Zone 1). Post ADR-066/071 amendment the
	// volatile foveal docs no longer live in the system prompt.
	if !contains(sys, "I am Cog.") {
		t.Error("system prompt missing nucleus")
	}
	if !contains(sys, "You are helpful.") {
		t.Error("system prompt missing client system")
	}
	if contains(sys, "Doc A") {
		t.Error("system prompt must NOT contain the foveal doc content (moved to trailing user message)")
	}

	// Messages should be: conversation history + current message.
	if len(msgs) != 3 {
		t.Fatalf("msgs len = %d; want 3", len(msgs))
	}
	if msgs[0].Content != "hello" {
		t.Errorf("msgs[0] = %q; want 'hello'", msgs[0].Content)
	}
	// The final user message carries the user's own text PLUS the trailing foveal
	// block (doc content). The user text must lead; the doc content must follow.
	last := msgs[2].Content
	if !strings.HasPrefix(last, "what is an eigenform?") {
		t.Errorf("final message must start with the user text; got %q", last)
	}
	if !contains(last, "Doc A") {
		t.Error("final user message missing trailing foveal doc content")
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

	sys, msgs := pkg.FormatForProvider()
	// Manifest now lives in the trailing user message, NOT the system prompt.
	if contains(sys, "# Workspace Context") {
		t.Errorf("manifest must not be in the system prompt: %q", sys)
	}
	if len(msgs) != 1 {
		t.Fatalf("msgs len = %d; want 1 (the current message with folded manifest)", len(msgs))
	}
	rendered := msgs[0].Content
	if !contains(rendered, "# Workspace Context (1 relevant CogDocs)") {
		t.Errorf("trailing message missing manifest heading: %q", rendered)
	}
	if !contains(rendered, "Use cog_read_cogdoc to access full content when needed") {
		t.Error("trailing message missing retrieval hint")
	}
	// Salience float has been STRIPPED for byte-stability — URI + summary remain.
	if !contains(rendered, "cog://mem/semantic/architecture/spec.cog.md — foveated context architecture overview") {
		t.Error("trailing message missing manifest entry")
	}
	if contains(rendered, "[salience:") {
		t.Error("manifest must not render the live salience float (byte-stability)")
	}
	if !contains(rendered, "## Schema Notes") {
		t.Error("trailing message missing schema notes")
	}
	if !contains(rendered, "missing: tags, type") {
		t.Error("trailing message missing schema issue details")
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

// ── foveated gating cap (issue #88) ──────────────────────────────────────────

// TestAssembleContextRespectsMaxFovealDocs confirms that the MaxFovealDocs cap
// from Config bounds the keyword/salience admission branch. Issue #88 acceptance:
// MaxFovealDocs = 5 must result in len(pkg.FovealDocs) <= 5 even when the index
// holds many more candidates.
func TestAssembleContextRespectsMaxFovealDocs(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	cfg.MaxFovealDocs = 5
	cfg.SalienceFloor = 0 // don't let the floor mask the cap behavior
	p := NewProcess(cfg, makeNucleus("T", "r"))

	memDir := filepath.Join(root, ".cog", "mem", "semantic")

	// Plant 20 active CogDocs, all matching the query keyword to push them
	// above zero relevance regardless of field salience.
	for i := 0; i < 20; i++ {
		body := "---\ntitle: Topic " +
			string(rune('A'+i%26)) + string(rune('0'+i%10)) +
			"\nstatus: active\ntags: [topic]\n---\n\ntopic body content for topic.\n"
		path := filepath.Join(memDir, "topic-"+string(rune('a'+i%26))+string(rune('0'+i%10))+"-"+
			string(rune('A'+i))+".cog.md")
		writeTestFile(t, path, body)
	}

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

	if len(pkg.FovealDocs) > 5 {
		t.Errorf("FovealDocs len = %d; MaxFovealDocs=5 should cap at 5", len(pkg.FovealDocs))
	}
	if len(pkg.FovealDocs) == 0 {
		t.Errorf("FovealDocs len = 0; cap-of-5 should still admit some docs when 20 match")
	}
}

// TestContextGatingDefaults exercises the Config.ContextGating accessor so the
// hot-update contract is covered.
func TestContextGatingDefaults(t *testing.T) {
	t.Parallel()
	cfg := &Config{}
	maxDocs, floor := cfg.ContextGating()
	if maxDocs != DefaultMaxFovealDocs {
		t.Errorf("default MaxFovealDocs = %d; want %d", maxDocs, DefaultMaxFovealDocs)
	}
	if floor != DefaultSalienceFloor {
		t.Errorf("default SalienceFloor = %v; want %v", floor, DefaultSalienceFloor)
	}

	newMax := 3
	newFloor := 0.55
	gotMax, gotFloor := cfg.SetContextGating(&newMax, &newFloor)
	if gotMax != 3 || gotFloor != 0.55 {
		t.Errorf("SetContextGating returned (%d, %v); want (3, 0.55)", gotMax, gotFloor)
	}
	gotMax, gotFloor = cfg.ContextGating()
	if gotMax != 3 || gotFloor != 0.55 {
		t.Errorf("ContextGating after Set = (%d, %v); want (3, 0.55)", gotMax, gotFloor)
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

// ── conversation combined-score (issue #92) ─────────────────────────────────

// TestScoreConversationPopulatesCombinedScore guards against the regression in
// issue #92: any turn with non-zero recency or relevance must have a non-zero
// CombinedScore. A flat-zero CombinedScore degrades eviction to position-based.
func TestScoreConversationPopulatesCombinedScore(t *testing.T) {
	t.Parallel()
	history := []ProviderMessage{
		{Role: "user", Content: "tell me about eigenforms"},
		{Role: "assistant", Content: "eigenforms are fixed points of cognition"},
		{Role: "user", Content: "and what about the weather"},
	}
	keywords := extractKeywords("eigenforms")
	scored := scoreConversation(history, keywords)
	if len(scored) == 0 {
		t.Fatalf("expected scored turns")
	}
	for i, m := range scored {
		if m.RecencyScore > 0 || m.RelevanceScore > 0 {
			if m.CombinedScore <= 0 {
				t.Errorf("turn %d: recency=%.3f relevance=%.3f but combined=%.3f; want > 0",
					i, m.RecencyScore, m.RelevanceScore, m.CombinedScore)
			}
		}
	}
}

// TestEvictForBudgetPrefersHighCombinedScore exercises the eviction loop's use
// of CombinedScore. Two pairs of identical token cost are presented; only one
// pair fits. The pair with higher CombinedScore must be retained regardless of
// chronological position.
func TestEvictForBudgetPrefersHighCombinedScore(t *testing.T) {
	t.Parallel()

	// Two user/assistant pairs, identical token cost (10 tokens each).
	// Pair A is older but has high relevance (combined ≈ 0.6).
	// Pair B is newer but has low relevance (combined ≈ 0.4).
	// Wait — recency favors B (newer = higher recency). To make the test
	// unambiguous about CombinedScore (not recency or position), set
	// scores explicitly so A's combined > B's combined.
	conv := []ScoredMessage{
		{Role: "user", Content: "old question", Tokens: 4, TurnIndex: 0,
			RecencyScore: 0.25, RelevanceScore: 1.0, CombinedScore: 0.85},
		{Role: "assistant", Content: "old answer with key info", Tokens: 6, TurnIndex: 1,
			RecencyScore: 0.50, RelevanceScore: 1.0, CombinedScore: 0.90},
		{Role: "user", Content: "newer chitchat", Tokens: 4, TurnIndex: 2,
			RecencyScore: 0.75, RelevanceScore: 0.0, CombinedScore: 0.45},
		{Role: "assistant", Content: "newer chitchat reply", Tokens: 6, TurnIndex: 3,
			RecencyScore: 1.00, RelevanceScore: 0.0, CombinedScore: 0.60},
	}

	// Budget fits exactly one pair (10 tokens).
	_, kept := evictForBudget(nil, conv, 10, t.TempDir())

	if len(kept) != 2 {
		t.Fatalf("kept len = %d; want 2 (one pair)", len(kept))
	}
	// Expect the higher-CombinedScore pair (A: "old question" / "old answer").
	if kept[0].Content != "old question" || kept[1].Content != "old answer with key info" {
		t.Errorf("kept = [%q, %q]; want the high-CombinedScore pair",
			kept[0].Content, kept[1].Content)
	}
}

// TestEvictForBudgetSingletonsPreferHighCombinedScore exercises the same
// invariant for standalone messages (no user/assistant pair binding).
func TestEvictForBudgetSingletonsPreferHighCombinedScore(t *testing.T) {
	t.Parallel()

	conv := []ScoredMessage{
		{Role: "system", Content: "low-value note", Tokens: 5, TurnIndex: 0,
			CombinedScore: 0.10},
		{Role: "tool_result", Content: "high-value tool output", Tokens: 5, TurnIndex: 1,
			CombinedScore: 0.95},
		{Role: "system", Content: "another low-value note", Tokens: 5, TurnIndex: 2,
			CombinedScore: 0.20},
	}

	// Budget fits one item.
	_, kept := evictForBudget(nil, conv, 5, t.TempDir())

	if len(kept) != 1 {
		t.Fatalf("kept len = %d; want 1", len(kept))
	}
	if kept[0].Content != "high-value tool output" {
		t.Errorf("kept = %q; want the high-CombinedScore standalone", kept[0].Content)
	}
}

// ── default_budget and exclude_substrings (issue #77) ──────────────────────

// TestEffectiveBudgetFallback verifies that EffectiveBudget() returns the
// package-level DefaultBudget constant when the config field is zero (not
// explicitly set via kernel.yaml).
func TestEffectiveBudgetFallback(t *testing.T) {
	t.Parallel()
	cfg := &Config{}
	if got := cfg.EffectiveBudget(); got != DefaultBudget {
		t.Errorf("EffectiveBudget with zero config = %d; want DefaultBudget=%d", got, DefaultBudget)
	}
}

// TestEffectiveBudgetFromConfig verifies that a non-zero DefaultBudget in
// the Config is respected by EffectiveBudget() and flows through to the
// assembled context package's Budget field.
func TestEffectiveBudgetFromConfig(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	cfg.DefaultBudget = 8192
	p := NewProcess(cfg, makeNucleus("T", "r"))

	pkg, err := p.AssembleContext("hello", nil, 0)
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}
	// Budget in the returned package must reflect the configured ceiling.
	if pkg.Budget != 8192 {
		t.Errorf("pkg.Budget = %d; want 8192 (from cfg.DefaultBudget)", pkg.Budget)
	}
}

// TestExplicitBudgetOverridesDefault verifies that a positive budget passed
// directly to AssembleContext takes precedence over cfg.DefaultBudget.
func TestExplicitBudgetOverridesDefault(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	cfg.DefaultBudget = 8192
	p := NewProcess(cfg, makeNucleus("T", "r"))

	const explicitBudget = 4096
	pkg, err := p.AssembleContext("hello", nil, explicitBudget)
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}
	if pkg.Budget != explicitBudget {
		t.Errorf("pkg.Budget = %d; want %d (explicit caller override)", pkg.Budget, explicitBudget)
	}
}

// TestExcludeSubstringsFiltersMatchingPaths verifies that paths matching any
// entry in cfg.ExcludeSubstrings are excluded from the foveated candidate set
// even when they score above the salience floor.
func TestExcludeSubstringsFiltersMatchingPaths(t *testing.T) {
	t.Parallel()
	root := makeWorkspace(t)
	cfg := makeConfig(t, root)
	cfg.SalienceFloor = 0 // floor off so exclusion is the only filter
	cfg.ExcludeSubstrings = []string{"/sensitive/"}
	p := NewProcess(cfg, makeNucleus("T", "r"))

	memDir := filepath.Join(root, ".cog", "mem")
	sensitiveDir := filepath.Join(memDir, "sensitive")
	if err := os.MkdirAll(sensitiveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	semanticDir := filepath.Join(memDir, "semantic")

	// Sensitive doc: should be excluded by the glob.
	writeTestFile(t, filepath.Join(sensitiveDir, "secret.cog.md"),
		"---\ntitle: Secret Sensitive\nstatus: active\n---\n\nsensitive secret content\n")
	// Normal doc: should pass through.
	writeTestFile(t, filepath.Join(semanticDir, "normal.cog.md"),
		"---\ntitle: Normal Doc\nstatus: active\n---\n\nnormal content\n")

	idx, err := BuildIndex(root)
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	p.indexMu.Lock()
	p.index = idx
	p.indexMu.Unlock()

	pkg, err := p.AssembleContext("sensitive secret normal", nil, 0)
	if err != nil {
		t.Fatalf("AssembleContext: %v", err)
	}

	for _, doc := range pkg.FovealDocs {
		if strings.Contains(filepath.ToSlash(doc.Path), "/sensitive/") {
			t.Errorf("excluded substring path leaked into foveal context: %s", doc.Path)
		}
	}
}

// TestPathMatchesExcludeSubstrings exercises the helper directly with edge cases.
func TestPathMatchesExcludeSubstrings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path       string
		substrings []string
		expect     bool
	}{
		{"/workspace/.cog/mem/inbox/foo.cog.md", []string{"/inbox/"}, true},
		{"/workspace/.cog/mem/semantic/foo.cog.md", []string{"/inbox/"}, false},
		{"/workspace/.cog/mem/semantic/foo.cog.md", nil, false},
		{"/workspace/.cog/mem/sensitive/bar.cog.md", []string{"/inbox/", "/sensitive/"}, true},
		{"/workspace/.cog/mem/semantic/foo.cog.md", []string{""}, false}, // empty entry ignored
	}
	for _, tc := range cases {
		got := pathMatchesExcludeSubstrings(tc.path, tc.substrings)
		if got != tc.expect {
			t.Errorf("pathMatchesExcludeSubstrings(%q, %v) = %v; want %v", tc.path, tc.substrings, got, tc.expect)
		}
	}
}

// TestContextExcludeSubstringsSnapshot verifies that ContextExcludeSubstrings
// returns a copy of the configured slice (not the live pointer) and handles
// nil safely.
func TestContextExcludeSubstringsSnapshot(t *testing.T) {
	t.Parallel()
	cfg := &Config{}
	if subs := cfg.ContextExcludeSubstrings(); subs != nil {
		t.Errorf("empty config: ContextExcludeSubstrings = %v; want nil", subs)
	}

	cfg.ExcludeSubstrings = []string{"/inbox/", "/sensitive/"}
	got := cfg.ContextExcludeSubstrings()
	if len(got) != 2 || got[0] != "/inbox/" || got[1] != "/sensitive/" {
		t.Errorf("ContextExcludeSubstrings = %v; want [/inbox/ /sensitive/]", got)
	}

	// Mutation of the returned slice must not affect the config.
	got[0] = "/mutated/"
	if cfg.ExcludeSubstrings[0] != "/inbox/" {
		t.Error("ContextExcludeSubstrings returned live slice instead of copy")
	}
}

// ── TestDebugZoneItemExposesCombinedScore ────────────────────────────────────

// TestDebugZoneItemExposesCombinedScore verifies the debug-snapshot pipeline
// surfaces CombinedScore on conversation zone items so the /v1/debug/last
// endpoint can render it instead of defaulting to 0.00.
func TestDebugZoneItemExposesCombinedScore(t *testing.T) {
	t.Parallel()

	pkg := &ContextPackage{
		Conversation: []ScoredMessage{
			{Role: "user", Content: "hello", Tokens: 1,
				RecencyScore: 0.5, RelevanceScore: 0.5, CombinedScore: 0.5},
		},
	}
	view := buildContextView(pkg)

	var convZone *DebugZone
	for i := range view.Zones {
		if view.Zones[i].Zone == "conversation" {
			convZone = &view.Zones[i]
			break
		}
	}
	if convZone == nil {
		t.Fatalf("expected conversation zone in debug view")
	}
	if len(convZone.Items) != 1 {
		t.Fatalf("conv items = %d; want 1", len(convZone.Items))
	}
	if got := convZone.Items[0].CombinedScore; got != 0.5 {
		t.Errorf("DebugZoneItem.CombinedScore = %v; want 0.5", got)
	}
}

// Regression: tool-call linkage (ToolCallID, ToolCalls) must survive the
// scoreConversation -> ScoredMessage -> FormatForProvider round-trip. Dropping
// it reduced history turns to {role, content}, sending role:"tool" results to
// Anthropic verbatim -> 400 "Unexpected role tool".
func TestContextAssemblyPreservesToolLinkage(t *testing.T) {
	history := []ProviderMessage{
		{Role: "user", Content: "run ls"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Name: "terminal"}}},
		{Role: "tool", ToolCallID: "call_1", Content: "file1 file2"},
	}
	scored := scoreConversation(history, nil)
	if len(scored) != 3 {
		t.Fatalf("want 3 scored messages; got %d", len(scored))
	}
	if scored[1].ToolCalls == nil || scored[1].ToolCalls[0].ID != "call_1" {
		t.Errorf("scored assistant must keep ToolCalls; got %+v", scored[1])
	}
	if scored[2].ToolCallID != "call_1" {
		t.Errorf("scored tool result must keep ToolCallID; got %q", scored[2].ToolCallID)
	}

	pkg := &ContextPackage{Conversation: scored}
	_, msgs := pkg.FormatForProvider()
	var tool, asst *ProviderMessage
	for i := range msgs {
		switch msgs[i].Role {
		case "tool":
			tool = &msgs[i]
		case "assistant":
			asst = &msgs[i]
		}
	}
	if tool == nil || tool.ToolCallID != "call_1" {
		t.Fatalf("FormatForProvider must preserve tool ToolCallID; got %+v", tool)
	}
	if asst == nil || len(asst.ToolCalls) != 1 || asst.ToolCalls[0].ID != "call_1" {
		t.Fatalf("FormatForProvider must preserve assistant ToolCalls; got %+v", asst)
	}
}
