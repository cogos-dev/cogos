// context_assembly.go — foveated context assembly for chat requests
//
// The engine owns the full context window. It accepts the client's messages[],
// decomposes them, scores conversation turns alongside CogDocs, and renders
// everything into a stability-ordered token stream within the configured budget.
//
// Stability zones (ordered for KV cache optimization):
//
//	Zone 0: Nucleus (identity card) — most stable, always present
//	Zone 1: CogDocs + client system prompt — shifts slowly per query
//	Zone 2: Conversation history — scored by recency + relevance, evictable
//	Zone 3: Current message — always present
//	[Reserve: OutputReserve tokens for model generation]
//
// Token budget is approximated as chars/4.
// Default budget: 32768 tokens (matches provider context_window from providers.yaml).
//
// Any OpenAI-compatible client works transparently — the engine intercepts the
// standard messages[] array and manages what the model actually sees.
package engine

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// ContextPackage is the assembled context for a single chat request.
type ContextPackage struct {
	// NucleusText is the identity card content — always present (Zone 0).
	NucleusText string

	// ClientSystem is the client's system prompt if provided (Zone 1).
	ClientSystem string

	// FovealDocs are the CogDocs selected for injection (Zone 1).
	FovealDocs []FovealDoc

	// Conversation is the scored/filtered conversation history (Zone 2).
	Conversation []ScoredMessage

	// CurrentMessage is the latest user message — always present (Zone 3).
	CurrentMessage *ProviderMessage

	// TotalTokens is the approximate token count of the assembled context.
	TotalTokens int

	// OutputReserve is tokens reserved for generation.
	OutputReserve int

	// InjectedPaths is the list of injected absolute file paths (for logging).
	InjectedPaths []string

	// CandidateCount is the number of CogDoc candidates considered before
	// budget eviction (and any pre-truncation step). Equals len(docCandidates)
	// at the moment of scoring; len(FovealDocs) is the post-eviction subset.
	CandidateCount int

	// Budget is the total context budget the assembler was invoked with
	// (after defaulting). Surfaced in debug snapshots so operators can see
	// the configured ceiling rather than a re-derived value.
	Budget int

	// FlexBudgetUsed is the sum of tokens spent on the flex zones (CogDocs
	// + conversation history) after eviction. Equal to the original flex
	// budget minus whatever remained unspent.
	FlexBudgetUsed int

	// PreviousTurnSpeculative is the barge-in-truncated suffix from the
	// previous turn — text the model generated but that was never played to
	// the user. Non-empty only when the previous turn was interrupted.
	// Injected into the system prompt as an agent-private block so the model
	// knows what it "almost said" without the user having heard it.
	PreviousTurnSpeculative string
}

// FovealDoc is a single CogDoc selected for context injection.
type FovealDoc struct {
	URI          string
	Path         string
	Title        string
	Content      string
	Summary      string
	SchemaIssues []string
	Salience     float64 // combined ranking score (relevance*2 + raw salience for keyword path; TRM score otherwise)
	Tokens       int
	Reason       string // "high-salience", "query-match", "both", or "trm"

	// Relevance is the raw query-keyword relevance the classifier saw. Stored
	// separately from Salience so the debug snapshot can report a value that
	// agrees with Reason: a doc with Reason="query-match" or "both" must have
	// Relevance > 0, and a doc with Reason="high-salience" must have
	// Relevance == 0. Without this field the snapshot displayed Relevance: 0
	// next to Reason: "both" because the raw value was thrown away after
	// being folded into Salience.
	Relevance float64
	// RawSalience is the raw salience (field score) before being combined
	// into Salience. Same rationale as Relevance: keeping the snapshot honest.
	RawSalience float64
}

// ScoredMessage is a conversation turn scored for retention.
type ScoredMessage struct {
	Role    string
	Content string
	// Preserve tool-call linkage and multi-modal parts through scoring so the
	// provider conversion can reconstruct Anthropic tool_use/tool_result blocks.
	// Dropping these reduced every history turn to {role, content}, which sent
	// role:"tool" results to Anthropic verbatim (rejected: "Unexpected role tool").
	ContentParts []ContentPart
	Name         string
	ToolCallID   string
	ToolCalls    []ToolCall
	Tokens       int
	TurnIndex      int     // 0 = oldest
	RecencyScore   float64 // 1.0 = most recent, decays toward 0
	RelevanceScore float64 // keyword overlap with current query
	CombinedScore  float64 // weighted combination
}

// estTokens approximates token count as chars/4 (fast, no tokenizer needed).
func estTokens(s string) int {
	n := (len(s) + 3) / 4
	if n < 0 {
		return 0
	}
	return n
}

// estTokensPrecise estimates tokens using rune-aware character-class heuristics.
//
// Heuristic selection:
//   - ASCII-heavy text: runes/4
//   - >20% non-ASCII: runes/2
//   - >30% non-alphanumeric: runes/3
//
// The result is never allowed to fall below the fast byte-based estimate, and a
// 10% safety margin is added to reduce underestimation near full contexts.
func estTokensPrecise(s string) int {
	if s == "" {
		return 0
	}

	runes := 0
	nonASCII := 0
	nonAlnum := 0
	for _, r := range s {
		runes++
		if r > unicode.MaxASCII {
			nonASCII++
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			nonAlnum++
		}
	}
	if runes == 0 {
		return 0
	}

	divisor := 4
	if nonASCII*5 > runes {
		divisor = 2
	}
	if nonAlnum*10 > runes*3 && divisor > 3 {
		divisor = 3
	}

	base := (runes + divisor - 1) / divisor
	if fast := estTokens(s); fast > base {
		base = fast
	}
	estimate := (base*11 + 9) / 10
	if estimate < 1 {
		return 1
	}
	return estimate
}

