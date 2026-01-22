package sdk

import (
	"context"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/cogos-dev/cogos/sdk/types"
)

// contextProjector handles cog://context namespace.
// Projects the 4-tier context pipeline for inference.
type contextProjector struct {
	BaseProjector
	kernel *Kernel
}

// Resolve assembles the four-tier context.
//
// Query parameters:
//   - budget: total token budget (default: 50000)
//   - tier: filter to specific tier (identity, temporal, present, feedback)
//   - include: comma-separated URIs to force-include
//   - exclude: comma-separated URI patterns to exclude
//   - session: session ID for temporal context
//
// Example URIs:
//
//	cog://context                     -> Full 4-tier context
//	cog://context?budget=30000        -> With custom budget
//	cog://context?tier=identity       -> Identity tier only
//	cog://context?include=cog://adr/021,cog://adr/033
func (p *contextProjector) Resolve(ctx context.Context, uri *ParsedURI) (*Resource, error) {
	// Parse configuration from query params
	config := types.DefaultContextConfig()

	if budget := uri.GetQueryInt("budget", 0); budget > 0 {
		config.Budget = budget
	}

	if tier := uri.GetQuery("tier"); tier != "" {
		config.Tier = tier
	}

	if include := uri.GetQuery("include"); include != "" {
		config.Include = strings.Split(include, ",")
	}

	if exclude := uri.GetQuery("exclude"); exclude != "" {
		config.Exclude = strings.Split(exclude, ",")
	}

	if session := uri.GetQuery("session"); session != "" {
		config.SessionID = session
	}

	// Assemble context
	contextState, err := p.assembleContext(ctx, config)
	if err != nil {
		return nil, err
	}

	// Create response
	return NewJSONResource(uri.Raw, contextState)
}

// assembleContext builds the 4-tier context state.
func (p *contextProjector) assembleContext(ctx context.Context, config *types.ContextConfig) (*types.ContextState, error) {
	state := &types.ContextState{
		Tiers:       make([]*types.ContextTier, 0, 4),
		Budget:      config.Budget,
		AssembledAt: time.Now(),
	}

	// Calculate budget allocations
	tierBudgets := p.calculateBudgets(config)

	// Determine which tiers to include
	tiersToInclude := []types.ContextTierName{
		types.TierIdentity,
		types.TierTemporal,
		types.TierPresent,
		types.TierFeedback,
	}

	if config.Tier != "" {
		tiersToInclude = []types.ContextTierName{types.ContextTierName(config.Tier)}
	}

	// Assemble each tier
	for _, tierName := range tiersToInclude {
		tier, err := p.assembleTier(ctx, tierName, tierBudgets[tierName], config)
		if err != nil {
			// Log error but continue with other tiers
			continue
		}
		state.Tiers = append(state.Tiers, tier)
		state.TotalTokens += tier.Tokens
	}

	// Get coherence score
	coherenceRes, err := p.kernel.ResolveContext(ctx, "cog://coherence")
	if err == nil {
		var coherence map[string]any
		if err := coherenceRes.JSON(&coherence); err == nil {
			if coherent, ok := coherence["coherent"].(bool); ok && coherent {
				state.CoherenceScore = 1.0
			} else {
				state.CoherenceScore = 0.5
			}
		}
	}

	return state, nil
}

// calculateBudgets computes token budgets for each tier.
func (p *contextProjector) calculateBudgets(config *types.ContextConfig) map[types.ContextTierName]int {
	percentages := make(map[types.ContextTierName]int)
	maps.Copy(percentages, types.DefaultTierBudgets)
	if config.TierBudgets != nil {
		maps.Copy(percentages, config.TierBudgets)
	}

	result := make(map[types.ContextTierName]int)
	for tier, pct := range percentages {
		result[tier] = config.Budget * pct / 100
	}
	return result
}

// assembleTier builds a single context tier.
func (p *contextProjector) assembleTier(ctx context.Context, tierName types.ContextTierName, budget int, config *types.ContextConfig) (*types.ContextTier, error) {
	tier := &types.ContextTier{
		Name:     string(tierName),
		Priority: types.DefaultTierPriorities[tierName],
		Budget:   budget,
		URIs:     make([]string, 0),
	}

	// Get default URIs for this tier
	uris := types.ContextURISource[tierName]

	// Add any force-included URIs
	for _, uri := range config.Include {
		// Check if this URI belongs to this tier (simple heuristic)
		if p.uriMatchesTier(uri, tierName) {
			uris = append(uris, uri)
		}
	}

	// Filter out excluded URIs
	filteredURIs := make([]string, 0, len(uris))
	for _, uri := range uris {
		if !p.isExcluded(uri, config.Exclude) {
			filteredURIs = append(filteredURIs, uri)
		}
	}

	tier.URIs = filteredURIs

	// Resolve each URI and collect content
	var contentParts []string
	resolvedCount := 0

	for _, uriStr := range filteredURIs {
		resource, err := p.kernel.ResolveContext(ctx, uriStr)
		if err != nil {
			// Skip failed URIs
			continue
		}

		content := p.extractContent(resource)
		if content != "" {
			contentParts = append(contentParts, content)
			resolvedCount++
		}
	}

	// Join content
	fullContent := strings.Join(contentParts, "\n\n")

	// Apply budget truncation
	tier.Content, tier.Truncated = TruncateToTokens(fullContent, budget)
	tier.Tokens = EstimateTokens(tier.Content)
	tier.Source = strings.Join(filteredURIs, ", ")

	return tier, nil
}

