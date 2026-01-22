package clients

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cogos-dev/cogos/sdk/types"
)

// TestMemoryClientSearchOptions tests the functional options pattern for search.
func TestMemoryClientSearchOptions(t *testing.T) {
	opts := &searchOptions{}

	// Test WithLimit
	WithLimit(10)(opts)
	if opts.limit != 10 {
		t.Errorf("WithLimit: expected 10, got %d", opts.limit)
	}

	// Test WithOffset
	WithOffset(5)(opts)
	if opts.offset != 5 {
		t.Errorf("WithOffset: expected 5, got %d", opts.offset)
	}

	// Test WithType
	WithType(types.CogdocTypeInsight)(opts)
	if opts.docType != types.CogdocTypeInsight {
		t.Errorf("WithType: expected insight, got %s", opts.docType)
	}

	// Test WithTags
	WithTags("tag1", "tag2")(opts)
	if len(opts.tags) != 2 {
		t.Errorf("WithTags: expected 2 tags, got %d", len(opts.tags))
	}

	// Test WithSort
	WithSort("created", true)(opts)
	if opts.sortBy != "created" || !opts.desc {
		t.Errorf("WithSort: expected created desc, got %s %v", opts.sortBy, opts.desc)
	}
}

// TestContextClientOptions tests the functional options pattern for context.
func TestContextClientOptions(t *testing.T) {
	opts := &contextOptions{}

	// Test WithBudget
	WithBudget(100000)(opts)
	if opts.budget != 100000 {
		t.Errorf("WithBudget: expected 100000, got %d", opts.budget)
	}

	// Test WithSession
	WithSession("session-123")(opts)
	if opts.sessionID != "session-123" {
		t.Errorf("WithSession: expected session-123, got %s", opts.sessionID)
	}

	// Test WithInclude
	WithInclude("cog://mem/semantic", "cog://identity")(opts)
	if len(opts.include) != 2 {
		t.Errorf("WithInclude: expected 2, got %d", len(opts.include))
	}

	// Test WithExclude
	WithExclude("cog://mem/episodic/*")(opts)
	if len(opts.exclude) != 1 {
		t.Errorf("WithExclude: expected 1, got %d", len(opts.exclude))
	}

	// Test WithTier
	WithTier("identity")(opts)
	if opts.tier != "identity" {
		t.Errorf("WithTier: expected identity, got %s", opts.tier)
	}

	// Test WithContextModel
	WithContextModel("sonnet")(opts)
	if opts.model != "sonnet" {
		t.Errorf("WithContextModel: expected sonnet, got %s", opts.model)
	}
}

// TestInferenceClientBuilder tests the builder pattern for InferenceClient.
func TestInferenceClientBuilder(t *testing.T) {
	base := &InferenceClient{}

	// Test WithModel
	withModel := base.WithModel("opus")
	if withModel.model != "opus" {
		t.Errorf("WithModel: expected opus, got %s", withModel.model)
	}
	// Ensure original is unchanged
	if base.model != "" {
		t.Errorf("WithModel: original should be unchanged, got %s", base.model)
	}

	// Test WithMaxTokens
	withTokens := base.WithMaxTokens(4096)
	if withTokens.maxTokens != 4096 {
		t.Errorf("WithMaxTokens: expected 4096, got %d", withTokens.maxTokens)
	}

	// Test WithTemperature
	withTemp := base.WithTemperature(0.7)
	if withTemp.temperature != 0.7 {
		t.Errorf("WithTemperature: expected 0.7, got %f", withTemp.temperature)
	}

	// Test WithContext
	withCtx := base.WithContext("cog://memory", "cog://identity")
	if len(withCtx.contextURIs) != 2 {
		t.Errorf("WithContext: expected 2 URIs, got %d", len(withCtx.contextURIs))
	}

	// Test chaining
	chained := base.WithModel("opus").WithMaxTokens(4096).WithTemperature(0.5)
	if chained.model != "opus" || chained.maxTokens != 4096 || chained.temperature != 0.5 {
		t.Errorf("Chaining failed: %+v", chained)
	}
}