// AssembleContext builds a ContextPackage from the full client request.
//
// It decomposes the incoming messages[], scores conversation history alongside
// CogDocs, manages eviction when the budget is exceeded, and prepares the
// context for stability-ordered rendering.
//
// The budget is in approximate tokens (chars/4). Pass 0 to use the default (32768).
// ctx and convID are optional (pass context.Background() / "" when not available).
// When TRM is loaded and ctx is non-nil, TRM scoring is used for CogDoc ranking.
func (p *Process) AssembleContext(query string, messages []ProviderMessage, budget int, opts ...AssembleOption) (*ContextPackage, error) {
	ao := assembleDefaults()
	for _, o := range opts {
		o(&ao)
	}
	return p.assembleContextInnerWithOpts(ao.ctx, ao.convID, query, messages, budget, ao.manifestMode, ao.iris, ao.previousTurnSpeculative, ao.memoryNamespace)
}

// AssembleOption configures optional AssembleContext parameters.
type AssembleOption func(*assembleOpts)

type assembleOpts struct {
	ctx                     context.Context
	convID                  string
	iris                    irisSignal
	manifestMode            bool
	previousTurnSpeculative string
	// memoryNamespace, when non-empty, restricts CogDoc foveation to the
	// resolved filesystem path prefix for this namespace (G3 Part B). This
	// is a cog:// URI such as "cog://mem/semantic/agents/sandy/" that the
	// assembler resolves to a path prefix and uses as a filter. Empty means
	// no scoping — all indexed documents are eligible (today's behavior).
	memoryNamespace string
}

func assembleDefaults() assembleOpts {
	return assembleOpts{ctx: context.Background()}
}

// WithContext sets the request context for TRM embedding calls.
func WithContext(ctx context.Context) AssembleOption {
	return func(o *assembleOpts) { o.ctx = ctx }
}

// WithConversationID sets the conversation ID for light cone tracking.
func WithConversationID(id string) AssembleOption {
	return func(o *assembleOpts) { o.convID = id }
}

// WithIrisSignal sets the current context-window usage signal for pressure-aware
// token estimation.
func WithIrisSignal(signal irisSignal) AssembleOption {
	return func(o *assembleOpts) { o.iris = signal }
}

// WithManifestMode switches CogDoc injection from full-body content to
// summary manifests with on-demand retrieval.
func WithManifestMode(enabled bool) AssembleOption {
	return func(o *assembleOpts) { o.manifestMode = enabled }
}

// WithPreviousTurnSpeculative injects the barge-in-truncated suffix from the
// previous turn as an agent-private block in the assembled system prompt.
// Pass "" to omit the block (no barge-in on the previous turn).
func WithPreviousTurnSpeculative(text string) AssembleOption {
	return func(o *assembleOpts) { o.previousTurnSpeculative = text }
}

// WithMemoryScope restricts CogDoc foveation to documents whose filesystem
// paths fall within the resolved namespace (G3 Part B). namespace is a
// cog:// URI such as "cog://mem/semantic/agents/sandy/"; the assembler
// resolves it to a filesystem prefix and drops candidates outside that prefix.
//
// Pass "" to request no scoping — all indexed documents are eligible (this
// is the default and must be the behavior when IdentityNakedDefault=false).
func WithMemoryScope(namespace string) AssembleOption {
	return func(o *assembleOpts) { o.memoryNamespace = namespace }
}

