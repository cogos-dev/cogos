// foveation_placement_test.go — acceptance tests for the ADR-066/071 amendment
// "foveation placement under prefix-cache runtimes" (2026-07-07).
//
// Covers:
//   - Change 1: cache-aware placement — the volatile foveal block moves from the
//     leading system prompt to a TRAILING injection in the final user message, so
//     the rendered PREFIX (system + all-but-last message) is byte-identical across
//     two consecutive turns with the same doc selection.
//   - Change 2: salience-float strip — an identical doc selection renders identical
//     manifest bytes.
//   - Change 3: session key + LightConeManager TTL.
//   - Wire safety: the trailing injection survives normalizeAnthropicMessages on the
//     Anthropic path (ends with a user turn, block order valid) and is present on the
//     OpenAI path.
package engine

import (
	"strings"
	"testing"
	"time"
)

// manifestDocs returns FovealDocs in manifest form (Content=="" so docsUseManifest
// is true) with the given salience values.
func manifestDocs(salienceA, salienceB float64) []FovealDoc {
	return []FovealDoc{
		{URI: "cog://mem/a", Summary: "doc a summary", Salience: salienceA},
		{URI: "cog://mem/b", Summary: "doc b summary", Salience: salienceB},
	}
}

// renderedPrefix returns the byte-string that a prefix cache would key on: the
// system prompt followed by every message EXCEPT the last. Two turns whose prefixes
// are equal can reuse the cached KV for everything up to the final exchange.
func renderedPrefix(sys string, msgs []ProviderMessage) string {
	var sb strings.Builder
	sb.WriteString("SYSTEM\x1e")
	sb.WriteString(sys)
	sb.WriteString("\x1d")
	for i := 0; i < len(msgs)-1; i++ {
		sb.WriteString(msgs[i].Role)
		sb.WriteString("\x1e")
		sb.WriteString(msgs[i].Content)
		sb.WriteString("\x1d")
	}
	return sb.String()
}

// ── Change 1: cache-aware placement + prefix stability ────────────────────────

func TestFovealBlockRendersTrailingNotInSystem(t *testing.T) {
	t.Parallel()
	pkg := &ContextPackage{
		NucleusText:    "I am Cog.",
		ClientSystem:   "You are helpful.",
		FovealDocs:     manifestDocs(0.91, 0.42),
		CurrentMessage: &ProviderMessage{Role: "user", Content: "what changed?"},
	}
	sys, msgs := pkg.FormatForProvider()

	if strings.Contains(sys, "# Workspace Context") {
		t.Errorf("system prompt must not contain the foveal manifest; got:\n%s", sys)
	}
	if !strings.Contains(sys, "I am Cog.") || !strings.Contains(sys, "# Client Context") {
		t.Error("system prompt must still carry nucleus + client context")
	}
	last := msgs[len(msgs)-1]
	if last.Role != "user" {
		t.Fatalf("final message role = %q; want user", last.Role)
	}
	if !strings.HasPrefix(last.Content, "what changed?") {
		t.Errorf("final message must lead with the user's text; got %q", last.Content)
	}
	if !strings.Contains(last.Content, "# Workspace Context") {
		t.Error("final user message must carry the trailing foveal manifest")
	}
}

