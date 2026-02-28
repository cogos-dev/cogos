// UCP Module - Universal Context Protocol
//
// This module implements the Universal Context Protocol (UCP) for explicit
// context injection via HTTP headers with JSON schema validation.
//
// See: cog://adr/043 for full specification

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// === UCP PACKET TYPES ===

// UCPIdentity represents the X-UCP-Identity header
type UCPIdentity struct {
	Version         string `json:"version"`
	Name            string `json:"name"`
	Role            string `json:"role"`
	ContextPlugin   string `json:"context_plugin,omitempty"`
	MemoryNamespace string `json:"memory_namespace,omitempty"`
	Directory       string `json:"directory,omitempty"`
	Metadata        map[string]interface{} `json:"metadata,omitempty"`
}

// UCPTAA represents the X-UCP-TAA header
type UCPTAA struct {
	Version            string                 `json:"version"`
	Profile            string                 `json:"profile"`
	TotalTokens        int                    `json:"total_tokens"`
	Tiers              UCPTAATiers            `json:"tiers"`
	ConstructedTokens  *int                   `json:"constructed_tokens,omitempty"`  // Response only
	TierBreakdown      map[string]int         `json:"tier_breakdown,omitempty"`      // Response only
}

// UCPTAATiers represents the tier configuration
type UCPTAATiers struct {
	Tier1Identity UCPTierConfig `json:"tier1_identity"`
	Tier2Temporal UCPTierConfig `json:"tier2_temporal"`
	Tier3Present  UCPTierConfig `json:"tier3_present"`
	Tier4Semantic UCPTierConfig `json:"tier4_semantic"`
}

// UCPTierConfig represents a single tier configuration
type UCPTierConfig struct {
	Enabled        bool    `json:"enabled"`
	Budget         int     `json:"budget"`
	WorkingMemory  *string `json:"working_memory,omitempty"`
	Namespace      *string `json:"namespace,omitempty"`
	MaxCandidates  *int    `json:"max_candidates,omitempty"`
	MaxResults     *int    `json:"max_results,omitempty"`
}

// UCPMemory represents the X-UCP-Memory header
type UCPMemory struct {
	Version         string                 `json:"version"`
	AnchorKeywords  []string               `json:"anchor_keywords,omitempty"`
	GoalKeywords    []string               `json:"goal_keywords,omitempty"`
	WorkingMemory   *UCPWorkingMemory      `json:"working_memory,omitempty"`
	SemanticQuery   *UCPSemanticQuery      `json:"semantic_query,omitempty"`
}

// UCPWorkingMemory represents working memory context
type UCPWorkingMemory struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// UCPSemanticQuery represents semantic memory query parameters
type UCPSemanticQuery struct {
	Namespace  *string `json:"namespace"`
	MaxResults int     `json:"max_results"`
}

// UCPHistory represents the X-UCP-History header
type UCPHistory struct {
	Version           string  `json:"version"`
	SessionID         string  `json:"session_id"`
	TurnNumber        int     `json:"turn_number"`
	ConversationHash  *string `json:"conversation_hash,omitempty"`
	ParentMessageID   *string `json:"parent_message_id,omitempty"`
}

// UCPUser represents the X-UCP-User header — identifies the human driving the request.
// Distinct from UCPIdentity which identifies the agent.
type UCPUser struct {
	Version     string `json:"version"`
	ID          string `json:"id"`                        // CogOS user ID (canonical)
	DiscordID   string `json:"discord_id,omitempty"`      // Discord snowflake ID
	DisplayName string `json:"display_name,omitempty"`    // Human-readable display name
	Source      string `json:"source,omitempty"`           // Origin platform: discord, telegram, web, etc.
}

// UCPWorkspace represents the X-UCP-Workspace header
type UCPWorkspace struct {
	Version      string            `json:"version"`
	Root         string            `json:"root"`
	Branch       string            `json:"branch"`
	Coherence    *UCPCoherence     `json:"coherence,omitempty"`
	IdentityHash *string           `json:"identity_hash,omitempty"`
}