func (p *Process) assembleContextInnerWithOpts(ctx context.Context, convID string, query string, messages []ProviderMessage, budget int, manifestMode bool, iris irisSignal, previousTurnSpeculative string, memoryNamespace string) (*ContextPackage, error) {
	if budget <= 0 {
		budget = p.cfg.EffectiveBudget()
	}

	estimateTokens := estTokens
	pressure := 0.0
	if iris.Size > 0 {
		pressure = float64(iris.Used) / float64(iris.Size)
		if pressure > 0.8 {
			estimateTokens = estTokensPrecise
		}
	}

	outputReserve := p.cfg.OutputReserve
	if outputReserve <= 0 {
		outputReserve = 4096
	}

	pkg := &ContextPackage{
		OutputReserve:           outputReserve,
		Budget:                  budget,
		PreviousTurnSpeculative: previousTurnSpeculative,
	}

	// === Decompose client messages ===

	// Extract client system prompt (messages[0] if role=="system").
	var clientMessages []ProviderMessage
	for _, m := range messages {
		if m.Role == "system" {
			pkg.ClientSystem = m.Content
		} else {
			clientMessages = append(clientMessages, m)
		}
	}

	// Separate current message (last user message) from history.
	var history []ProviderMessage
	if len(clientMessages) > 0 {
		last := clientMessages[len(clientMessages)-1]
		if last.Role == "user" {
			pkg.CurrentMessage = &last
			history = clientMessages[:len(clientMessages)-1]
		} else {
			// Last message isn't from user — keep all as history.
			history = clientMessages
		}
	}

	// === Fixed allocations (never evicted) ===

	p.nucleus.mu.RLock()
	nucleusCard := p.nucleus.Card
	p.nucleus.mu.RUnlock()
	pkg.NucleusText = nucleusCard

	nucleusTokens := estimateTokens(nucleusCard)
	clientSysTokens := estimateTokens(pkg.ClientSystem)
	currentMsgTokens := 0
	if pkg.CurrentMessage != nil {
		currentMsgTokens = estimateTokens(pkg.CurrentMessage.Content)
	}
	speculativeTokens := 0
	if previousTurnSpeculative != "" {
		// Approximate tokens for the injected speculative block (header + content).
		speculativeTokens = estimateTokens("<previous-turn-speculative>\n" + previousTurnSpeculative + "\n</previous-turn-speculative>")
	}

	// Budget available for CogDocs + conversation history.
	flexBudget := budget - outputReserve - nucleusTokens - clientSysTokens - currentMsgTokens - speculativeTokens
	if flexBudget < 0 {
		flexBudget = 0
	}

	// === Score CogDocs ===

	keywords := extractKeywords(query)
	cogIdx := p.Index()

	// Read gating knobs once at request start so a concurrent PATCH cannot
	// half-apply mid-assembly. See .cog/scratch/audit-dashboard-context/REPORT.md
	// §4 — without these the chat path admits every doc above zero salience.
	maxFovealDocs, salienceFloor := p.cfg.ContextGating()
	excludeSubstrings := p.cfg.ContextExcludeSubstrings()

	var docCandidates []FovealDoc
	usedTRM := false

	// G3 Part B: resolve memory namespace to a filesystem path prefix.
	// When memoryNamespace is non-empty, only documents whose absolute paths
	// start with this prefix are admitted into the candidate set. Empty prefix
	// means no scoping (all documents eligible) — this is the default and the
	// flag-off invariant.
	memScopePrefix := ""
	if memoryNamespace != "" {
		memScopePrefix = resolveMemoryNamespacePrefix(p.cfg.WorkspaceRoot, memoryNamespace)
	}

	// Try TRM scoring first (when model and embedding index are available).
	if p.trm != nil && p.embeddingIndex != nil && query != "" {
		trmResults := trmScoreDocs(ctx, p, query, convID, 100)
		if len(trmResults) > 0 {
			usedTRM = true
			// Build doc candidates from TRM results, using TRM score as primary ranking.
			for _, tr := range trmResults {
				score := float64(tr.TRMScore)
				if score < salienceFloor {
					continue
				}
				// G3 Part B: memory scope filter.
				if memScopePrefix != "" && !strings.HasPrefix(filepath.ToSlash(tr.IndexResult.ChunkMeta.Path), filepath.ToSlash(memScopePrefix)) {
					continue
				}
				docCandidates = append(docCandidates, FovealDoc{
					URI:         "cog://chunks/" + tr.IndexResult.ChunkMeta.ChunkID,
					Path:        tr.IndexResult.ChunkMeta.Path,
					Title:       tr.IndexResult.ChunkMeta.Title,
					Salience:    score,
					RawSalience: score,
					Reason:      "trm",
				})
			}
			// TRM results arrive pre-sorted by score; truncate after the floor
			// so the cap and the floor compose correctly.
			if maxFovealDocs > 0 && len(docCandidates) > maxFovealDocs {
				docCandidates = docCandidates[:maxFovealDocs]
			}
			slog.Debug("context: TRM scored candidates", "count", len(docCandidates))
		}
	}

	// Fall back to keyword + salience scoring when TRM is not available.
	if !usedTRM && cogIdx != nil && len(cogIdx.ByURI) > 0 {
		for _, doc := range cogIdx.ByURI {
			switch strings.ToLower(doc.Status) {
			case "superseded", "deprecated", "retired":
				continue
			}
			if strings.Contains(filepath.ToSlash(doc.Path), "/archive/") {
				continue
			}
			if pathMatchesExcludeSubstrings(doc.Path, excludeSubstrings) {
				continue
			}
			// G3 Part B: memory scope filter.
			if memScopePrefix != "" && !strings.HasPrefix(filepath.ToSlash(doc.Path), filepath.ToSlash(memScopePrefix)) {
				continue
			}

			relevance := queryRelevance(doc, keywords)
			salience := p.field.Score(doc.Path)
			if relevance <= 0 && salience <= 0 {
				continue
			}

			combined := relevance*2.0 + salience
			if combined < salienceFloor {
				continue
			}

			reason := "high-salience"
			switch {
			case relevance > 0 && salience > 0:
				reason = "both"
			case relevance > 0:
				reason = "query-match"
			}

			docCandidates = append(docCandidates, FovealDoc{
				URI:         doc.URI,
				Path:        doc.Path,
				Title:       doc.Title,
				Salience:    combined,
				Relevance:   relevance,
				RawSalience: salience,
				Reason:      reason,
			})
		}
		sort.Slice(docCandidates, func(i, j int) bool {
			return docCandidates[i].Salience > docCandidates[j].Salience
		})
		// Cap after sort so the highest-scoring docs survive.
		if maxFovealDocs > 0 && len(docCandidates) > maxFovealDocs {
			docCandidates = docCandidates[:maxFovealDocs]
		}
	}

	// === Score conversation history ===

	scoredHistory := scoreConversationWithEstimator(history, keywords, estimateTokens)

	// CandidateCount is the count of CogDocs that survived ranking but
	// before any budget-driven eviction. This is what operators want to see
	// in `engine.cogdocs_scored`: a faithful "we considered N candidates
	// and chose K" record.
	pkg.CandidateCount = len(docCandidates)

	// === Evict to fit budget ===

	pkg.FovealDocs, pkg.Conversation = evictForBudgetModeWithEstimator(docCandidates, scoredHistory, flexBudget, p.cfg.WorkspaceRoot, manifestMode, estimateTokens)

	// Compute total tokens and flex usage. flex_budget_used is the sum of
	// post-eviction CogDoc + conversation zone tokens (i.e. everything that
	// landed in the flex zones).
	total := nucleusTokens + clientSysTokens + currentMsgTokens
	flexUsed := 0
	for _, d := range pkg.FovealDocs {
		total += d.Tokens
		flexUsed += d.Tokens
		pkg.InjectedPaths = append(pkg.InjectedPaths, d.Path)
	}
	for _, m := range pkg.Conversation {
		total += m.Tokens
		flexUsed += m.Tokens
	}
	pkg.TotalTokens = total
	pkg.FlexBudgetUsed = flexUsed

	// Record assembly event.
	p.emitEvent("context.assembly", map[string]interface{}{
		"query":            query,
		"keywords":         keywords,
		"injected_docs":    len(pkg.FovealDocs),
		"conversation_len": len(pkg.Conversation),
		"total_tokens":     pkg.TotalTokens,
		"budget":           budget,
		"output_reserve":   outputReserve,
		"flex_budget":      flexBudget,
		"iris_pressure":    pressure,
		"precise_tokens":   pressure > 0.8,
		"used_trm":         usedTRM,
	})

	slog.Info("context: assembled",
		"docs", len(pkg.FovealDocs),
		"conv_turns", len(pkg.Conversation),
		"tokens", pkg.TotalTokens,
		"budget", budget,
		"pressure", fmt.Sprintf("%.1f%%", pressure*100),
	)

	return pkg, nil
}

