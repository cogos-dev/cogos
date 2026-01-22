// Context Constructor - TAA Core Orchestrator
//
// This module implements the Temporal Attention Architecture (TAA) context
// construction pipeline. It orchestrates the three tiers:
//
//   Tier 1: Identity Context (~33k tokens)
//   Tier 2: Temporal Context (~25k tokens)
//   Tier 3: Present Context (~33k tokens)
//
// The context constructor calls each tier in sequence, passing anchor and goal
// from Tier 2 to Tier 3 for coherence checking.

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// TAAProfile represents a named TAA configuration profile.
// Profiles live in .cog/config/taa/profiles/{name}.yaml
type TAAProfile struct {
	Name        string               `yaml:"name"`
	Description string               `yaml:"description"`
	Identity    *TAAProfileIdentity  `yaml:"identity"`
	Tiers       TAAProfileTiers      `yaml:"tiers"`
	Coherence   *TAAProfileCoherence `yaml:"coherence"`
}

type TAAProfileIdentity struct {
	Name         *string `yaml:"name"`         // nil = no identity
	Directory    string  `yaml:"directory"`
	InjectPlugin bool    `yaml:"inject_plugin"`
}

type TAAProfileTiers struct {
	TotalTokens  int                  `yaml:"total_tokens"`
	Tier1        *TAAProfileTierSpec  `yaml:"tier1_identity"`
	Tier2        *TAAProfileTierSpec  `yaml:"tier2_temporal"`
	Tier3        *TAAProfileTierSpec  `yaml:"tier3_present"`
	Tier4        *TAAProfileTierSpec  `yaml:"tier4_semantic"`
}

type TAAProfileTierSpec struct {
	Enabled       bool    `yaml:"enabled"`
	Budget        int     `yaml:"budget"`        // percentage of total
	WorkingMemory string  `yaml:"working_memory,omitempty"`
	Namespace     *string `yaml:"namespace,omitempty"`
	MaxCandidates int     `yaml:"max_candidates,omitempty"`
	MaxResults    int     `yaml:"max_results,omitempty"`
}

type TAAProfileCoherence struct {
	MinScore    float64 `yaml:"min_score"`
	FailureMode string  `yaml:"failure_mode"`
}

// LoadTAAProfile loads a TAA profile by name from the profiles directory
func LoadTAAProfile(workspaceRoot, profileName string) (*TAAProfile, error) {
	profilePath := filepath.Join(workspaceRoot, ".cog", "config", "taa", "profiles", profileName+".yaml")

	data, err := os.ReadFile(profilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read TAA profile %s: %w", profileName, err)
	}

	var profile TAAProfile
	if err := yaml.Unmarshal(data, &profile); err != nil {
		return nil, fmt.Errorf("failed to parse TAA profile %s: %w", profileName, err)
	}

	// Set defaults
	if profile.Tiers.TotalTokens == 0 {
		profile.Tiers.TotalTokens = TotalContextTokens
	}

	return &profile, nil
}

// Total context budget constants
const (
	// Total token budget for context construction
	TotalContextTokens = 100000

	// Tier allocations (percentages)
	Tier1Allocation = 0.33 // 33% for identity
	Tier2Allocation = 0.25 // 25% for temporal
	Tier3Allocation = 0.33 // 33% for present
	Tier4Allocation = 0.06 // 6% for semantic memory (constellation)
	// Remaining 3% reserved for overhead
)

