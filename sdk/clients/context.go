package clients

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/cogos-dev/cogos/sdk"
	"github.com/cogos-dev/cogos/sdk/types"
)

// ContextClient builds context for inference.
//
// The context system uses a 4-tier priority model:
//   - Identity (P1): Stable identity, roles, agents (~33% of budget)
//   - Temporal (P2): Session state, handoffs (~25% of budget)
//   - Present (P3): Current thread, signals (~30% of budget)
//   - Feedback (P4): Coherence state, recent decisions (~12% of budget)
//
// All methods are goroutine-safe.
type ContextClient struct {
	kernel *sdk.Kernel
}

// NewContextClient creates a new ContextClient.
func NewContextClient(k *sdk.Kernel) *ContextClient {
	return &ContextClient{kernel: k}
}

// ContextOption configures context building behavior.
type ContextOption func(*contextOptions)

type contextOptions struct {
	budget    int
	sessionID string
	include   []string
	exclude   []string
	tier      string
	model     string
}

// WithBudget sets the total token budget for context.
func WithBudget(tokens int) ContextOption {
	return func(o *contextOptions) {
		o.budget = tokens
	}
}

// WithSession sets the session ID for temporal context.
func WithSession(id string) ContextOption {
	return func(o *contextOptions) {
		o.sessionID = id
	}
}

// WithInclude adds URIs to force-include in context.
func WithInclude(uris ...string) ContextOption {
	return func(o *contextOptions) {
		o.include = append(o.include, uris...)
	}
}

// WithExclude adds URI patterns to exclude from context.
func WithExclude(patterns ...string) ContextOption {
	return func(o *contextOptions) {
		o.exclude = append(o.exclude, patterns...)
	}
}

// WithTier filters to a specific tier only.
func WithTier(tier string) ContextOption {
	return func(o *contextOptions) {
		o.tier = tier
	}
}

// WithContextModel sets an optional model override in the context.
func WithContextModel(model string) ContextOption {
	return func(o *contextOptions) {
		o.model = model
	}
}

// Build assembles the full 4-tier context for inference.
//
// Example:
//
//	ctx, err := c.Context.Build(WithBudget(50000))
//	fmt.Printf("Total tokens: %d\n", ctx.TotalTokens)
//	fmt.Println(ctx.BuildContextString())
func (c *ContextClient) Build(opts ...ContextOption) (*types.ContextState, error) {
	return c.BuildContext(context.Background(), opts...)
}

// BuildContext is like Build but accepts a context.
func (c *ContextClient) BuildContext(ctx context.Context, opts ...ContextOption) (*types.ContextState, error) {
	// Apply options
	o := &contextOptions{
		budget: 50000, // Default budget
	}
	for _, opt := range opts {
		opt(o)
	}

	// Build query string
	params := url.Values{}
	if o.budget > 0 {
		params.Set("budget", fmt.Sprintf("%d", o.budget))
	}
	if o.sessionID != "" {
		params.Set("session", o.sessionID)
	}
	if o.tier != "" {
		params.Set("tier", o.tier)
	}
	if o.model != "" {
		params.Set("model", o.model)
	}
	for _, uri := range o.include {
		params.Add("include", uri)
	}
	for _, pattern := range o.exclude {
		params.Add("exclude", pattern)
	}

	uri := "cog://context"
	if len(params) > 0 {
		uri = fmt.Sprintf("cog://context?%s", params.Encode())
	}

	resource, err := c.kernel.ResolveContext(ctx, uri)
	if err != nil {
		return nil, err
	}

	var state types.ContextState
	if err := json.Unmarshal(resource.Content, &state); err != nil {
		return nil, fmt.Errorf("parse context: %w", err)
	}

	return &state, nil
}

// Identity returns just the identity tier (P1).
// This contains stable identity, roles, and agent definitions.
//
// Example:
//
//	tier, err := c.Context.Identity()
//	fmt.Println(tier.Content)
func (c *ContextClient) Identity() (*types.ContextTier, error) {
	return c.IdentityContext(context.Background())
}

// IdentityContext is like Identity but accepts a context.
func (c *ContextClient) IdentityContext(ctx context.Context) (*types.ContextTier, error) {
	return c.getTier(ctx, types.TierIdentity)
}

// Temporal returns just the temporal tier (P2).
// This contains session state and handoff documents.
//
// Example:
//
//	tier, err := c.Context.Temporal()
//	fmt.Printf("Temporal context: %d tokens\n", tier.Tokens)
func (c *ContextClient) Temporal() (*types.ContextTier, error) {
	return c.TemporalContext(context.Background())
}