// FormatForProvider renders a ContextPackage as (systemPrompt, messages) for the provider.
//
// The system prompt is stability-ordered for KV cache optimization:
// nucleus → client system prompt → CogDocs (by salience descending).
//
// Messages are in chronological order: conversation history → current message.
func (pkg *ContextPackage) FormatForProvider() (string, []ProviderMessage) {
	// === System prompt (Zone 0 + Zone 1) ===
	var sb strings.Builder

	if pkg.NucleusText != "" {
		sb.WriteString(pkg.NucleusText)
	}

	if pkg.ClientSystem != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n---\n")
		}
		sb.WriteString("# Client Context\n\n")
		sb.WriteString(pkg.ClientSystem)
	}

	if len(pkg.FovealDocs) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n\n---\n")
		}
		if docsUseManifest(pkg.FovealDocs) {
			sb.WriteString(renderWorkspaceManifest(pkg.FovealDocs))
		} else {
			sb.WriteString("# Workspace Context\n\n")
			for _, doc := range pkg.FovealDocs {
				fmt.Fprintf(&sb, "## %s\n\n", doc.Title)
				sb.WriteString(doc.Content)
				sb.WriteString("\n\n")
			}
		}
	}

	// Inject speculative-output block as agent-private context (Slice 4).
	// This block is visible only to the model — it is NOT shown to the user.
	// It carries the suffix of the previous response that was generated but
	// never delivered because the user barged in. The model can use it to
	// decide whether to repeat, continue, or drop the unsaid content.
	if pkg.PreviousTurnSpeculative != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n\n---\n")
		}
		sb.WriteString("<previous-turn-speculative>\n")
		sb.WriteString("The following text was generated in your previous response but was NOT delivered to the user (they interrupted before it could be spoken). You may choose to naturally resume, re-state, or drop this content based on their new message.\n\n")
		sb.WriteString(pkg.PreviousTurnSpeculative)
		sb.WriteString("\n</previous-turn-speculative>")
	}

	systemPrompt := sb.String()

	// === Messages (Zone 2 + Zone 3) ===
	var msgs []ProviderMessage
	for _, sm := range pkg.Conversation {
		msgs = append(msgs, ProviderMessage{
			Role:         sm.Role,
			Content:      sm.Content,
			ContentParts: sm.ContentParts,
			Name:         sm.Name,
			ToolCallID:   sm.ToolCallID,
			ToolCalls:    sm.ToolCalls,
		})
	}
	if pkg.CurrentMessage != nil {
		msgs = append(msgs, *pkg.CurrentMessage)
	}

	// Tool-pairing repair is now handled by the wire-layer normalizer
	// (normalizeAnthropicMessages) inside buildAnthropicRequest, which operates
	// on the FINAL block structure and can enforce I4 block-order as well.
	// The pre-conversion repairToolPairing call here is subsumed and removed.

	return systemPrompt, msgs
}

// scoreConversation scores conversation turns by recency and query relevance.
// Returns ScoredMessage slice preserving chronological order.
func scoreConversation(history []ProviderMessage, keywords []string) []ScoredMessage {
	return scoreConversationWithEstimator(history, keywords, estTokens)
}

func scoreConversationWithEstimator(history []ProviderMessage, keywords []string, estimateTokens func(string) int) []ScoredMessage {
	if len(history) == 0 {
		return nil
	}

	total := len(history)
	scored := make([]ScoredMessage, total)

	for i, m := range history {
		recency := float64(i+1) / float64(total) // 0→1, newest = highest
		relevance := messageRelevance(m.Content, keywords)

		scored[i] = ScoredMessage{
			Role:           m.Role,
			Content:        m.Content,
			ContentParts:   m.ContentParts,
			Name:           m.Name,
			ToolCallID:     m.ToolCallID,
			ToolCalls:      m.ToolCalls,
			Tokens:         estimateTokens(m.Content),
			TurnIndex:      i,
			RecencyScore:   recency,
			RelevanceScore: relevance,
			CombinedScore:  0.6*recency + 0.4*relevance,
		}
	}

	return scored
}