// PREFIX-STABILITY — the acceptance criterion for Change 1.
//
// Two consecutive turns of the same conversation with the SAME doc selection:
// the rendered prefix (system + all-but-last message) must be byte-identical, so a
// prefix cache reuses everything and only the final exchange + foveal block
// re-prefill.
func TestPrefixByteStableAcrossTurnsSameSelection(t *testing.T) {
	t.Parallel()

	// Turn 1: history [u,a], current = user "Q1".
	turn1 := &ContextPackage{
		NucleusText:  "I am Cog.",
		ClientSystem: "You are helpful.",
		FovealDocs:   manifestDocs(0.91, 0.42),
		Conversation: []ScoredMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi there"},
		},
		CurrentMessage: &ProviderMessage{Role: "user", Content: "Q1"},
	}

	// Turn 2: turn-1's current exchange has become history; a NEW user message
	// arrives. SAME doc selection. In a real prefix cache the salience floats would
	// have been re-scored — model different values here to prove the strip matters.
	turn2 := &ContextPackage{
		NucleusText:  "I am Cog.",
		ClientSystem: "You are helpful.",
		FovealDocs:   manifestDocs(0.55, 0.88), // re-scored salience; must not affect bytes
		Conversation: []ScoredMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi there"},
			{Role: "user", Content: "Q1"},
			{Role: "assistant", Content: "A1"},
		},
		CurrentMessage: &ProviderMessage{Role: "user", Content: "Q2"},
	}

	sys1, msgs1 := turn1.FormatForProvider()
	sys2, msgs2 := turn2.FormatForProvider()

	// The turn-1 prefix (system + [hello, hi there]) must appear byte-identically
	// at the head of the turn-2 prefix. We check the strongest property: the whole
	// turn-1 prefix is a prefix of the turn-2 prefix.
	p1 := renderedPrefix(sys1, msgs1)
	p2 := renderedPrefix(sys2, msgs2)
	if !strings.HasPrefix(p2, p1) {
		t.Fatalf("prefix instability: turn-1 prefix is not a byte-prefix of turn-2.\nturn1:\n%q\nturn2:\n%q", p1, p2)
	}

	// And specifically: the system prompt is byte-identical turn to turn (it now
	// carries only session-stable content).
	if sys1 != sys2 {
		t.Errorf("system prompt drifted between turns:\n%q\nvs\n%q", sys1, sys2)
	}

	// Sanity: the re-scored salience did NOT leak into the trailing block bytes.
	lastTurn1 := msgs1[len(msgs1)-1].Content
	lastTurn2 := msgs2[len(msgs2)-1].Content
	if strings.Contains(lastTurn1, "salience:") || strings.Contains(lastTurn2, "salience:") {
		t.Error("salience float leaked into the trailing block despite the strip")
	}
}

// A CHANGED doc selection must alter ONLY the trailing block — the conversation
// prefix stays stable.
func TestPrefixStableWhenOnlySelectionChanges(t *testing.T) {
	t.Parallel()
	base := func(docs []FovealDoc) *ContextPackage {
		return &ContextPackage{
			NucleusText:  "I am Cog.",
			ClientSystem: "You are helpful.",
			FovealDocs:   docs,
			Conversation: []ScoredMessage{
				{Role: "user", Content: "hello"},
				{Role: "assistant", Content: "hi there"},
			},
			CurrentMessage: &ProviderMessage{Role: "user", Content: "same question"},
		}
	}
	selA := []FovealDoc{{URI: "cog://mem/a", Summary: "doc a", Salience: 0.9}}
	selB := []FovealDoc{{URI: "cog://mem/z", Summary: "doc z", Salience: 0.9}}

	sysA, msgsA := base(selA).FormatForProvider()
	sysB, msgsB := base(selB).FormatForProvider()

	if renderedPrefix(sysA, msgsA) != renderedPrefix(sysB, msgsB) {
		t.Error("changing doc selection must not disturb the conversation prefix")
	}
	if msgsA[len(msgsA)-1].Content == msgsB[len(msgsB)-1].Content {
		t.Error("changing doc selection must change the trailing foveal block")
	}
}

// ── Change 2: salience-float strip → byte-stable manifest ─────────────────────

func TestManifestByteStableAcrossSalience(t *testing.T) {
	t.Parallel()
	a := renderWorkspaceManifest(manifestDocs(0.10, 0.20))
	b := renderWorkspaceManifest(manifestDocs(0.99, 0.01))
	if a != b {
		t.Errorf("manifest bytes differ for identical selection with different salience:\n%q\nvs\n%q", a, b)
	}
	if strings.Contains(a, "salience") {
		t.Errorf("manifest still renders a salience float: %q", a)
	}
	// URIs + summaries preserved.
	if !strings.Contains(a, "cog://mem/a — doc a summary") {
		t.Errorf("manifest dropped URI/summary: %q", a)
	}
}