// UCPCoherence represents workspace coherence tracking
type UCPCoherence struct {
	CanonicalHash string   `json:"canonical_hash"`
	CurrentHash   string   `json:"current_hash"`
	DriftFiles    []string `json:"drift_files,omitempty"`
}

// === UCP CONTEXT ===

// UCPContext holds all parsed and validated UCP packets
type UCPContext struct {
	Identity  *UCPIdentity  `json:"identity,omitempty"`
	TAA       *UCPTAA       `json:"taa,omitempty"`
	Memory    *UCPMemory    `json:"memory,omitempty"`
	History   *UCPHistory   `json:"history,omitempty"`
	Workspace *UCPWorkspace `json:"workspace,omitempty"`
	User      *UCPUser      `json:"user,omitempty"`
}

// === SCHEMA VALIDATION ===

var (
	ucpSchemas     = make(map[string]*jsonschema.Schema)
	ucpSchemasOnce sync.Once
)

// loadUCPSchemas loads and compiles all UCP JSON schemas
func loadUCPSchemas(workspaceRoot string) error {
	schemaDir := filepath.Join(workspaceRoot, ".cog", "schemas", "ucp")

	schemaFiles := map[string]string{
		"identity":  "ucp-identity-v1.schema.json",
		"taa":       "ucp-taa-v1.schema.json",
		"memory":    "ucp-memory-v1.schema.json",
		"history":   "ucp-history-v1.schema.json",
		"workspace": "ucp-workspace-v1.schema.json",
		"user":      "ucp-user-v1.schema.json",
	}

	compiler := jsonschema.NewCompiler()
	compiler.Draft = jsonschema.Draft7

	for packetType, filename := range schemaFiles {
		schemaPath := filepath.Join(schemaDir, filename)

		// Read schema file
		schemaData, err := os.ReadFile(schemaPath)
		if err != nil {
			return fmt.Errorf("failed to read schema %s: %w", filename, err)
		}

		// Add to compiler
		schemaURL := fmt.Sprintf("https://cogos.dev/schemas/ucp/%s/v1", packetType)
		if err := compiler.AddResource(schemaURL, strings.NewReader(string(schemaData))); err != nil {
			return fmt.Errorf("failed to add schema %s: %w", filename, err)
		}

		// Compile schema
		schema, err := compiler.Compile(schemaURL)
		if err != nil {
			return fmt.Errorf("failed to compile schema %s: %w", filename, err)
		}

		ucpSchemas[packetType] = schema
	}

	return nil
}

// ensureUCPSchemas ensures schemas are loaded (lazy initialization)
func ensureUCPSchemas(workspaceRoot string) error {
	var loadErr error
	ucpSchemasOnce.Do(func() {
		loadErr = loadUCPSchemas(workspaceRoot)
	})
	return loadErr
}

// validateUCPPacket validates a JSON packet against its schema
func validateUCPPacket(packetType string, data []byte, workspaceRoot string) error {
	if err := ensureUCPSchemas(workspaceRoot); err != nil {
		return fmt.Errorf("failed to load schemas: %w", err)
	}

	schema, ok := ucpSchemas[packetType]
	if !ok {
		return fmt.Errorf("unknown packet type: %s", packetType)
	}

	var v interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}

	if err := schema.Validate(v); err != nil {
		return fmt.Errorf("schema validation failed: %w", err)
	}

	return nil
}

// === HEADER PARSING ===