// extractContent gets string content from a resource.
func (p *contextProjector) extractContent(resource *Resource) string {
	if resource == nil {
		return ""
	}

	// For collections, summarize children
	if resource.IsCollection() {
		var parts []string
		for _, child := range resource.Children {
			if name, ok := child.GetMetadata("name"); ok {
				parts = append(parts, name.(string))
			}
		}
		if len(parts) > 0 {
			return "Available: " + strings.Join(parts, ", ")
		}
		return ""
	}

	// For content, return as string
	return string(resource.Content)
}

// uriMatchesTier checks if a URI belongs to a tier (heuristic).
func (p *contextProjector) uriMatchesTier(uri string, tier types.ContextTierName) bool {
	switch tier {
	case types.TierIdentity:
		return strings.Contains(uri, "identity") ||
			strings.Contains(uri, "roles") ||
			strings.Contains(uri, "agents")
	case types.TierTemporal:
		return strings.Contains(uri, "handoff") ||
			strings.Contains(uri, "ledger") ||
			strings.Contains(uri, "session")
	case types.TierPresent:
		return strings.Contains(uri, "thread") ||
			strings.Contains(uri, "signals")
	case types.TierFeedback:
		return strings.Contains(uri, "coherence") ||
			strings.Contains(uri, "decision")
	}
	return false
}

// isExcluded checks if a URI matches any exclusion pattern.
func (p *contextProjector) isExcluded(uri string, patterns []string) bool {
	for _, pattern := range patterns {
		// Simple prefix matching for now
		if strings.HasPrefix(uri, pattern) || strings.Contains(uri, pattern) {
			return true
		}
	}
	return false
}

// ContextBuilder provides a fluent interface for building context.
type ContextBuilder struct {
	kernel  *Kernel
	config  *types.ContextConfig
	include []string
	exclude []string
}

// NewContextBuilder creates a new context builder.
func NewContextBuilder(kernel *Kernel) *ContextBuilder {
	return &ContextBuilder{
		kernel:  kernel,
		config:  types.DefaultContextConfig(),
		include: make([]string, 0),
		exclude: make([]string, 0),
	}
}

// WithBudget sets the token budget.
func (b *ContextBuilder) WithBudget(budget int) *ContextBuilder {
	b.config.Budget = budget
	return b
}

// WithTier filters to a specific tier.
func (b *ContextBuilder) WithTier(tier types.ContextTierName) *ContextBuilder {
	b.config.Tier = string(tier)
	return b
}

// Include adds URIs to include.
func (b *ContextBuilder) Include(uris ...string) *ContextBuilder {
	b.include = append(b.include, uris...)
	return b
}

// Exclude adds URI patterns to exclude.
func (b *ContextBuilder) Exclude(patterns ...string) *ContextBuilder {
	b.exclude = append(b.exclude, patterns...)
	return b
}

// WithSession sets the session ID for temporal context.
func (b *ContextBuilder) WithSession(sessionID string) *ContextBuilder {
	b.config.SessionID = sessionID
	return b
}

// Build assembles the context.
func (b *ContextBuilder) Build(ctx context.Context) (*types.ContextState, error) {
	// Build query string
	var parts []string
	parts = append(parts, "cog://context")

	var queryParts []string
	if b.config.Budget != 50000 {
		queryParts = append(queryParts, "budget="+string(rune(b.config.Budget)))
	}
	if b.config.Tier != "" {
		queryParts = append(queryParts, "tier="+b.config.Tier)
	}
	if len(b.include) > 0 {
		queryParts = append(queryParts, "include="+strings.Join(b.include, ","))
	}
	if len(b.exclude) > 0 {
		queryParts = append(queryParts, "exclude="+strings.Join(b.exclude, ","))
	}

	uri := "cog://context"
	if len(queryParts) > 0 {
		uri += "?" + strings.Join(queryParts, "&")
	}

	resource, err := b.kernel.ResolveContext(ctx, uri)
	if err != nil {
		return nil, err
	}

	var state types.ContextState
	if err := resource.JSON(&state); err != nil {
		return nil, err
	}

	return &state, nil
}

// SortTiersByPriority sorts tiers by their priority (ascending).
func SortTiersByPriority(tiers []*types.ContextTier) {
	sort.Slice(tiers, func(i, j int) bool {
		return tiers[i].Priority < tiers[j].Priority
	})
}