// evictForBudget fills the flex budget with the highest-value CogDocs and
// conversation turns. When the budget is exceeded, low-value items are dropped.
//
// CogDocs are read from disk and token-counted during this phase.
// Conversation turns are evicted in user/assistant pairs to maintain coherence.
func evictForBudget(docs []FovealDoc, conv []ScoredMessage, budget int, workspaceRoot string) ([]FovealDoc, []ScoredMessage) {
	return evictForBudgetMode(docs, conv, budget, workspaceRoot, false)
}

func evictForBudgetMode(docs []FovealDoc, conv []ScoredMessage, budget int, workspaceRoot string, manifestMode bool) ([]FovealDoc, []ScoredMessage) {
	return evictForBudgetModeWithEstimator(docs, conv, budget, workspaceRoot, manifestMode, estTokens)
}

func evictForBudgetModeWithEstimator(docs []FovealDoc, conv []ScoredMessage, budget int, workspaceRoot string, manifestMode bool, estimateTokens func(string) int) ([]FovealDoc, []ScoredMessage) {
	if budget <= 0 {
		return nil, nil
	}

	remaining := budget

	// Phase 1: Fill with top CogDocs (they provide grounding).
	var keptDocs []FovealDoc
	skippedManifest := 0
	skippedBudget := 0
	skippedRead := 0
	for _, doc := range docs {
		if remaining <= 0 {
			break
		}
		if manifestMode {
			manifestDoc, err := buildManifestDocWithEstimator(doc, workspaceRoot, estimateTokens)
			if err != nil || manifestDoc.Summary == "" {
				skippedManifest++
				slog.Debug("evict: manifest build failed",
					"path", doc.Path,
					"err", err,
					"summary_empty", manifestDoc.Summary == "",
				)
				continue
			}
			if manifestDoc.Tokens > remaining {
				skippedBudget++
				slog.Debug("evict: doc exceeds remaining budget",
					"path", doc.Path,
					"tokens", manifestDoc.Tokens,
					"remaining", remaining,
				)
				continue
			}
			doc = manifestDoc
		} else {
			readPath := doc.Path
			if !filepath.IsAbs(readPath) && workspaceRoot != "" {
				readPath = filepath.Join(workspaceRoot, readPath)
			}
			content, err := readDocContent(readPath, remaining)
			if err != nil || content == "" {
				skippedRead++
				slog.Debug("evict: content read failed",
					"path", readPath,
					"err", err,
					"content_empty", content == "",
				)
				continue
			}
			tokens := estimateTokens(content)
			title := doc.Title
			if title == "" {
				title = filepath.Base(doc.Path)
			}
			doc.Title = title
			doc.Content = content
			doc.Tokens = tokens
		}
		keptDocs = append(keptDocs, doc)
		remaining -= doc.Tokens
	}
	if skippedManifest > 0 || skippedBudget > 0 || skippedRead > 0 {
		slog.Info("evict: docs skipped",
			"manifest_err", skippedManifest,
			"budget_exceeded", skippedBudget,
			"read_err", skippedRead,
			"kept", len(keptDocs),
			"total_input", len(docs),
		)
	}

	// Phase 2: Fill remaining with conversation history.
	// Selection prefers high-CombinedScore turns; on ties (e.g. when scoring
	// inputs are absent), prefer newer turns. User/assistant pairs are kept
	// together. Output is restored to chronological order.
	keptConv := selectConversationTurns(conv, remaining)

	return keptDocs, keptConv
}

