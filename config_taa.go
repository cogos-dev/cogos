// config_taa.go - TAA (Temporal Attention Architecture) Configuration
//
// Loads and provides access to TAA configuration from .cog/config/taa.yaml

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"gopkg.in/yaml.v3"
)

// TAAConfig holds all TAA configuration values.
type TAAConfig struct {
	Budgets   BudgetConfig    `yaml:"budgets"`
	Temporal  TemporalConfig  `yaml:"temporal"`
	Semantic  SemanticConfig  `yaml:"semantic"`
	Substance SubstanceConfig `yaml:"substance"`
	Ranking   RankingConfig   `yaml:"ranking"`
	Coherence CoherenceConfig `yaml:"coherence"`
	Debug     DebugConfig     `yaml:"debug"`
}

// BudgetConfig controls token allocation per tier.
type BudgetConfig struct {
	TotalTokens   int `yaml:"total_tokens"`
	Tier1Identity int `yaml:"tier1_identity"`
	Tier2Temporal int `yaml:"tier2_temporal"`
	Tier3Present  int `yaml:"tier3_present"`
	Tier4Semantic int `yaml:"tier4_semantic"`
}

// TemporalConfig controls Tier 2 extraction behavior.
type TemporalConfig struct {
	ExtractionMethod    string  `yaml:"extraction_method"`
	AnchorKeywords      int     `yaml:"anchor_keywords"`
	GoalKeywords        int     `yaml:"goal_keywords"`
	RecencyWindow       int     `yaml:"recency_window"`
	ConfidenceThreshold float64 `yaml:"confidence_threshold"`
}

// SemanticConfig controls Tier 4 retrieval behavior.
type SemanticConfig struct {
	MaxCandidates     int `yaml:"max_candidates"`
	MaxResults        int `yaml:"max_results"`
	NodeTruncateChars int `yaml:"node_truncate_chars"`
	CharsPerToken     int `yaml:"chars_per_token"`
}

// SubstanceConfig controls substance-based filtering.
type SubstanceConfig struct {
	Enabled                   bool    `yaml:"enabled"`
	MinRatio                  float64 `yaml:"min_ratio"`
	PreferLeafNodes           bool    `yaml:"prefer_leaf_nodes"`
	LeafSubstanceThreshold    float64 `yaml:"leaf_substance_threshold"`
	LeafMaxRefs               int     `yaml:"leaf_max_refs"`
	RoutingSubstanceThreshold float64 `yaml:"routing_substance_threshold"`
	RoutingMinRefs            int     `yaml:"routing_min_refs"`
}

// RankingConfig controls combined scoring weights.
type RankingConfig struct {
	BM25Weight      float64 `yaml:"bm25_weight"`
	SubstanceWeight float64 `yaml:"substance_weight"`
	RecencyWeight   float64 `yaml:"recency_weight"`
}

// CoherenceConfig controls context refresh triggers.
type CoherenceConfig struct {
	MinScore    float64 `yaml:"min_score"`
	FailureMode string  `yaml:"failure_mode"`
}

// DebugConfig controls tracing and logging.
type DebugConfig struct {
	TraceTiers     bool   `yaml:"trace_tiers"`
	TraceQueries   bool   `yaml:"trace_queries"`
	TraceSubstance bool   `yaml:"trace_substance"`
	TraceFile      string `yaml:"trace_file"`
}

// Default configuration values
var defaultTAAConfig = TAAConfig{
	Budgets: BudgetConfig{
		TotalTokens:   100000,
		Tier1Identity: 33,
		Tier2Temporal: 25,
		Tier3Present:  33,
		Tier4Semantic: 6,
	},
	Temporal: TemporalConfig{
		ExtractionMethod:    "heuristic",
		AnchorKeywords:      5,
		GoalKeywords:        5,
		RecencyWindow:       10,
		ConfidenceThreshold: 0.6,
	},
	Semantic: SemanticConfig{
		MaxCandidates:     20,
		MaxResults:        10,
		NodeTruncateChars: 2000,
		CharsPerToken:     4,
	},
	Substance: SubstanceConfig{
		Enabled:                   true,
		MinRatio:                  0.5,
		PreferLeafNodes:           true,
		LeafSubstanceThreshold:    0.7,
		LeafMaxRefs:               3,
		RoutingSubstanceThreshold: 0.5,
		RoutingMinRefs:            3,
	},
	Ranking: RankingConfig{
		BM25Weight:      0.5,
		SubstanceWeight: 0.3,
		RecencyWeight:   0.2,
	},
	Coherence: CoherenceConfig{
		MinScore:    0.66,
		FailureMode: "continue",
	},
	Debug: DebugConfig{
		TraceTiers:     false,
		TraceQueries:   false,
		TraceSubstance: false,
		TraceFile:      "",
	},
}

// Cached config and mutex for thread-safe access
var (
	cachedTAAConfig *TAAConfig
	taaConfigMutex  sync.RWMutex
)

// LoadTAAConfig loads the TAA configuration from disk.
// Returns cached config if already loaded. Uses defaults if file doesn't exist.
func LoadTAAConfig(workspaceRoot string) *TAAConfig {
	taaConfigMutex.RLock()
	if cachedTAAConfig != nil {
		defer taaConfigMutex.RUnlock()
		return cachedTAAConfig
	}
	taaConfigMutex.RUnlock()

	// Upgrade to write lock
	taaConfigMutex.Lock()
	defer taaConfigMutex.Unlock()

	// Double-check after acquiring write lock
	if cachedTAAConfig != nil {
		return cachedTAAConfig
	}

	// Start with defaults
	config := defaultTAAConfig

	// Try to load from file
	configPath := filepath.Join(workspaceRoot, ".cog", "config", "taa.yaml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		// File doesn't exist, use defaults
		cachedTAAConfig = &config
		return cachedTAAConfig
	}

	// Parse YAML (overlay on defaults)
	if err := yaml.Unmarshal(data, &config); err != nil {
		fmt.Fprintf(os.Stderr, "[TAA] Warning: failed to parse config, using defaults: %v\n", err)
		config = defaultTAAConfig
	}

	cachedTAAConfig = &config
	return cachedTAAConfig
}

// ReloadTAAConfig forces a reload of the TAA configuration.
func ReloadTAAConfig(workspaceRoot string) *TAAConfig {
	taaConfigMutex.Lock()
	cachedTAAConfig = nil
	taaConfigMutex.Unlock()
	return LoadTAAConfig(workspaceRoot)
}

// GetTier4Budget calculates the token budget for Tier 4.
func (c *TAAConfig) GetTier4Budget() int {
	return (c.Budgets.TotalTokens * c.Budgets.Tier4Semantic) / 100
}

// GetTier3Budget calculates the token budget for Tier 3.
func (c *TAAConfig) GetTier3Budget() int {
	return (c.Budgets.TotalTokens * c.Budgets.Tier3Present) / 100
}

// GetTier2Budget calculates the token budget for Tier 2.
func (c *TAAConfig) GetTier2Budget() int {
	return (c.Budgets.TotalTokens * c.Budgets.Tier2Temporal) / 100
}

// GetTier1Budget calculates the token budget for Tier 1.
func (c *TAAConfig) GetTier1Budget() int {
	return (c.Budgets.TotalTokens * c.Budgets.Tier1Identity) / 100
}
