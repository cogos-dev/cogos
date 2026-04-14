// serve_foveated.go — POST /v1/context/foveated
//
// Bridge endpoint matching the v2 foveated context API so that the Claude Code
// hook (foveated-context.py) can point at the v3 kernel.
//
// Input:  {prompt, iris: {size, used}, profile, session_id}
// Output: {context, tokens, anchor, goal, iris_pressure}
//
// The "context" field is a rendered string of CogBlock HTML comment blocks that
// get injected into Claude's context window via the hook's additionalContext.
package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// foveatedRequest is the wire format for POST /v1/context/foveated.
type foveatedRequest struct {
	Prompt    string     `json:"prompt"`
	Query     string     `json:"query"` // alias for prompt — accepted for API compatibility
	Iris      irisSignal `json:"iris"`
	Profile   string     `json:"profile"`
	SessionID string     `json:"session_id"`
}

type irisSignal struct {
	Size int `json:"size"` // total context window tokens
	Used int `json:"used"` // estimated tokens consumed
}

// foveatedResponse is the wire format returned to the hook.
type foveatedResponse struct {
	Context         string         `json:"context"`
	Tokens          int            `json:"tokens"`
	Anchor          string         `json:"anchor"`
	Goal            string         `json:"goal"`
	IrisPressure    float64        `json:"iris_pressure"`
	CoherenceScore  float64        `json:"coherence_score"`
	TierBreakdown   map[string]int `json:"tier_breakdown"`
	EffectiveBudget int            `json:"effective_budget"`
	Blocks          []foveatedBlock `json:"blocks"`
}

type foveatedBlock struct {
	Tier      string                `json:"tier"`
	Name      string                `json:"name"`
	Hash      string                `json:"hash"`
	Tokens    int                   `json:"tokens"`
	Stability int                   `json:"stability"`
	Preview   string                `json:"preview,omitempty"`
	Sources   []foveatedBlockSource `json:"sources,omitempty"`
}

type foveatedBlockSource struct {
	URI      string  `json:"uri"`
	Title    string  `json:"title,omitempty"`
	Path     string  `json:"path,omitempty"`
	Salience float64 `json:"salience,omitempty"`
	Reason   string  `json:"reason,omitempty"`
	Summary  string  `json:"summary,omitempty"`
}

