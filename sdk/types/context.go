package types

import (
	"encoding/json"
	"time"
)

// ContextTier represents a single tier of the 4-tier context pipeline.
// Tiers are prioritized and budget-allocated for optimal inference.
type ContextTier struct {
	// Name is the tier identifier (identity, temporal, present, feedback).
	Name string `json:"name"`

	// Priority is the tier's priority (1 = highest).
	Priority int `json:"priority"`

	// Budget is the token budget allocation for this tier.
	Budget int `json:"budget"`

	// URIs is the list of cog:// URIs contributing to this tier.
	URIs []string `json:"uris"`

	// Content is the assembled content from resolved URIs.
	Content string `json:"content,omitempty"`

	// Tokens is the estimated token count of the content.
	Tokens int `json:"tokens"`

	// Source describes where this tier content came from.
	Source string `json:"source,omitempty"`

	// Truncated indicates if content was truncated to fit budget.
	Truncated bool `json:"truncated,omitempty"`
}

// ContextTierName defines the four context tiers.
type ContextTierName string

const (
	// TierIdentity is the stable identity tier (~1/3 of budget).
	// Contains: cog://identity, agent cards, roles
	TierIdentity ContextTierName = "identity"

	// TierTemporal is the session-aware tier.
	// Contains: current session, recent handoffs
	TierTemporal ContextTierName = "temporal"

	// TierPresent is the current interaction tier.
	// Contains: active threads, relevant signals
	TierPresent ContextTierName = "present"

	// TierFeedback is the coherence feedback tier.
	// Contains: coherence state, recent decisions
	TierFeedback ContextTierName = "feedback"
)

// DefaultTierPriorities defines the default priority ordering.
var DefaultTierPriorities = map[ContextTierName]int{
	TierIdentity: 1,
	TierTemporal: 2,
	TierPresent:  3,
	TierFeedback: 4,
}

// DefaultTierBudgets defines the default budget allocation (as percentages).
// Total should sum to 100.
var DefaultTierBudgets = map[ContextTierName]int{
	TierIdentity: 33, // ~1/3 of budget for stable identity
	TierTemporal: 25, // Session state
	TierPresent:  30, // Current interaction
	TierFeedback: 12, // Coherence feedback
}

// ContextConfig configures the context projection.
type ContextConfig struct {
	// Budget is the total token budget (default: 50000).
	Budget int `json:"budget"`

	// TierBudgets overrides default tier budget percentages.
	TierBudgets map[ContextTierName]int `json:"tier_budgets,omitempty"`

	// Include is a list of URIs to force-include.
	Include []string `json:"include,omitempty"`

	// Exclude is a list of URI patterns to exclude.
	Exclude []string `json:"exclude,omitempty"`

	// Tier filters to a specific tier (empty = all tiers).
	Tier string `json:"tier,omitempty"`

	// SessionID is the current session for temporal context.
	SessionID string `json:"session_id,omitempty"`
}

// DefaultContextConfig returns the default context configuration.
func DefaultContextConfig() *ContextConfig {
	return &ContextConfig{
		Budget: 50000,
	}
}

// ContextState represents the assembled four-tier context.
type ContextState struct {
	// Tiers contains all four context tiers.
	Tiers []*ContextTier `json:"tiers"`

	// TotalTokens is the sum of all tier token counts.
	TotalTokens int `json:"total_tokens"`

	// Budget is the token budget this context was built for.
	Budget int `json:"budget"`

	// Model is an optional model override from context.
	Model string `json:"model,omitempty"`

	// CoherenceScore is the workspace coherence at assembly time.
	CoherenceScore float64 `json:"coherence_score"`

	// AssembledAt is when this context was assembled.
	AssembledAt time.Time `json:"assembled_at"`

	// ShouldRefresh indicates if context should be refreshed.
	ShouldRefresh bool `json:"should_refresh,omitempty"`

	// Schema is an optional JSON schema for structured output.
	Schema json.RawMessage `json:"schema,omitempty"`

	// AllowedTools restricts which tools can be used.
	AllowedTools []string `json:"allowed_tools,omitempty"`

	// DisallowedTools blocks specific tools.
	DisallowedTools []string `json:"disallowed_tools,omitempty"`
}

// GetTier returns a tier by name.
func (cs *ContextState) GetTier(name ContextTierName) *ContextTier {
	for _, tier := range cs.Tiers {
		if tier.Name == string(name) {
			return tier
		}
	}
	return nil
}

// BuildContextString assembles the full context string from tiers.
// Tiers are joined with section separators.
func (cs *ContextState) BuildContextString() string {
	if cs == nil || len(cs.Tiers) == 0 {
		return ""
	}

	result := ""
	for i, tier := range cs.Tiers {
		if tier.Content == "" {
			continue
		}
		if i > 0 && result != "" {
			result += "\n\n---\n\n"
		}
		result += tier.Content
	}
	return result
}

// ContextMetrics captures metrics about assembled context.
type ContextMetrics struct {
	// TotalTokens is the total token count.
	TotalTokens int `json:"total_tokens"`

	// TierBreakdown maps tier names to token counts.
	TierBreakdown map[string]int `json:"tier_breakdown"`

	// CoherenceScore is the workspace coherence score.
	CoherenceScore float64 `json:"coherence_score"`

	// CompressionUsed indicates if compression was applied.
	CompressionUsed bool `json:"compression_used"`

	// URIsResolved is the count of URIs successfully resolved.
	URIsResolved int `json:"uris_resolved"`

	// URIsFailed is the count of URIs that failed to resolve.
	URIsFailed int `json:"uris_failed"`
}

// BuildMetrics creates metrics from the context state.
func (cs *ContextState) BuildMetrics() *ContextMetrics {
	if cs == nil {
		return nil
	}

	tierBreakdown := make(map[string]int)
	for _, tier := range cs.Tiers {
		tierBreakdown[tier.Name] = tier.Tokens
	}

	return &ContextMetrics{
		TotalTokens:    cs.TotalTokens,
		TierBreakdown:  tierBreakdown,
		CoherenceScore: cs.CoherenceScore,
	}
}

// ContextURISource maps tier names to their default URI sources.
var ContextURISource = map[ContextTierName][]string{
	TierIdentity: {
		"cog://identity",
		"cog://roles",
		"cog://agents",
	},
	TierTemporal: {
		"cog://handoffs",
		"cog://ledger",
	},
	TierPresent: {
		"cog://thread/current",
		"cog://signals",
	},
	TierFeedback: {
		"cog://coherence",
		"cog://mem/episodic/decisions",
	},
}