// ── Wire safety: Anthropic path survives the normalizer ───────────────────────

func TestTrailingFovealSurvivesAnthropicNormalizer(t *testing.T) {
	t.Parallel()
	pkg := &ContextPackage{
		NucleusText:  "I am Cog.",
		ClientSystem: "sys",
		FovealDocs:   manifestDocs(0.9, 0.1),
		Conversation: []ScoredMessage{
			{Role: "user", Content: "hello"},
			{Role: "assistant", Content: "hi"},
		},
		CurrentMessage: &ProviderMessage{Role: "user", Content: "current question"},
	}
	sys, msgs := pkg.FormatForProvider()

	req := &CompletionRequest{Messages: msgs, SystemPrompt: sys}
	ar := buildAnthropicRequest("claude-sonnet-4-20250514", req, false, 8192)

	// The normalizer ran inside buildAnthropicRequest. Output must be legal.
	if vs := validateAnthropicMessages(ar.Messages); len(vs) != 0 {
		t.Fatalf("anthropic wire invalid after trailing foveal injection: %v", vs)
	}
	if len(ar.Messages) == 0 {
		t.Fatal("no messages survived normalization")
	}
	// Must end on a user turn (I7) and that turn must carry the foveal manifest.
	last := ar.Messages[len(ar.Messages)-1]
	if last.Role != "user" {
		t.Fatalf("final wire message role = %q; want user (I7)", last.Role)
	}
	if s, ok := last.Content.(string); ok {
		if !strings.Contains(s, "# Workspace Context") {
			t.Errorf("final wire user message lost the foveal manifest; got %q", s)
		}
	} else {
		t.Fatalf("final user content type = %T; want string", last.Content)
	}
}

// Multimodal path: when the current user message carries an image part, the foveal
// block must also appear as a trailing text block (Anthropic renders from
// ContentParts and ignores Content on that path).
func TestTrailingFovealFoldsIntoImageMessage(t *testing.T) {
	t.Parallel()
	pkg := &ContextPackage{
		NucleusText: "I am Cog.",
		FovealDocs:  manifestDocs(0.9, 0.1),
		CurrentMessage: &ProviderMessage{
			Role:    "user",
			Content: "look at this",
			ContentParts: []ContentPart{
				{Type: "text", Text: "look at this"},
				{Type: "image_url", ImageURL: "data:image/png;base64,AAAA"},
			},
		},
	}
	_, msgs := pkg.FormatForProvider()
	last := msgs[len(msgs)-1]

	// Content string carries the fold (OpenAI + non-image Anthropic path).
	if !strings.Contains(last.Content, "# Workspace Context") {
		t.Error("Content string missing folded foveal block")
	}
	// A trailing text ContentPart carries the fold (Anthropic image path).
	var sawFovealPart bool
	for _, p := range last.ContentParts {
		if p.Type == "text" && strings.Contains(p.Text, "# Workspace Context") {
			sawFovealPart = true
		}
	}
	if !sawFovealPart {
		t.Error("ContentParts missing folded foveal text block for the multimodal path")
	}
}

// ── Change 3: session key ─────────────────────────────────────────────────────

func TestFoveationSessionKeyPrecedence(t *testing.T) {
	t.Parallel()
	msgs := []ProviderMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "hello"},
	}

	// Header wins.
	if k := foveationSessionKey("hdr-1", "user-1", msgs); k != "sid:hdr-1" {
		t.Errorf("header should take precedence; got %q", k)
	}
	// User field second.
	if k := foveationSessionKey("", "user-1", msgs); k != "usr:user-1" {
		t.Errorf("user field should be used when no header; got %q", k)
	}
	// Fallback to leading-turns hash.
	k := foveationSessionKey("", "", msgs)
	if !strings.HasPrefix(k, "lead:") {
		t.Errorf("fallback should be a leading-turns hash; got %q", k)
	}
}

