package sdk

import (
	"slices"
	"testing"

	"github.com/cogos-dev/cogos/sdk/types"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		expected int
		delta    int // allowable variance
	}{
		{"empty", "", 0, 0},
		{"short", "hello", 2, 1},
		{"medium", "hello world this is a test", 7, 2},
		{"exact 4 chars", "abcd", 1, 0},
		{"8 chars", "abcdefgh", 2, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EstimateTokens(tt.text)
			if got < tt.expected-tt.delta || got > tt.expected+tt.delta {
				t.Errorf("EstimateTokens(%q) = %d, want ~%d", tt.text, got, tt.expected)
			}
		})
	}
}

func TestTruncateToTokens(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		maxTokens int
		truncated bool
	}{
		{"no truncation needed", "short text", 100, false},
		{"exact fit", "abcd", 1, false},
		{"needs truncation", "this is a much longer piece of text that should be truncated", 5, true},
		{"zero budget", "anything", 0, false}, // 0 means no limit
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, wasTruncated := TruncateToTokens(tt.content, tt.maxTokens)
			if wasTruncated != tt.truncated {
				t.Errorf("TruncateToTokens() truncated = %v, want %v", wasTruncated, tt.truncated)
			}
			if wasTruncated && !stringContains(result, "...") {
				t.Error("Truncated content should end with ...")
			}
		})
	}
}

func TestAllocateBudget(t *testing.T) {
	priorities := map[string]int{
		"high":   1,
		"medium": 2,
		"low":    3,
	}

	allocations := AllocateBudget(1000, priorities)

	if len(allocations) != 3 {
		t.Errorf("Expected 3 allocations, got %d", len(allocations))
	}

	// High priority should get more than low priority
	if allocations["high"] <= allocations["low"] {
		t.Errorf("High priority (%d) should be > low priority (%d)",
			allocations["high"], allocations["low"])
	}

	// Total should equal budget
	total := allocations["high"] + allocations["medium"] + allocations["low"]
	if total != 1000 {
		t.Errorf("Total allocation = %d, want 1000", total)
	}
}

func TestAllocateBudgetByPercentage(t *testing.T) {
	percentages := map[string]int{
		"identity": 33,
		"temporal": 25,
		"present":  30,
		"feedback": 12,
	}

	allocations := AllocateBudgetByPercentage(50000, percentages)

	if allocations["identity"] != 16500 {
		t.Errorf("identity allocation = %d, want 16500", allocations["identity"])
	}

	if allocations["temporal"] != 12500 {
		t.Errorf("temporal allocation = %d, want 12500", allocations["temporal"])
	}
}

func TestCompactContent(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{"empty", "", ""},
		{"no change needed", "hello world", "hello world"},
		{"trim whitespace", "  hello  ", "hello"},
		{"collapse blank lines", "a\n\n\n\nb", "a\n\nb"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CompactContent(tt.input)
			if got != tt.expect {
				t.Errorf("CompactContent(%q) = %q, want %q", tt.input, got, tt.expect)
			}
		})
	}
}

func TestAnalyzeContent(t *testing.T) {
	content := "Hello world.\nThis is a test."
	stats := AnalyzeContent(content)

	if stats.Lines != 2 {
		t.Errorf("Lines = %d, want 2", stats.Lines)
	}
	if stats.Words != 6 {
		t.Errorf("Words = %d, want 6", stats.Words)
	}
	if stats.Tokens == 0 {
		t.Error("Tokens should be > 0")
	}
}

func TestContextTierDefaults(t *testing.T) {
	// Check default priorities
	if types.DefaultTierPriorities[types.TierIdentity] != 1 {
		t.Error("Identity tier should have priority 1")
	}
	if types.DefaultTierPriorities[types.TierFeedback] != 4 {
		t.Error("Feedback tier should have priority 4")
	}

	// Check default budgets sum to 100
	total := 0
	for _, pct := range types.DefaultTierBudgets {
		total += pct
	}
	if total != 100 {
		t.Errorf("Default tier budgets sum to %d, want 100", total)
	}
}

func TestContextConfig(t *testing.T) {
	config := types.DefaultContextConfig()

	if config.Budget != 50000 {
		t.Errorf("Default budget = %d, want 50000", config.Budget)
	}
}

func TestContextState(t *testing.T) {
	state := &types.ContextState{
		Tiers: []*types.ContextTier{
			{Name: "identity", Content: "Identity content", Priority: 1, Tokens: 100},
			{Name: "temporal", Content: "Temporal content", Priority: 2, Tokens: 50},
		},
		TotalTokens: 150,
		Budget:      50000,
	}

	// Test GetTier
	identity := state.GetTier(types.TierIdentity)
	if identity == nil {
		t.Fatal("GetTier(identity) returned nil")
	}
	if identity.Content != "Identity content" {
		t.Error("Wrong content for identity tier")
	}

	// Test non-existent tier
	missing := state.GetTier(types.TierFeedback)
	if missing != nil {
		t.Error("GetTier(feedback) should return nil")
	}

	// Test BuildContextString
	contextStr := state.BuildContextString()
	if !stringContains(contextStr, "Identity content") {
		t.Error("Context string should contain identity content")
	}
	if !stringContains(contextStr, "Temporal content") {
		t.Error("Context string should contain temporal content")
	}
	if !stringContains(contextStr, "---") {
		t.Error("Context string should contain tier separator")
	}

	// Test BuildMetrics
	metrics := state.BuildMetrics()
	if metrics.TotalTokens != 150 {
		t.Errorf("TotalTokens = %d, want 150", metrics.TotalTokens)
	}
	if metrics.TierBreakdown["identity"] != 100 {
		t.Error("Identity tier token count wrong in breakdown")
	}
}

func TestContextURISource(t *testing.T) {
	// Check each tier has URIs
	for tier, uris := range types.ContextURISource {
		if len(uris) == 0 {
			t.Errorf("Tier %s has no default URIs", tier)
		}
	}

	// Check identity tier includes identity URI
	identityURIs := types.ContextURISource[types.TierIdentity]
	if !slices.Contains(identityURIs, "cog://identity") {
		t.Error("Identity tier should include cog://identity")
	}
}

func TestSortTiersByPriority(t *testing.T) {
	tiers := []*types.ContextTier{
		{Name: "feedback", Priority: 4},
		{Name: "identity", Priority: 1},
		{Name: "present", Priority: 3},
		{Name: "temporal", Priority: 2},
	}

	SortTiersByPriority(tiers)

	expected := []string{"identity", "temporal", "present", "feedback"}
	for i, tier := range tiers {
		if tier.Name != expected[i] {
			t.Errorf("After sort, tier[%d] = %s, want %s", i, tier.Name, expected[i])
		}
	}
}

// stringContains is a simple contains check without importing strings
func stringContains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