// TemporalContext is like Temporal but accepts a context.
func (c *ContextClient) TemporalContext(ctx context.Context) (*types.ContextTier, error) {
	return c.getTier(ctx, types.TierTemporal)
}

// Present returns just the present tier (P3).
// This contains current thread and active signals.
//
// Example:
//
//	tier, err := c.Context.Present()
//	fmt.Println(tier.Content)
func (c *ContextClient) Present() (*types.ContextTier, error) {
	return c.PresentContext(context.Background())
}

// PresentContext is like Present but accepts a context.
func (c *ContextClient) PresentContext(ctx context.Context) (*types.ContextTier, error) {
	return c.getTier(ctx, types.TierPresent)
}

// Feedback returns just the feedback tier (P4).
// This contains coherence state and recent decisions.
//
// Example:
//
//	tier, err := c.Context.Feedback()
//	fmt.Println(tier.Content)
func (c *ContextClient) Feedback() (*types.ContextTier, error) {
	return c.FeedbackContext(context.Background())
}

// FeedbackContext is like Feedback but accepts a context.
func (c *ContextClient) FeedbackContext(ctx context.Context) (*types.ContextTier, error) {
	return c.getTier(ctx, types.TierFeedback)
}

// getTier fetches a specific tier from the context projector.
func (c *ContextClient) getTier(ctx context.Context, tier types.ContextTierName) (*types.ContextTier, error) {
	uri := fmt.Sprintf("cog://context?tier=%s", tier)
	resource, err := c.kernel.ResolveContext(ctx, uri)
	if err != nil {
		return nil, err
	}

	// Try to parse as a single tier
	var tierObj types.ContextTier
	if err := json.Unmarshal(resource.Content, &tierObj); err == nil && tierObj.Name != "" {
		return &tierObj, nil
	}

	// Fall back to parsing as full context and extracting tier
	var state types.ContextState
	if err := json.Unmarshal(resource.Content, &state); err != nil {
		return nil, fmt.Errorf("parse context: %w", err)
	}

	for _, t := range state.Tiers {
		if t.Name == string(tier) {
			return t, nil
		}
	}

	return nil, fmt.Errorf("tier %s not found", tier)
}

// Metrics returns metrics about the last built context.
//
// Example:
//
//	metrics, err := c.Context.Metrics()
//	fmt.Printf("Total tokens: %d, Coherence: %.2f\n", metrics.TotalTokens, metrics.CoherenceScore)
func (c *ContextClient) Metrics() (*types.ContextMetrics, error) {
	return c.MetricsContext(context.Background())
}

// MetricsContext is like Metrics but accepts a context.
func (c *ContextClient) MetricsContext(ctx context.Context) (*types.ContextMetrics, error) {
	state, err := c.BuildContext(ctx)
	if err != nil {
		return nil, err
	}
	return state.BuildMetrics(), nil
}

// EstimateBudget estimates the token budget needed for the given URIs.
// Useful for planning context before building it.
func (c *ContextClient) EstimateBudget(uris ...string) (int, error) {
	return c.EstimateBudgetContext(context.Background(), uris...)
}

// EstimateBudgetContext is like EstimateBudget but accepts a context.
func (c *ContextClient) EstimateBudgetContext(ctx context.Context, uris ...string) (int, error) {
	total := 0
	for _, uri := range uris {
		resource, err := c.kernel.ResolveContext(ctx, uri)
		if err != nil {
			continue // Skip failed URIs
		}
		// Rough estimate: 4 chars per token
		total += len(resource.Content) / 4
	}
	return total, nil
}

// ForInference returns context formatted for immediate inference use.
// This is a convenience method that builds context and returns the string.
//
// Example:
//
//	contextStr, err := c.Context.ForInference(WithBudget(50000))
//	// Use contextStr as system prompt or context prefix
func (c *ContextClient) ForInference(opts ...ContextOption) (string, error) {
	state, err := c.Build(opts...)
	if err != nil {
		return "", err
	}
	return state.BuildContextString(), nil
}

// Config returns the default context configuration.
func (c *ContextClient) Config() *types.ContextConfig {
	return types.DefaultContextConfig()
}

// TierURIs returns the default URIs for a given tier.
// Useful for understanding what's included in each tier.
func (c *ContextClient) TierURIs(tier types.ContextTierName) []string {
	return types.ContextURISource[tier]
}

// AllTiers returns all tier names in priority order.
func (c *ContextClient) AllTiers() []types.ContextTierName {
	return []types.ContextTierName{
		types.TierIdentity,
		types.TierTemporal,
		types.TierPresent,
		types.TierFeedback,
	}
}

// IsStale returns true if context should be refreshed based on age and signals.
func (c *ContextClient) IsStale(state *types.ContextState) bool {
	return state != nil && state.ShouldRefresh
}