// ConstructContextState orchestrates the TAA context construction pipeline.
//
// This function calls all four tiers in sequence:
// 1. Tier 1 (Identity): Load identity context from configuration
// 2. Tier 2 (Temporal): Analyze conversation for anchor and goal
// 3. Tier 3 (Present): Format recent conversation with coherence check
// 4. Tier 4 (Semantic): Query constellation knowledge graph
//
// Parameters:
//   - messages: Full conversation history (ChatMessage slice)
//   - sessionID: Current session identifier
//   - workspaceRoot: Absolute path to workspace root
//
// Returns:
//   - *ContextState: Fully populated context state
//   - error: Any error (nil if successful, errors logged but don't block)
func ConstructContextState(messages []ChatMessage, sessionID string, workspaceRoot string) (*ContextState, error) {
	// Calculate token budgets for each tier
	tier1Budget := int(float64(TotalContextTokens) * Tier1Allocation)
	tier2Budget := int(float64(TotalContextTokens) * Tier2Allocation)
	tier3Budget := int(float64(TotalContextTokens) * Tier3Allocation)
	tier4Budget := int(float64(TotalContextTokens) * Tier4Allocation)

	// Initialize context state
	state := &ContextState{}

	var errors []string
	totalTokens := 0

	// === Tier 1: Identity Context ===
	tier1Content, err := LoadIdentityContext(workspaceRoot, tier1Budget)
	if err != nil {
		log.Printf("[TAA] Tier 1 (Identity) error: %v", err)
		errors = append(errors, fmt.Sprintf("tier1: %v", err))
		// Continue with empty identity - don't fail the whole pipeline
		tier1Content = ""
	}

	if tier1Content != "" {
		tier1Tokens := len(tier1Content) / CharsPerToken
		state.Tier1Identity = &ContextTier{
			Content: tier1Content,
			Tokens:  tier1Tokens,
			Source:  "identity",
		}
		totalTokens += tier1Tokens
	}

	// === Tier 2: Temporal Context ===
	var anchor, goal string
	tier2Content, anchor, goal, err := RetrieveTemporalContext(messages, sessionID, workspaceRoot, tier2Budget)
	if err != nil {
		log.Printf("[TAA] Tier 2 (Temporal) error: %v", err)
		errors = append(errors, fmt.Sprintf("tier2: %v", err))
		// Continue with empty temporal context
		tier2Content = ""
	}

	if tier2Content != "" {
		tier2Tokens := len(tier2Content) / CharsPerToken
		state.Tier2Temporal = &ContextTier{
			Content: tier2Content,
			Tokens:  tier2Tokens,
			Source:  "temporal",
		}
		totalTokens += tier2Tokens
	}

	// === Tier 3: Present Context ===
	// Pass anchor and goal from Tier 2 for coherence checking
	tier3Content, err := FormatPresentContext(messages, anchor, goal, tier3Budget)
	if err != nil {
		log.Printf("[TAA] Tier 3 (Present) error: %v", err)
		errors = append(errors, fmt.Sprintf("tier3: %v", err))
		// Continue with empty present context
		tier3Content = ""
	}

	if tier3Content != "" {
		tier3Tokens := len(tier3Content) / CharsPerToken
		state.Tier3Present = &ContextTier{
			Content: tier3Content,
			Tokens:  tier3Tokens,
			Source:  "present",
		}
		totalTokens += tier3Tokens
	}

	// === Tier 4: Semantic Memory (Constellation) ===
	// Query constellation based on anchor and goal from Tier 2
	tier4Content, err := QueryConstellation(workspaceRoot, anchor, goal, tier4Budget)
	if err != nil {
		log.Printf("[TAA] Tier 4 (Semantic) error: %v", err)
		errors = append(errors, fmt.Sprintf("tier4: %v", err))
		// Continue with empty semantic context
		tier4Content = ""
	}

	if tier4Content != "" {
		tier4Tokens := len(tier4Content) / CharsPerToken
		state.Tier4Semantic = &ContextTier{
			Content: tier4Content,
			Tokens:  tier4Tokens,
			Source:  "constellation",
		}
		totalTokens += tier4Tokens
	}

	// Set total tokens
	state.TotalTokens = totalTokens

	// Calculate coherence score based on successful tier loads
	successfulTiers := 0
	if state.Tier1Identity != nil {
		successfulTiers++
	}
	if state.Tier2Temporal != nil {
		successfulTiers++
	}
	if state.Tier3Present != nil {
		successfulTiers++
	}
	if state.Tier4Semantic != nil {
		successfulTiers++
	}
	state.CoherenceScore = float64(successfulTiers) / 4.0

	// Store TAA signals for visibility/debugging
	state.Anchor = anchor
	state.Goal = goal

	// Mark for refresh if coherence is low
	state.ShouldRefresh = state.CoherenceScore < 0.66

	// Return state even if there were errors (partial success is OK)
	if len(errors) > 0 {
		return state, fmt.Errorf("partial construction errors: %s", strings.Join(errors, "; "))
	}

	return state, nil
}