// Same conversation across two turns → SAME key. Different conversation → different.
func TestFoveationSessionKeyStableWithinConversation(t *testing.T) {
	t.Parallel()
	convTurn1 := []ProviderMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "first question"},
	}
	convTurn2 := []ProviderMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "first question"},
		{Role: "assistant", Content: "answer"},
		{Role: "user", Content: "SECOND question"},
	}
	k1 := foveationSessionKey("", "", convTurn1)
	k2 := foveationSessionKey("", "", convTurn2)
	if k1 != k2 {
		t.Errorf("same conversation must resolve to the same key across turns: %q vs %q", k1, k2)
	}

	other := []ProviderMessage{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "a completely different opening"},
	}
	if foveationSessionKey("", "", other) == k1 {
		t.Error("different conversations must resolve to different keys (no cross-conversation bleed)")
	}
}

// Guard against the historical bug: the key must never be a fresh per-request UUID.
// Two calls for the same conversation (no session id, no user) must be identical.
func TestFoveationSessionKeyNotPerRequest(t *testing.T) {
	t.Parallel()
	msgs := []ProviderMessage{{Role: "user", Content: "stable"}}
	a := foveationSessionKey("", "", msgs)
	b := foveationSessionKey("", "", msgs)
	if a != b {
		t.Fatalf("key must be stable per-conversation, not per-request: %q vs %q", a, b)
	}
}

// ── Change 3: LightConeManager TTL / pruning ──────────────────────────────────

func TestLightConeManagerPruneEvictsStale(t *testing.T) {
	t.Parallel()
	// ttl<=0 disables the background sweeper so the test is deterministic.
	m := NewLightConeManagerWithTTL(nil, 0)
	defer m.Close()

	m.Set("conv-old", &LightCone{})
	m.Set("conv-new", &LightCone{})
	if m.Count() != 2 {
		t.Fatalf("want 2 cones; got %d", m.Count())
	}

	// Age conv-old past the cutoff by rewriting its updatedAt directly is not
	// possible from outside the package cleanly, so instead prune with a cutoff in
	// the future to evict everything, then confirm Prune's contract.
	pruned := m.Prune(time.Now().Add(time.Hour))
	if pruned != 2 {
		t.Errorf("Prune(future) should evict all stale cones; pruned %d", pruned)
	}
	if m.Count() != 0 {
		t.Errorf("all cones should be evicted; got %d", m.Count())
	}
}

// The default-constructed manager runs a background sweeper; a very short TTL must
// eventually evict an idle cone. Uses NewLightConeManagerWithTTL with a tiny TTL
// and a manual Prune to avoid depending on the 5-minute sweep cadence.
func TestLightConeManagerTTLBoundsMemory(t *testing.T) {
	t.Parallel()
	m := NewLightConeManagerWithTTL(nil, time.Millisecond)
	defer m.Close()

	m.Set("conv", &LightCone{})
	// Touch is at now; wait past the TTL, then prune with now-ttl cutoff (what the
	// sweeper does).
	time.Sleep(5 * time.Millisecond)
	pruned := m.Prune(time.Now().Add(-time.Millisecond))
	if pruned != 1 {
		t.Errorf("idle cone past TTL should be pruned; pruned %d, count %d", pruned, m.Count())
	}
}

// A cone Set() after the cutoff must NOT be pruned (recently active).
func TestLightConeManagerKeepsFresh(t *testing.T) {
	t.Parallel()
	m := NewLightConeManagerWithTTL(nil, 0)
	defer m.Close()
	m.Set("fresh", &LightCone{})
	pruned := m.Prune(time.Now().Add(-time.Hour)) // cutoff in the past → nothing stale
	if pruned != 0 {
		t.Errorf("fresh cone must not be pruned; pruned %d", pruned)
	}
	if m.Get("fresh") == nil {
		t.Error("fresh cone should still be retrievable")
	}
}
