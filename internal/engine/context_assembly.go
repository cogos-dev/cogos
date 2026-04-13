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
}

// FovealDoc is a single CogDoc selected for context injection.
type FovealDoc struct {
	URI          string
	Path         string
	Title        string
	Content      string
	Summary      string
	SchemaIssues []string
	Salience     float64
	Tokens       int
	Reason       string // "high-salience", "query-match", or "both"
}

// ScoredMessage is a conversation turn scored for retention.
type ScoredMessage struct {
	Role           string
	Content        string
	Tokens         int
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
	return p.assembleContextInnerWithOpts(ao.ctx, ao.convID, query, messages, budget, ao.manifestMode, ao.iris)
}

// AssembleOption configures optional AssembleContext parameters.
type AssembleOption func(*assembleOpts)

type assembleOpts struct {
	ctx          context.Context
	convID       string
	iris         irisSignal
	manifestMode bool
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

func (p *Process) assembleContextInnerWithOpts(ctx context.Context, convID string, query string, messages []ProviderMessage, budget int, manifestMode bool, iris irisSignal) (*ContextPackage, error) {
	if budget <= 0 {
		budget = 32768
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

	pkg := &ContextPackage{OutputReserve: outputReserve}

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

	// Budget available for CogDocs + conversation history.
	flexBudget := budget - outputReserve - nucleusTokens - clientSysTokens - currentMsgTokens
	if flexBudget < 0 {
		flexBudget = 0
	}

	// === Score CogDocs ===

	keywords := extractKeywords(query)
	cogIdx := p.Index()

	var docCandidates []FovealDoc
	usedTRM := false

	// Try TRM scoring first (when model and embedding index are available).
	if p.trm != nil && p.embeddingIndex != nil && query != "" {
		trmResults := trmScoreDocs(ctx, p, query, convID, 100)
		if len(trmResults) > 0 {
			usedTRM = true
			// Build doc candidates from TRM results, using TRM score as primary ranking.
			for _, tr := range trmResults {
				docCandidates = append(docCandidates, FovealDoc{
					URI:      "cog://chunks/" + tr.IndexResult.ChunkMeta.ChunkID,
					Path:     tr.IndexResult.ChunkMeta.Path,
					Title:    tr.IndexResult.ChunkMeta.Title,
					Salience: float64(tr.TRMScore),
					Reason:   "trm",
				})
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

			relevance := queryRelevance(doc, keywords)
			salience := p.field.Score(doc.Path)
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

			docCandidates = append(docCandidates, FovealDoc{
				URI:      doc.URI,
				Path:     doc.Path,
				Title:    doc.Title,
				Salience: relevance*2.0 + salience,
				Reason:   reason,
			})
		}
		sort.Slice(docCandidates, func(i, j int) bool {
			return docCandidates[i].Salience > docCandidates[j].Salience
		})
	}

	// === Score conversation history ===

	scoredHistory := scoreConversationWithEstimator(history, keywords, estimateTokens)

	// === Evict to fit budget ===

	pkg.FovealDocs, pkg.Conversation = evictForBudgetModeWithEstimator(docCandidates, scoredHistory, flexBudget, p.cfg.WorkspaceRoot, manifestMode, estimateTokens)

	// Compute total tokens.
	total := nucleusTokens + clientSysTokens + currentMsgTokens
	for _, d := range pkg.FovealDocs {
		total += d.Tokens
		pkg.InjectedPaths = append(pkg.InjectedPaths, d.Path)
	}
	for _, m := range pkg.Conversation {
		total += m.Tokens
	}
	pkg.TotalTokens = total

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

	systemPrompt := sb.String()

	// === Messages (Zone 2 + Zone 3) ===
	var msgs []ProviderMessage
	for _, sm := range pkg.Conversation {
		msgs = append(msgs, ProviderMessage{Role: sm.Role, Content: sm.Content})
	}
	if pkg.CurrentMessage != nil {
		msgs = append(msgs, *pkg.CurrentMessage)
	}

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

	// Phase 2: Fill remaining with conversation history (newest first).
	// We want to keep recent turns, so iterate from newest to oldest,
	// then reverse to restore chronological order.
	var keptConv []ScoredMessage
	if remaining > 0 && len(conv) > 0 {
		// Iterate newest to oldest.
		for i := len(conv) - 1; i >= 0; i-- {
			if remaining <= 0 {
				break
			}
			m := conv[i]
			if m.Role == "assistant" && i > 0 && conv[i-1].Role == "user" {
				pairTokens := conv[i-1].Tokens + m.Tokens
				if pairTokens <= remaining {
					keptConv = append(keptConv, m, conv[i-1])
					remaining -= pairTokens
				}
				i--
				continue
			}
			if m.Tokens <= remaining {
				keptConv = append(keptConv, m)
				remaining -= m.Tokens
			}
		}
		// Reverse to restore chronological order.
		for i, j := 0, len(keptConv)-1; i < j; i, j = i+1, j-1 {
			keptConv[i], keptConv[j] = keptConv[j], keptConv[i]
		}
	}

	return keptDocs, keptConv
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