// Note: BuildContextString method is defined in inference.go
// It assembles the full context string from all tiers with separators.
// Usage: contextState.BuildContextString()

// ConstructContextStateMinimal creates a minimal context state for simple requests.
// This bypasses the full TAA pipeline when only present context is needed.
//
// Use cases:
// - Quick inference requests without identity/temporal context
// - Testing and debugging
// - Fallback when full pipeline fails
func ConstructContextStateMinimal(messages []ChatMessage) *ContextState {
	// Just format present context with defaults
	tier3Content, err := FormatPresentContext(messages, "", "", 0)
	if err != nil {
		return &ContextState{}
	}

	return &ContextState{
		Tier3Present: &ContextTier{
			Content: tier3Content,
			Tokens:  len(tier3Content) / CharsPerToken,
			Source:  "present",
		},
		TotalTokens:    len(tier3Content) / CharsPerToken,
		CoherenceScore: 0.33, // Partial coherence (only 1/3 tiers)
		ShouldRefresh:  true, // Mark for refresh since incomplete
	}
}

// ConstructContextStateWithProfile builds TAA context using a named profile.
//
// This function loads the profile configuration and constructs context
// according to its settings. Profiles can enable/disable individual tiers
// and configure tier-specific settings.
//
// Parameters:
//   - messages: Full conversation history
//   - sessionID: Current session identifier
//   - workspaceRoot: Absolute path to workspace root
//   - profileName: Name of the TAA profile to use (e.g., "default", "minimal")
//
// Returns:
//   - *ContextState: Fully populated context state per profile settings
//   - error: Any error during construction
func ConstructContextStateWithProfile(messages []ChatMessage, sessionID, workspaceRoot, profileName string) (*ContextState, error) {
	// Load the profile
	profile, err := LoadTAAProfile(workspaceRoot, profileName)
	if err != nil {
		log.Printf("[TAA] Failed to load profile %s, falling back to default: %v", profileName, err)
		// Fall back to default behavior
		return ConstructContextState(messages, sessionID, workspaceRoot)
	}

	// Calculate token budgets based on profile
	totalTokens := profile.Tiers.TotalTokens
	if totalTokens == 0 {
		totalTokens = TotalContextTokens
	}

	// Initialize context state
	state := &ContextState{}
	var errors []string
	usedTokens := 0
	enabledTiers := 0
	successfulTiers := 0

	// === Tier 1: Identity Context ===
	if profile.Tiers.Tier1 != nil && profile.Tiers.Tier1.Enabled {
		enabledTiers++
		tier1Budget := (totalTokens * profile.Tiers.Tier1.Budget) / 100

		// Use profile-specific identity settings
		var tier1Content string
		if profile.Identity != nil && profile.Identity.Name != nil {
			tier1Content, err = LoadIdentityContextWithConfig(
				workspaceRoot,
				*profile.Identity.Name,
				profile.Identity.Directory,
				profile.Identity.InjectPlugin,
				tier1Budget,
			)
		} else {
			tier1Content, err = LoadIdentityContext(workspaceRoot, tier1Budget)
		}

		if err != nil {
			log.Printf("[TAA] Tier 1 (Identity) error: %v", err)
			errors = append(errors, fmt.Sprintf("tier1: %v", err))
		} else if tier1Content != "" {
			tier1Tokens := len(tier1Content) / CharsPerToken
			state.Tier1Identity = &ContextTier{
				Content: tier1Content,
				Tokens:  tier1Tokens,
				Source:  "identity:" + profileName,
			}
			usedTokens += tier1Tokens
			successfulTiers++
		}
	}

	// === Tier 2: Temporal Context ===
	var anchor, goal string
	if profile.Tiers.Tier2 != nil && profile.Tiers.Tier2.Enabled {
		enabledTiers++
		tier2Budget := (totalTokens * profile.Tiers.Tier2.Budget) / 100

		var tier2Content string
		tier2Content, anchor, goal, err = RetrieveTemporalContext(messages, sessionID, workspaceRoot, tier2Budget)
		if err != nil {
			log.Printf("[TAA] Tier 2 (Temporal) error: %v", err)
			errors = append(errors, fmt.Sprintf("tier2: %v", err))
		} else if tier2Content != "" {
			tier2Tokens := len(tier2Content) / CharsPerToken
			state.Tier2Temporal = &ContextTier{
				Content: tier2Content,
				Tokens:  tier2Tokens,
				Source:  "temporal:" + profileName,
			}
			usedTokens += tier2Tokens
			successfulTiers++
		}
	}

	// === Tier 3: Present Context ===
	if profile.Tiers.Tier3 != nil && profile.Tiers.Tier3.Enabled {
		enabledTiers++
		tier3Budget := (totalTokens * profile.Tiers.Tier3.Budget) / 100

		tier3Content, err := FormatPresentContext(messages, anchor, goal, tier3Budget)
		if err != nil {
			log.Printf("[TAA] Tier 3 (Present) error: %v", err)
			errors = append(errors, fmt.Sprintf("tier3: %v", err))
		} else if tier3Content != "" {
			tier3Tokens := len(tier3Content) / CharsPerToken
			state.Tier3Present = &ContextTier{
				Content: tier3Content,
				Tokens:  tier3Tokens,
				Source:  "present:" + profileName,
			}
			usedTokens += tier3Tokens
			successfulTiers++
		}
	}

	// === Tier 4: Semantic Memory ===
	if profile.Tiers.Tier4 != nil && profile.Tiers.Tier4.Enabled {
		enabledTiers++
		tier4Budget := (totalTokens * profile.Tiers.Tier4.Budget) / 100

		tier4Content, err := QueryConstellation(workspaceRoot, anchor, goal, tier4Budget)
		if err != nil {
			log.Printf("[TAA] Tier 4 (Semantic) error: %v", err)
			errors = append(errors, fmt.Sprintf("tier4: %v", err))
		} else if tier4Content != "" {
			tier4Tokens := len(tier4Content) / CharsPerToken
			state.Tier4Semantic = &ContextTier{
				Content: tier4Content,
				Tokens:  tier4Tokens,
				Source:  "constellation:" + profileName,
			}
			usedTokens += tier4Tokens
			successfulTiers++
		}
	}

	// Set totals and coherence
	state.TotalTokens = usedTokens
	state.Anchor = anchor
	state.Goal = goal

	// Calculate coherence based on enabled tiers (not fixed 4)
	if enabledTiers > 0 {
		state.CoherenceScore = float64(successfulTiers) / float64(enabledTiers)
	} else {
		state.CoherenceScore = 1.0 // No tiers enabled = trivially coherent
	}

	// Check coherence threshold from profile
	minScore := 0.66
	if profile.Coherence != nil && profile.Coherence.MinScore > 0 {
		minScore = profile.Coherence.MinScore
	}
	state.ShouldRefresh = state.CoherenceScore < minScore

	if len(errors) > 0 {
		return state, fmt.Errorf("partial construction errors (profile=%s): %s", profileName, strings.Join(errors, "; "))
	}

	return state, nil
}