// selectConversationTurns picks turns to retain under a token budget.
//
// Selection ordering: highest CombinedScore first; ties broken by newer
// turn (higher TurnIndex). User/assistant adjacent pairs are evaluated as
// a unit (pair score = max of the two CombinedScores) and admitted or
// rejected together. Standalone messages (system, tool_result, or an
// unpaired user/assistant) are evaluated individually.
//
// The returned slice is in chronological (TurnIndex) order.
func selectConversationTurns(conv []ScoredMessage, budget int) []ScoredMessage {
	if budget <= 0 || len(conv) == 0 {
		return nil
	}

	// Build admission units: either a (user, assistant) pair or a standalone.
	type unit struct {
		indices []int   // indexes into conv, in chronological order
		tokens  int     // total token cost
		score   float64 // priority score (max CombinedScore among members)
		newest  int     // newest conv index in the unit, for tie-break
	}

	var units []unit
	for i := 0; i < len(conv); i++ {
		m := conv[i]
		// Detect adjacent user→assistant pair.
		if m.Role == "user" && i+1 < len(conv) && conv[i+1].Role == "assistant" {
			a := conv[i+1]
			score := m.CombinedScore
			if a.CombinedScore > score {
				score = a.CombinedScore
			}
			units = append(units, unit{
				indices: []int{i, i + 1},
				tokens:  m.Tokens + a.Tokens,
				score:   score,
				newest:  i + 1,
			})
			i++
			continue
		}
		units = append(units, unit{
			indices: []int{i},
			tokens:  m.Tokens,
			score:   m.CombinedScore,
			newest:  i,
		})
	}

	// Sort units: highest score first, then newer conv position (tie-break).
	sort.SliceStable(units, func(i, j int) bool {
		if units[i].score != units[j].score {
			return units[i].score > units[j].score
		}
		return units[i].newest > units[j].newest
	})

	remaining := budget
	keptIdx := make([]int, 0, len(conv))
	for _, u := range units {
		if remaining <= 0 {
			break
		}
		if u.tokens <= remaining {
			keptIdx = append(keptIdx, u.indices...)
			remaining -= u.tokens
		}
	}

	// Restore chronological order.
	sort.Ints(keptIdx)
	out := make([]ScoredMessage, 0, len(keptIdx))
	for _, idx := range keptIdx {
		out = append(out, conv[idx])
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// repairToolPairing drops orphan tool_use / tool_result messages from a
// ProviderMessage slice so that Anthropic's pairing invariant always holds.
//
// SUPERSEDED: this function is no longer called from production paths. Its
// logic is subsumed by normalizeAnthropicMessages (anthropic_normalize.go)
// which operates on the FINAL []anthropicMessage block structure and additionally
// enforces I4 block-order. This function is retained to keep existing tests
// (TestRepairToolPairing_*) passing during the transition.
//
//   - Every role=="tool" message must have a matching role=="assistant" message
//     with a ToolCall whose ID equals ToolCallID immediately upstream.
//   - Every role=="assistant" message with ToolCalls must have a corresponding
//     role=="tool" result for each ToolCall.ID downstream.
//
// The function is deliberately drop-only (no stub injection) because orphaned
// calls were truncated by the budget eviction loop — injecting stubs risks the
// model re-acting on phantom calls. An assistant with all its results evicted
// but non-empty Content is kept as plain assistant text (ToolCalls=nil).
//
// Algorithm (two-pass + fixpoint reconcile):
//
//	Pass A — drop orphan tool_result messages (tool_result whose tool_use is
//	         absent upstream).  Walks the slice once with a rolling set of known
//	         IDs that is reset on each new assistant message and cleared on each
//	         real user message.  Matches Hermes _repair_message_sequence pass 1.
//
//	Pass B — reconcile assistant tool_use against surviving tool_result messages.
//	         Builds resultIDs from surviving role=="tool" messages; per assistant
//	         with ToolCalls, keeps only tc entries whose ID appears in resultIDs;
//	         drops the whole assistant message if all ToolCalls are gone AND
//	         Content is empty.
//
//	Reconcile — runs A->B, then re-drops any tool_result whose assistant was
//	            dropped in Pass B (single fixpoint; the repair is idempotent on
//	            the result because Pass A runs first on the already-cleaned
//	            slice).
//
// Returns the repaired slice and the number of messages dropped.
func repairToolPairing(msgs []ProviderMessage) ([]ProviderMessage, int) {
	// Fast path: nothing to repair when there are no tool messages.
	hasTools := false
	for i := range msgs {
		if msgs[i].Role == "tool" || (msgs[i].Role == "assistant" && len(msgs[i].ToolCalls) > 0) {
			hasTools = true
			break
		}
	}
	if !hasTools {
		return msgs, 0
	}

	dropped := 0

	// -- Pass A: drop orphan tool_result messages --------------------------------
	// A tool_result is orphaned when its ToolCallID is not present in the
	// immediately-preceding assistant message's ToolCalls set.
	knownIDs := make(map[string]bool)
	passA := make([]ProviderMessage, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "assistant":
			// Reset the known-ID set for each new assistant message.
			knownIDs = make(map[string]bool)
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					knownIDs[tc.ID] = true
				}
			}
			passA = append(passA, m)
		case "tool":
			if m.ToolCallID != "" && knownIDs[m.ToolCallID] {
				passA = append(passA, m)
			} else {
				// Orphan tool_result -- no preceding assistant tool_use.
				dropped++
				slog.Debug("repairToolPairing: dropped orphan tool_result",
					"tool_call_id", m.ToolCallID,
				)
			}
		default:
			// A user (or system) message closes the current tool-result run;
			// subsequent tool messages without a fresh assistant tool_call
			// are orphans.  Clear the known set.
			knownIDs = make(map[string]bool)
			passA = append(passA, m)
		}
	}

	// -- Pass B: drop assistant tool_uses with no downstream tool_result --------
	// Build the set of surviving result IDs from Pass A output.
	resultIDs := make(map[string]bool)
	for _, m := range passA {
		if m.Role == "tool" && m.ToolCallID != "" {
			resultIDs[m.ToolCallID] = true
		}
	}

	passB := make([]ProviderMessage, 0, len(passA))
	for _, m := range passA {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			passB = append(passB, m)
			continue
		}
		// Keep only ToolCalls that have a surviving result.
		var kept []ToolCall
		for _, tc := range m.ToolCalls {
			if tc.ID != "" && resultIDs[tc.ID] {
				kept = append(kept, tc)
			} else {
				slog.Debug("repairToolPairing: dropped orphan tool_use",
					"tool_call_id", tc.ID,
					"tool_name", tc.Name,
				)
				dropped++
			}
		}
		if len(kept) == 0 && m.Content == "" {
			// All tool_uses gone and no text -- drop the whole assistant message.
			dropped++
			slog.Debug("repairToolPairing: dropped assistant message with all tool_uses gone and no text")
			continue
		}
		m.ToolCalls = kept // nil when kept is nil (all results evicted, text kept)
		passB = append(passB, m)
	}

	// -- Reconcile: re-drop tool_result whose assistant was dropped in Pass B ---
	// After Pass B some assistants may have been dropped, leaving tool_result
	// messages that survived Pass A but are now orphaned.
	survivingAssistantIDs := make(map[string]bool)
	for _, m := range passB {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.ID != "" {
					survivingAssistantIDs[tc.ID] = true
				}
			}
		}
	}
	out := make([]ProviderMessage, 0, len(passB))
	for _, m := range passB {
		if m.Role == "tool" && m.ToolCallID != "" && !survivingAssistantIDs[m.ToolCallID] {
			dropped++
			slog.Debug("repairToolPairing: dropped newly-orphaned tool_result after Pass B",
				"tool_call_id", m.ToolCallID,
			)
			continue
		}
		out = append(out, m)
	}

	// -- Merge consecutive plain-text user messages created by Pass B drops -----
	// When a dropped assistant sat between two user messages they become adjacent.
	// Only merge string-content users; leave multimodal (ContentParts) users alone.
	merged := make([]ProviderMessage, 0, len(out))
	for _, m := range out {
		if len(merged) > 0 &&
			m.Role == "user" && merged[len(merged)-1].Role == "user" &&
			len(m.ContentParts) == 0 && len(merged[len(merged)-1].ContentParts) == 0 {
			prev := &merged[len(merged)-1]
			if prev.Content != "" && m.Content != "" {
				prev.Content = prev.Content + "\n\n" + m.Content
			} else {
				prev.Content = prev.Content + m.Content
			}
			dropped++ // count the merge as a drop
			continue
		}
		merged = append(merged, m)
	}

	return merged, dropped
}