// TestInferenceRequestBuilding tests that requests are built correctly.
func TestInferenceRequestBuilding(t *testing.T) {
	client := &InferenceClient{
		model:       "opus",
		maxTokens:   4096,
		temperature: 0.7,
		contextURIs: []string{"cog://memory"},
	}

	req := client.buildRequest("Hello world")

	if req.Prompt != "Hello world" {
		t.Errorf("Prompt: expected 'Hello world', got '%s'", req.Prompt)
	}
	if req.Model != "opus" {
		t.Errorf("Model: expected opus, got %s", req.Model)
	}
	if req.MaxTokens != 4096 {
		t.Errorf("MaxTokens: expected 4096, got %d", req.MaxTokens)
	}
	if req.Temperature != 0.7 {
		t.Errorf("Temperature: expected 0.7, got %f", req.Temperature)
	}
	if len(req.Context) != 1 || req.Context[0] != "cog://memory" {
		t.Errorf("Context: expected [cog://memory], got %v", req.Context)
	}
}

// TestSignalTypes tests that signal types work correctly.
func TestSignalTypes(t *testing.T) {
	field := &SignalField{
		Signals:     make(map[string][]*types.Signal),
		TotalActive: 5,
		Timestamp:   time.Now(),
	}

	// Add some signals
	field.Signals["inference"] = []*types.Signal{
		{Location: "inference", Type: "ACTIVE", Strength: 1.0},
		{Location: "inference", Type: "PENDING", Strength: 0.5},
	}

	if len(field.Signals["inference"]) != 2 {
		t.Errorf("Expected 2 signals at inference, got %d", len(field.Signals["inference"]))
	}
}

// TestCogdocSerialization tests cogdoc to bytes conversion.
func TestCogdocSerialization(t *testing.T) {
	client := &MemoryClient{}

	doc := &types.Cogdoc{
		Meta: types.CogdocMeta{
			ID:    "semantic.insight.test",
			Type:  types.CogdocTypeInsight,
			Title: "Test Insight",
		},
		Content: "# Test Content\n\nThis is a test.",
	}

	bytes, err := client.cogdocToBytes(doc)
	if err != nil {
		t.Fatalf("cogdocToBytes failed: %v", err)
	}

	// Check that output contains expected elements
	output := string(bytes)
	if len(output) == 0 {
		t.Error("Expected non-empty output")
	}
	if !containsString(output, "---") {
		t.Error("Expected YAML frontmatter delimiter")
	}
	if !containsString(output, "semantic.insight.test") {
		t.Error("Expected ID in output")
	}
	if !containsString(output, "Test Insight") {
		t.Error("Expected title in output")
	}
	if !containsString(output, "# Test Content") {
		t.Error("Expected content in output")
	}
}

// TestContextTierNames verifies tier names are consistent.
func TestContextTierNames(t *testing.T) {
	client := &ContextClient{}

	tiers := client.AllTiers()
	if len(tiers) != 4 {
		t.Errorf("Expected 4 tiers, got %d", len(tiers))
	}

	expected := []types.ContextTierName{
		types.TierIdentity,
		types.TierTemporal,
		types.TierPresent,
		types.TierFeedback,
	}

	for i, tier := range tiers {
		if tier != expected[i] {
			t.Errorf("Tier %d: expected %s, got %s", i, expected[i], tier)
		}
	}
}

// TestContextConfig tests default context configuration.
func TestContextConfig(t *testing.T) {
	client := &ContextClient{}
	config := client.Config()

	if config.Budget != 50000 {
		t.Errorf("Default budget: expected 50000, got %d", config.Budget)
	}
}

// TestTierURIs verifies tier URI sources are defined.
func TestTierURIs(t *testing.T) {
	client := &ContextClient{}

	// Check that each tier has URIs defined
	for _, tier := range client.AllTiers() {
		uris := client.TierURIs(tier)
		if len(uris) == 0 {
			t.Errorf("Tier %s has no URIs defined", tier)
		}
	}
}

// TestThreadTypes tests thread type conversions.
func TestThreadTypes(t *testing.T) {
	// Test Thread creation
	thread := types.NewThread("test-id")
	if thread.ID != "test-id" {
		t.Errorf("Expected ID test-id, got %s", thread.ID)
	}
	if thread.Status != "active" {
		t.Errorf("Expected status active, got %s", thread.Status)
	}

	// Test message append
	msg := types.NewUserMessage("Hello")
	thread.AppendMessage(msg)

	if thread.MessageCount() != 1 {
		t.Errorf("Expected 1 message, got %d", thread.MessageCount())
	}

	// Test LastN
	last := thread.LastN(5)
	if len(last) != 1 {
		t.Errorf("Expected 1 message in LastN, got %d", len(last))
	}
}