// LoadIdentityContextWithConfig loads identity context with explicit configuration.
// This allows profiles to specify custom identity settings.
func LoadIdentityContextWithConfig(workspaceRoot, identityName, identityDir string, injectPlugin bool, tokenBudget int) (string, error) {
	// Build identity path
	if identityDir == "" {
		identityDir = ".cog/bin/agents/identities"
	}
	identityPath := filepath.Join(workspaceRoot, identityDir, "identity_"+identityName+".md")

	// Read identity card
	content, err := os.ReadFile(identityPath)
	if err != nil {
		return "", fmt.Errorf("failed to read identity %s: %w", identityName, err)
	}

	result := string(content)

	// TODO: Handle plugin injection if injectPlugin is true
	// For now, just return the identity card content

	// Truncate to budget if needed
	maxChars := tokenBudget * CharsPerToken
	if len(result) > maxChars {
		result = result[:maxChars]
	}

	return result, nil
}

// EstimateContextTokens provides a quick estimate of context token usage
// without fully constructing the context.
//
// Useful for:
// - Checking if context might overflow before construction
// - Monitoring context growth during conversation
// - Debugging token allocation issues
func EstimateContextTokens(messages []ChatMessage, workspaceRoot string) int {
	// Estimate Tier 1: identity card + plugin output
	// Typical identity: ~2000 chars = ~500 tokens
	tier1Estimate := 500

	// Estimate Tier 2: temporal context
	// Depends on working memory size, typically ~1000-4000 chars = ~250-1000 tokens
	tier2Estimate := 500

	// Estimate Tier 3: present context from messages
	tier3Estimate := EstimatePresentContextTokens(messages)

	return tier1Estimate + tier2Estimate + tier3Estimate
}