// messageRelevance scores a message's content against query keywords.
func messageRelevance(content string, keywords []string) float64 {
	if len(keywords) == 0 {
		return 0.0
	}
	lower := strings.ToLower(content)
	matches := 0
	for _, kw := range keywords {
		if strings.Contains(lower, kw) {
			matches++
		}
	}
	return float64(matches) / float64(len(keywords))
}

// extractKeywords splits a query into lowercase, de-stopworded keywords.
func extractKeywords(query string) []string {
	stopWords := map[string]bool{
		"a": true, "an": true, "the": true, "is": true, "are": true,
		"was": true, "were": true, "be": true, "been": true, "being": true,
		"have": true, "has": true, "had": true, "do": true, "does": true,
		"did": true, "will": true, "would": true, "could": true, "should": true,
		"may": true, "might": true, "must": true, "shall": true, "can": true,
		"and": true, "but": true, "or": true, "for": true, "nor": true,
		"so": true, "yet": true, "at": true, "by": true, "in": true,
		"of": true, "on": true, "to": true, "up": true, "as": true,
		"it": true, "its": true, "this": true, "that": true, "with": true,
		"from": true, "into": true, "what": true, "how": true, "why": true,
		"when": true, "where": true, "who": true, "which": true,
		"explain": true, "describe": true, "tell": true, "me": true,
		"about": true, "give": true, "your": true, "you": true, "our": true,
		"we": true, "my": true, "please": true, "just": true, "more": true,
	}

	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_'
	})

	var keywords []string
	seen := map[string]bool{}
	for _, w := range fields {
		if len(w) >= 3 && !stopWords[w] && !seen[w] {
			keywords = append(keywords, w)
			seen[w] = true
		}
	}
	return keywords
}

// pathMatchesExcludeSubstrings reports whether the slash-normalised path
// contains any of the configured exclude substrings. Each entry is treated as
// a literal path substring (not a shell glob), consistent with the existing
// /archive/ and /inbox/ exclusion rules used elsewhere in the assembler.
// An empty list always returns false.
func pathMatchesExcludeSubstrings(path string, substrings []string) bool {
	if len(substrings) == 0 {
		return false
	}
	slashed := filepath.ToSlash(path)
	for _, sub := range substrings {
		if sub != "" && strings.Contains(slashed, sub) {
			return true
		}
	}
	return false
}

// queryRelevance scores a CogDoc against a keyword set.
func queryRelevance(doc *IndexedCogdoc, keywords []string) float64 {
	if len(keywords) == 0 {
		return 0.0
	}
	meta := strings.ToLower(
		doc.Title + " " +
			doc.ID + " " +
			strings.Join(doc.Tags, " ") + " " +
			filepath.Base(doc.Path),
	)
	matches := 0
	for _, kw := range keywords {
		if strings.Contains(meta, kw) {
			matches++
		}
	}
	return float64(matches) / float64(len(keywords))
}

// readDocContent reads a CogDoc's body (frontmatter stripped), capped at maxTokens.
func readDocContent(path string, maxTokens int) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	_, body := parseCogdocFrontmatter(string(data))
	body = strings.TrimSpace(body)

	maxChars := maxTokens * 4
	if maxChars <= 0 {
		return "", nil
	}
	if len(body) > maxChars {
		body = body[:maxChars]
		if idx := strings.LastIndex(body, "\n"); idx > maxChars*3/4 {
			body = body[:idx]
		}
		body += "\n... [truncated]"
	}

	return body, nil
}

func docsUseManifest(docs []FovealDoc) bool {
	if len(docs) == 0 {
		return false
	}
	for _, doc := range docs {
		if doc.Content != "" || doc.Summary == "" {
			return false
		}
	}
	return true
}