// handleFoveatedContext assembles context blocks for Claude Code injection.
//
//	POST /v1/context/foveated
func (s *Server) handleFoveatedContext(w http.ResponseWriter, r *http.Request) {
	var req foveatedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "parse body: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Accept "query" as an alias for "prompt" for API compatibility.
	if req.Prompt == "" && req.Query != "" {
		req.Prompt = req.Query
	}

	// Extract anchor (topic) and goal from prompt.
	anchor := extractAnchor(req.Prompt)
	goal := extractGoal(req.Prompt)
	keywords := extractKeywords(req.Prompt)

	// Compute iris pressure.
	pressure := 0.0
	if req.Iris.Size > 0 {
		pressure = float64(req.Iris.Used) / float64(req.Iris.Size)
	}

	// === Tier 4: Knowledge (Constellation) ===
	// Try TRM scoring first (best results), fall back to keyword+salience.
	var knowledgeDocs []FovealDoc
	usedTRM := false

	hasTRM := s.process.TRM() != nil
	hasIdx := s.process.EmbeddingIndex() != nil
	hasPrompt := req.Prompt != ""
	slog.Info("foveated: TRM gate check",
		"has_trm", hasTRM,
		"has_idx", hasIdx,
		"has_prompt", hasPrompt,
		"prompt_len", len(req.Prompt),
		"prompt_prefix", truncate(req.Prompt, 80),
	)

	if hasTRM && hasIdx && hasPrompt {
		// Use a 5s timeout for TRM scoring — the hook has 10s total,
		// and we need time for rendering + response writing.
		trmCtx, trmCancel := context.WithTimeout(r.Context(), 5*time.Second)
		trmResults := trmScoreDocs(trmCtx, s.process, req.Prompt, req.SessionID, 100)
		trmCancel()
		slog.Info("foveated: TRM scoring complete", "results", len(trmResults))
		if len(trmResults) > 0 {
			usedTRM = true
			// Deduplicate by path: keep only the highest-scoring chunk per file.
			// TRM results are pre-sorted by score descending, so the first
			// occurrence of each path is the best chunk from that file.
			seenPaths := make(map[string]bool)
			for _, tr := range trmResults {
				if seenPaths[tr.IndexResult.ChunkMeta.Path] {
					continue
				}
				seenPaths[tr.IndexResult.ChunkMeta.Path] = true
				knowledgeDocs = append(knowledgeDocs, FovealDoc{
					URI:      "cog://chunks/" + tr.IndexResult.ChunkMeta.ChunkID,
					Path:     tr.IndexResult.ChunkMeta.Path,
					Title:    tr.IndexResult.ChunkMeta.Title,
					Salience: float64(tr.TRMScore),
					Reason:   "trm",
				})
			}
			slog.Info("foveated: TRM docs built",
				"unique_paths", len(knowledgeDocs),
				"total_chunks", len(trmResults),
			)
		}
	}

	// Fall back to keyword+salience when TRM unavailable.
	if !usedTRM {
		cogIdx := s.process.Index()
		if cogIdx != nil && len(cogIdx.ByURI) > 0 {
			for _, doc := range cogIdx.ByURI {
				switch strings.ToLower(doc.Status) {
				case "superseded", "deprecated", "retired":
					continue
				}
				if strings.Contains(filepath.ToSlash(doc.Path), "/archive/") {
					continue
				}

				relevance := queryRelevance(doc, keywords)
				salience := s.process.Field().Score(doc.Path)
				if relevance <= 0 && salience <= 0 {
					continue
				}

				reason := "high-salience"
				switch {
				case relevance > 0 && salience > 0:
					reason = "both"
				case relevance > 0:
					reason = "query-match"
				}

				knowledgeDocs = append(knowledgeDocs, FovealDoc{
					URI:      doc.URI,
					Path:     doc.Path,
					Title:    doc.Title,
					Salience: relevance*2.0 + salience,
					Reason:   reason,
				})
			}
		}

		// Sort by salience descending.
		sort.Slice(knowledgeDocs, func(i, j int) bool {
			return knowledgeDocs[i].Salience > knowledgeDocs[j].Salience
		})
	}

	maxDocs := 10
	if len(knowledgeDocs) > maxDocs {
		knowledgeDocs = knowledgeDocs[:maxDocs]
	}

	// Build a manifest for selected docs (budget: ~4000 tokens for tier4).
	tier4Budget := 4000
	renderedDocs, _ := evictForBudgetMode(knowledgeDocs, nil, tier4Budget, s.cfg.WorkspaceRoot, true)

	// === Build ContextFrame with all block builders ===
	frame := &ContextFrame{
		Anchor: anchor,
		Goal:   goal,
	}

	// Tier 0, stability 90: Project block (CLAUDE.md)
	if blk := buildProjectBlock(s.cfg.WorkspaceRoot); blk != nil {
		frame.Blocks = append(frame.Blocks, *blk)
	}

	// Tier 2, stability 70: Node health (sibling services)
	if blk := buildNodeBlock(s.process); blk != nil {
		frame.Blocks = append(frame.Blocks, *blk)
	}

	// Tier 2, stability 40: Attentional field top-10
	if blk := buildFieldBlock(s.process, s.cfg.WorkspaceRoot); blk != nil {
		frame.Blocks = append(frame.Blocks, *blk)
	}

	// Tier 1, stability 30: Knowledge (foveated CogDocs — existing logic)
	knowledgeContent := renderKnowledgeContent(renderedDocs)
	knowledgeBlock := NewBlock(BlockKnowledge, knowledgeContent)
	frame.Blocks = append(frame.Blocks, knowledgeBlock)

	// Tier 2, stability 20: Recent ledger events
	if blk := buildEventsBlock(s.cfg.WorkspaceRoot); blk != nil {
		frame.Blocks = append(frame.Blocks, *blk)
	}

	// Compute effective budget from iris signal.
	effectiveBudget := tier4Budget
	if req.Iris.Size > 0 && req.Iris.Used > 0 {
		available := req.Iris.Size - req.Iris.Used
		if available > 0 && available < effectiveBudget {
			effectiveBudget = available
		}
	}

	// Fit blocks within budget and render.
	frame.FitBudget(effectiveBudget)
	contextStr := frame.Render()
	tokens := estTokens(contextStr)

	slog.Info("foveated: assembled",
		"anchor", anchor,
		"goal", goal,
		"docs", len(renderedDocs),
		"frame_blocks", len(frame.Blocks),
		"tokens", tokens,
		"pressure", fmt.Sprintf("%.1f%%", pressure*100),
	)

	// Build response blocks from the frame for backward compatibility.
	blocks := make([]foveatedBlock, 0, len(frame.Blocks))
	tierBreakdown := make(map[string]int)
	for _, b := range frame.Blocks {
		tierKey := fmt.Sprintf("tier%d", b.Tier)
		tierBreakdown[tierKey] += b.Tokens

		var sources []foveatedBlockSource
		if b.Name == BlockKnowledge {
			for _, doc := range renderedDocs {
				sources = append(sources, foveatedBlockSource{
					URI:      doc.URI,
					Title:    doc.Title,
					Path:     doc.Path,
					Salience: doc.Salience,
					Reason:   doc.Reason,
					Summary:  truncate(strings.TrimSpace(doc.Summary), 180),
				})
			}
		}

		blocks = append(blocks, foveatedBlock{
			Tier:      tierKey,
			Name:      b.Name,
			Hash:      b.Hash,
			Tokens:    b.Tokens,
			Stability: b.Stability,
			Preview:   truncate(b.Content, 280),
			Sources:   sources,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(foveatedResponse{
		Context:         contextStr,
		Tokens:          tokens,
		Anchor:          anchor,
		Goal:            goal,
		IrisPressure:    pressure,
		CoherenceScore:  1.0, // TODO: compute from tier quality scores
		TierBreakdown:   tierBreakdown,
		EffectiveBudget: effectiveBudget,
		Blocks:          blocks,
	})
}


// renderKnowledgeContent builds the knowledge block content string from foveated docs.
// This is the content-only portion; the block envelope is handled by ContextFrame.Render.
func renderKnowledgeContent(docs []FovealDoc) string {
	var content strings.Builder

	if docsUseManifest(docs) {
		content.WriteString(renderWorkspaceManifest(docs))
		return content.String()
	}

	content.WriteString("# Relevant Knowledge (Constellation)\n\n")
	content.WriteString("The following documents from the workspace knowledge graph are relevant to your query:\n\n")

	if len(docs) > 0 {
		for _, doc := range docs {
			title := doc.Title
			if title == "" {
				title = filepath.Base(doc.Path)
			}
			fmt.Fprintf(&content, "### %s\n", title)
			fmt.Fprintf(&content, "_Source: %s | Salience: %.2f | Reason: %s_\n\n", doc.URI, doc.Salience, doc.Reason)
			if doc.Content != "" {
				content.WriteString(doc.Content)
				content.WriteString("\n\n")
			}
		}
	}

	return content.String()
}

// extractAnchor derives the current topic from the user's prompt.
// Returns the most salient keyword or short phrase.
func extractAnchor(prompt string) string {
	keywords := extractKeywords(prompt)
	if len(keywords) == 0 {
		return "(none)"
	}
	// Take up to first 3 keywords as the anchor.
	n := min(3, len(keywords))
	return strings.Join(keywords[:n], " ")
}

// extractGoal derives the user's intent from their prompt.
// Uses simple heuristics to classify as question, action, or exploration.
func extractGoal(prompt string) string {
	lower := strings.ToLower(strings.TrimSpace(prompt))

	actionVerbs := []string{"build", "create", "implement", "write", "add", "fix", "update", "refactor", "delete", "remove", "move", "rename"}
	for _, verb := range actionVerbs {
		if strings.Contains(lower, verb) {
			return "action: " + verb + " " + truncateGoal(prompt)
		}
	}

	questionVerbs := []string{"explain", "describe", "analyze", "review", "document"}
	for _, verb := range questionVerbs {
		if strings.Contains(lower, verb) {
			return "understand: " + truncateGoal(prompt)
		}
	}

	opsVerbs := []string{"deploy", "configure", "install", "setup", "set up", "migrate", "optimize", "test", "debug"}
	for _, verb := range opsVerbs {
		if strings.Contains(lower, verb) {
			return "operate: " + verb + " " + truncateGoal(prompt)
		}
	}

	// Imperative patterns (let's, lets).
	if strings.HasPrefix(lower, "let") {
		return "action: let " + truncateGoal(prompt)
	}

	// Question-mark fallback.
	if strings.Contains(lower, "?") {
		return "understand: " + truncateGoal(prompt)
	}

	// Default: exploration.
	return "(exploring/discussing)"
}

// truncateGoal caps a goal string at ~80 chars.
func truncateGoal(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		// Try to break at a word boundary.
		if idx := strings.LastIndex(s[:80], " "); idx > 40 {
			return s[:idx]
		}
		return s[:80]
	}
	return s
}