// ContextSummary provides a human-readable summary of the context state.
// Useful for debugging and logging.
func (cs *ContextState) ContextSummary() string {
	if cs == nil {
		return "ContextState: nil"
	}

	var sb strings.Builder
	sb.WriteString("ContextState Summary:\n")

	// Tier 1
	if cs.Tier1Identity != nil {
		sb.WriteString(fmt.Sprintf("  Tier 1 (Identity): %d tokens from %s\n",
			cs.Tier1Identity.Tokens, cs.Tier1Identity.Source))
	} else {
		sb.WriteString("  Tier 1 (Identity): not loaded\n")
	}

	// Tier 2
	if cs.Tier2Temporal != nil {
		sb.WriteString(fmt.Sprintf("  Tier 2 (Temporal): %d tokens from %s\n",
			cs.Tier2Temporal.Tokens, cs.Tier2Temporal.Source))
	} else {
		sb.WriteString("  Tier 2 (Temporal): not loaded\n")
	}

	// Tier 3
	if cs.Tier3Present != nil {
		sb.WriteString(fmt.Sprintf("  Tier 3 (Present): %d tokens from %s\n",
			cs.Tier3Present.Tokens, cs.Tier3Present.Source))
	} else {
		sb.WriteString("  Tier 3 (Present): not loaded\n")
	}

	// Tier 4
	if cs.Tier4Semantic != nil {
		sb.WriteString(fmt.Sprintf("  Tier 4 (Semantic): %d tokens from %s\n",
			cs.Tier4Semantic.Tokens, cs.Tier4Semantic.Source))
	} else {
		sb.WriteString("  Tier 4 (Semantic): not loaded\n")
	}

	// Totals
	sb.WriteString(fmt.Sprintf("  Total: %d tokens\n", cs.TotalTokens))
	sb.WriteString(fmt.Sprintf("  Coherence: %.2f\n", cs.CoherenceScore))
	if cs.ShouldRefresh {
		sb.WriteString("  Status: needs refresh\n")
	} else {
		sb.WriteString("  Status: healthy\n")
	}

	return sb.String()
}