// parseUCPHeaders extracts and validates UCP packets from HTTP headers
func parseUCPHeaders(r *http.Request, workspaceRoot string) (*UCPContext, error) {
	ctx := &UCPContext{}

	// Parse X-UCP-Identity
	if identityJSON := r.Header.Get("X-UCP-Identity"); identityJSON != "" {
		if err := validateUCPPacket("identity", []byte(identityJSON), workspaceRoot); err != nil {
			return nil, fmt.Errorf("invalid X-UCP-Identity: %w", err)
		}
		var identity UCPIdentity
		if err := json.Unmarshal([]byte(identityJSON), &identity); err != nil {
			return nil, fmt.Errorf("failed to parse X-UCP-Identity: %w", err)
		}
		ctx.Identity = &identity
	}

	// Parse X-UCP-TAA
	if taaJSON := r.Header.Get("X-UCP-TAA"); taaJSON != "" {
		if err := validateUCPPacket("taa", []byte(taaJSON), workspaceRoot); err != nil {
			return nil, fmt.Errorf("invalid X-UCP-TAA: %w", err)
		}
		var taa UCPTAA
		if err := json.Unmarshal([]byte(taaJSON), &taa); err != nil {
			return nil, fmt.Errorf("failed to parse X-UCP-TAA: %w", err)
		}
		ctx.TAA = &taa
	}

	// Parse X-UCP-Memory
	if memoryJSON := r.Header.Get("X-UCP-Memory"); memoryJSON != "" {
		if err := validateUCPPacket("memory", []byte(memoryJSON), workspaceRoot); err != nil {
			return nil, fmt.Errorf("invalid X-UCP-Memory: %w", err)
		}
		var memory UCPMemory
		if err := json.Unmarshal([]byte(memoryJSON), &memory); err != nil {
			return nil, fmt.Errorf("failed to parse X-UCP-Memory: %w", err)
		}
		ctx.Memory = &memory
	}

	// Parse X-UCP-History
	if historyJSON := r.Header.Get("X-UCP-History"); historyJSON != "" {
		if err := validateUCPPacket("history", []byte(historyJSON), workspaceRoot); err != nil {
			return nil, fmt.Errorf("invalid X-UCP-History: %w", err)
		}
		var history UCPHistory
		if err := json.Unmarshal([]byte(historyJSON), &history); err != nil {
			return nil, fmt.Errorf("failed to parse X-UCP-History: %w", err)
		}
		ctx.History = &history
	}

	// Parse X-UCP-Workspace
	if workspaceJSON := r.Header.Get("X-UCP-Workspace"); workspaceJSON != "" {
		if err := validateUCPPacket("workspace", []byte(workspaceJSON), workspaceRoot); err != nil {
			return nil, fmt.Errorf("invalid X-UCP-Workspace: %w", err)
		}
		var workspace UCPWorkspace
		if err := json.Unmarshal([]byte(workspaceJSON), &workspace); err != nil {
			return nil, fmt.Errorf("failed to parse X-UCP-Workspace: %w", err)
		}
		ctx.Workspace = &workspace
	}

	// Parse X-UCP-User (human identity, distinct from agent identity)
	if userJSON := r.Header.Get("X-UCP-User"); userJSON != "" {
		if err := validateUCPPacket("user", []byte(userJSON), workspaceRoot); err != nil {
			return nil, fmt.Errorf("invalid X-UCP-User: %w", err)
		}
		var user UCPUser
		if err := json.Unmarshal([]byte(userJSON), &user); err != nil {
			return nil, fmt.Errorf("failed to parse X-UCP-User: %w", err)
		}
		ctx.User = &user
	}

	return ctx, nil
}

// === RESPONSE HEADERS ===

// setUCPResponseHeaders sets UCP response headers with metrics
func setUCPResponseHeaders(w http.ResponseWriter, ctx *UCPContext) error {
	// Return TAA metrics if TAA was used
	if ctx.TAA != nil && ctx.TAA.ConstructedTokens != nil {
		taaJSON, err := json.Marshal(ctx.TAA)
		if err != nil {
			return fmt.Errorf("failed to marshal TAA response: %w", err)
		}
		w.Header().Set("X-UCP-TAA", string(taaJSON))
	}

	// Return workspace coherence if available
	if ctx.Workspace != nil {
		workspaceJSON, err := json.Marshal(ctx.Workspace)
		if err != nil {
			return fmt.Errorf("failed to marshal workspace response: %w", err)
		}
		w.Header().Set("X-UCP-Workspace", string(workspaceJSON))
	}

	return nil
}