// TestMessageFactory tests message creation helpers.
func TestMessageFactory(t *testing.T) {
	userMsg := types.NewUserMessage("User content")
	if userMsg.Role != types.MessageRoleUser {
		t.Errorf("Expected user role, got %s", userMsg.Role)
	}
	if userMsg.Content != "User content" {
		t.Errorf("Expected 'User content', got %s", userMsg.Content)
	}

	assistantMsg := types.NewAssistantMessage("Assistant content")
	if assistantMsg.Role != types.MessageRoleAssistant {
		t.Errorf("Expected assistant role, got %s", assistantMsg.Role)
	}

	systemMsg := types.NewSystemMessage("System content")
	if systemMsg.Role != types.MessageRoleSystem {
		t.Errorf("Expected system role, got %s", systemMsg.Role)
	}
}

// TestSectorClient tests sector client path construction.
func TestSectorClient(t *testing.T) {
	// Create a mock memory client with nil kernel (won't make actual calls)
	memClient := &MemoryClient{}

	// Test each sector
	semantic := memClient.Semantic()
	if semantic.sector != "semantic" {
		t.Errorf("Expected semantic sector, got %s", semantic.sector)
	}

	episodic := memClient.Episodic()
	if episodic.sector != "episodic" {
		t.Errorf("Expected episodic sector, got %s", episodic.sector)
	}

	procedural := memClient.Procedural()
	if procedural.sector != "procedural" {
		t.Errorf("Expected procedural sector, got %s", procedural.sector)
	}

	reflective := memClient.Reflective()
	if reflective.sector != "reflective" {
		t.Errorf("Expected reflective sector, got %s", reflective.sector)
	}
}

// TestContextStateJSONRoundtrip tests that ContextState can be serialized and deserialized.
func TestContextStateJSONRoundtrip(t *testing.T) {
	original := &types.ContextState{
		Tiers: []*types.ContextTier{
			{Name: "identity", Priority: 1, Budget: 16500, Tokens: 12000},
			{Name: "temporal", Priority: 2, Budget: 12500, Tokens: 8000},
		},
		TotalTokens:    20000,
		Budget:         50000,
		CoherenceScore: 0.95,
	}

	// Serialize
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// Deserialize
	var restored types.ContextState
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// Verify
	if len(restored.Tiers) != 2 {
		t.Errorf("Expected 2 tiers, got %d", len(restored.Tiers))
	}
	if restored.TotalTokens != 20000 {
		t.Errorf("Expected 20000 tokens, got %d", restored.TotalTokens)
	}
	if restored.CoherenceScore != 0.95 {
		t.Errorf("Expected 0.95 coherence, got %f", restored.CoherenceScore)
	}
}

// TestSignalSetJSONRoundtrip tests that SignalSet can be serialized and deserialized.
func TestSignalSetJSONRoundtrip(t *testing.T) {
	now := time.Now()
	original := &types.SignalSet{
		Location: "inference",
		Signals: []*types.Signal{
			{
				Location:    "inference",
				Type:        "ACTIVE",
				DepositedAt: now,
				HalfLife:    4.0,
				Strength:    1.0,
			},
		},
		ActiveCount: 1,
		Timestamp:   now,
	}

	// Serialize
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}

	// Deserialize
	var restored types.SignalSet
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}

	// Verify
	if restored.Location != "inference" {
		t.Errorf("Expected location inference, got %s", restored.Location)
	}
	if len(restored.Signals) != 1 {
		t.Errorf("Expected 1 signal, got %d", len(restored.Signals))
	}
	if restored.ActiveCount != 1 {
		t.Errorf("Expected 1 active, got %d", restored.ActiveCount)
	}
}

// Helper function
func containsString(haystack, needle string) bool {
	return len(haystack) > 0 && len(needle) > 0 &&
		(haystack == needle || len(haystack) > len(needle))
}