func renderWorkspaceManifest(docs []FovealDoc) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Workspace Context (%d relevant CogDocs)\n", len(docs))
	sb.WriteString("# Use cog_read_cogdoc to access full content when needed\n\n")
	for _, doc := range docs {
		uri := doc.URI
		if uri == "" {
			uri = doc.Path
		}
		fmt.Fprintf(&sb, "- %s — %s [salience: %.2f]\n", uri, doc.Summary, doc.Salience)
	}

	var schemaNotes []string
	for _, doc := range docs {
		if len(doc.SchemaIssues) == 0 {
			continue
		}
		uri := doc.URI
		if uri == "" {
			uri = doc.Path
		}
		schemaNotes = append(schemaNotes, fmt.Sprintf("- %s — missing: %s", uri, strings.Join(schemaIssueFields(doc.SchemaIssues), ", ")))
	}
	if len(schemaNotes) > 0 {
		sb.WriteString("\n## Schema Notes\n")
		sb.WriteString("# These CogDocs are missing required fields. When you read them with cog_read_cogdoc,\n")
		sb.WriteString("# include a 'patch_frontmatter' object in your response with the missing fields filled in.\n")
		for _, note := range schemaNotes {
			sb.WriteString(note)
			sb.WriteString("\n")
		}
	}

	return strings.TrimRight(sb.String(), "\n")
}

func buildManifestDocWithEstimator(doc FovealDoc, workspaceRoot string, estimateTokens func(string) int) (FovealDoc, error) {
	absPath := doc.Path
	if !filepath.IsAbs(absPath) && workspaceRoot != "" {
		absPath = filepath.Join(workspaceRoot, absPath)
	}
	source, err := readManifestSource(absPath, 100)
	if err != nil {
		return FovealDoc{}, err
	}

	fm, body := parseCogdocFrontmatter(source)
	title := firstNonBlank(strings.TrimSpace(doc.Title), strings.TrimSpace(fm.Title), filepath.Base(doc.Path))
	if title == "" {
		title = filepath.Base(doc.Path)
	}

	summary := strings.TrimSpace(normalizeManifestText(fm.Description))
	if summary == "" {
		excerpt := manifestBodyExcerpt(body, 100)
		summary = title
		if excerpt != "" {
			summary += ": " + excerpt
		}
	}

	uri := strings.TrimSpace(doc.URI)
	if uri == "" || strings.HasPrefix(uri, "cog://chunks/") {
		if resolved, err := PathToURI(workspaceRoot, doc.Path); err == nil {
			uri = resolved
		}
	}

	doc.URI = uri
	doc.Title = title
	doc.Content = ""
	doc.Summary = summary
	doc.SchemaIssues = missingSchemaIssues(source)
	doc.Tokens = estimateTokens(fmt.Sprintf("- %s — %s [salience: %.2f]", firstNonBlank(uri, doc.Path), summary, doc.Salience))
	return doc, nil
}

func readManifestSource(path string, minBodyChars int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	defer f.Close()

	buf := make([]byte, 4096)
	var data []byte
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			data = append(data, buf[:n]...)
		}

		content := string(data)
		if !hasFrontmatterPrefix(content) {
			if len(normalizeManifestText(content)) >= minBodyChars || readErr == io.EOF {
				return content, nil
			}
		} else {
			_, body := parseCogdocFrontmatter(content)
			if body != content && len(normalizeManifestText(body)) >= minBodyChars {
				return content, nil
			}
			if readErr == io.EOF {
				return content, nil
			}
		}

		if readErr == io.EOF {
			return content, nil
		}
		if readErr != nil {
			return "", fmt.Errorf("read %s: %w", path, readErr)
		}
	}
}

func hasFrontmatterPrefix(content string) bool {
	return strings.HasPrefix(content, "---\n") || strings.HasPrefix(content, "---\r\n")
}

func extractFrontmatterYAML(content string) (string, string, bool) {
	skipBytes := 0
	switch {
	case strings.HasPrefix(content, "---\n"):
		skipBytes = 4
	case strings.HasPrefix(content, "---\r\n"):
		skipBytes = 5
	default:
		return "", content, false
	}

	rest := content[skipBytes:]
	yamlBlock, tail, found := strings.Cut(rest, "\n---")
	if !found {
		return "", content, false
	}
	body := strings.TrimLeft(tail, "\r\n")
	return yamlBlock, body, true
}

func missingSchemaIssues(content string) []string {
	presence := frontmatterPresence(content)
	var issues []string
	if !presence["description"] {
		issues = append(issues, "missing_description")
	}
	if !presence["tags"] {
		issues = append(issues, "missing_tags")
	}
	if !presence["type"] {
		issues = append(issues, "missing_type")
	}
	return issues
}

func frontmatterPresence(content string) map[string]bool {
	presence := map[string]bool{}
	yamlBlock, _, ok := extractFrontmatterYAML(content)
	if !ok {
		return presence
	}

	var raw map[string]any
	if err := yaml.Unmarshal([]byte(yamlBlock), &raw); err != nil {
		return presence
	}
	_, presence["description"] = raw["description"]
	_, presence["tags"] = raw["tags"]
	_, presence["type"] = raw["type"]
	return presence
}

func manifestBodyExcerpt(body string, maxChars int) string {
	normalized := normalizeManifestText(body)
	if normalized == "" {
		return ""
	}
	if len(normalized) <= maxChars {
		return normalized
	}
	return normalized[:maxChars] + "..."
}

func normalizeManifestText(s string) string {
	if s == "" {
		return ""
	}
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}

func schemaIssueFields(issues []string) []string {
	order := []struct {
		issue string
		field string
	}{
		{issue: "missing_description", field: "description"},
		{issue: "missing_tags", field: "tags"},
		{issue: "missing_type", field: "type"},
	}
	issueSet := make(map[string]bool, len(issues))
	for _, issue := range issues {
		issueSet[issue] = true
	}
	var fields []string
	for _, item := range order {
		if issueSet[item.issue] {
			fields = append(fields, item.field)
		}
	}
	return fields
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
