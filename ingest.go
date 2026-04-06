//go:build mcpserver

// ingest.go — Core types and interfaces for the deterministic ingestion pipeline.
//
// The ingestion pipeline decomposes heterogeneous input (URLs, conversations,
// documents) into normalized IngestResult records suitable for the inbox/memory
// lifecycle. Format-specific logic lives in Decomposer implementations; this
// file defines the shared vocabulary and the pipeline orchestrator.
package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Ledger event type for ingestion
// ---------------------------------------------------------------------------

// IngestEventType is the ledger event type emitted when new material is ingested.
const IngestEventType = "ingested"

// ---------------------------------------------------------------------------
// Source enum — where the material came from
// ---------------------------------------------------------------------------

// IngestSource identifies the origin system of ingested material.
type IngestSource string

const (
	SourceDiscord IngestSource = "discord"
	SourceChatGPT IngestSource = "chatgpt"
	SourceClaude  IngestSource = "claude"
	SourceGemini  IngestSource = "gemini"
	SourceURL     IngestSource = "url"
	SourceFile    IngestSource = "file"
)

// ---------------------------------------------------------------------------
// Format enum — structural shape of the input
// ---------------------------------------------------------------------------

// IngestFormat describes the structural format of the raw input data.
type IngestFormat string

const (
	FormatURL          IngestFormat = "url"
	FormatConversation IngestFormat = "conversation"
	FormatMessage      IngestFormat = "message"
	FormatDocument     IngestFormat = "document"
)

// ---------------------------------------------------------------------------
// ContentType enum — classified type of the extracted content
// ---------------------------------------------------------------------------

// ContentType is the semantic classification assigned after decomposition.
type ContentType string

const (
	ContentPaper      ContentType = "paper"
	ContentRepo       ContentType = "repo"
	ContentVideo      ContentType = "video"
	ContentArticle    ContentType = "article"
	ContentDiscussion ContentType = "discussion"
	ContentTool       ContentType = "tool"
	ContentUnknown    ContentType = "unknown"
)

// ---------------------------------------------------------------------------
// Request / Result
// ---------------------------------------------------------------------------

// IngestRequest is the input to the ingestion pipeline.
type IngestRequest struct {
	Source   IngestSource      `json:"source"`
	Format   IngestFormat      `json:"format"`
	Data     string            `json:"data"`     // raw material (URL, JSON blob, text)
	Metadata map[string]string `json:"metadata"` // optional context (discord_message_id, channel, etc.)
}

// IngestResult is the normalized output of decomposition.
type IngestResult struct {
	Title       string            `json:"title"`
	URL         string            `json:"url,omitempty"`
	Domain      string            `json:"domain,omitempty"`
	ContentType ContentType       `json:"content_type"`
	Tags        []string          `json:"tags"`
	Summary     string            `json:"summary,omitempty"` // first ~500 chars or abstract
	Fields      map[string]string `json:"fields,omitempty"`  // type-specific extracted fields
	Source      IngestSource      `json:"source"`
	SourceID    string            `json:"source_id,omitempty"` // dedup key (URL, message ID, etc.)
}

// ---------------------------------------------------------------------------
// Decomposer interface
// ---------------------------------------------------------------------------

// Decomposer is implemented by format-specific processors that know how to
// break raw input into a normalized IngestResult.
type Decomposer interface {
	// CanDecompose reports whether this decomposer can handle the request.
	CanDecompose(req *IngestRequest) bool

	// Decompose extracts structured content from the request.
	Decompose(ctx context.Context, req *IngestRequest) (*IngestResult, error)
}

// ---------------------------------------------------------------------------
// Inbox lifecycle status
// ---------------------------------------------------------------------------

// IngestStatus tracks where an item sits in the inbox lifecycle.
type IngestStatus string

const (
	StatusRaw        IngestStatus = "raw"
	StatusEnriched   IngestStatus = "enriched"
	StatusIntegrated IngestStatus = "integrated"
)

// ---------------------------------------------------------------------------
// Deduplication
// ---------------------------------------------------------------------------

// DedupChecker scans the inbox sector for existing CogDocs to prevent
// duplicate ingestion of the same source material.
type DedupChecker struct {
	inboxRoot string // absolute path to .cog/mem/semantic/inbox/
}

// NewDedupChecker creates a checker that scans the given workspace's inbox.
func NewDedupChecker(workspaceRoot string) *DedupChecker {
	return &DedupChecker{
		inboxRoot: filepath.Join(workspaceRoot, ".cog", "mem", "semantic", "inbox"),
	}
}

// IsDuplicate checks whether a CogDoc with the given source ID already
// exists in the inbox. It scans frontmatter of existing files for a
// matching source_id field. This is O(n) over inbox size — acceptable
// for the expected volume. A future optimization could use an index.
func (d *DedupChecker) IsDuplicate(sourceID string) bool {
	if sourceID == "" {
		return false
	}

	needle := "source_id: " + sourceID

	found := false
	_ = filepath.WalkDir(d.inboxRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil || found {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}

		f, ferr := os.Open(path)
		if ferr != nil {
			return nil // skip unreadable files
		}
		defer f.Close()

		// Read first 1KB — frontmatter lives at the top.
		buf := make([]byte, 1024)
		n, _ := io.ReadAtLeast(f, buf, 1)
		if n == 0 {
			return nil
		}
		head := string(buf[:n])

		// Only inspect content between YAML fences (---).
		if !strings.HasPrefix(head, "---") {
			return nil
		}
		end := strings.Index(head[3:], "---")
		if end == -1 {
			// Frontmatter not closed within 1KB — search what we have.
			end = len(head) - 3
		}
		frontmatter := head[3 : 3+end]

		if strings.Contains(frontmatter, needle) {
			found = true
		}
		return nil
	})

	return found
}

// ---------------------------------------------------------------------------
// Pipeline
// ---------------------------------------------------------------------------

// IngestPipeline orchestrates decomposition by delegating to the first
// registered Decomposer that can handle a given request.
type IngestPipeline struct {
	decomposers   []Decomposer
	workspaceRoot string
	dedup         *DedupChecker
}

// NewIngestPipeline creates a pipeline rooted at the given workspace directory.
func NewIngestPipeline(workspaceRoot string) *IngestPipeline {
	return &IngestPipeline{
		workspaceRoot: workspaceRoot,
		dedup:         NewDedupChecker(workspaceRoot),
	}
}

// CheckDuplicate returns true if a source ID already exists in the inbox.
func (p *IngestPipeline) CheckDuplicate(sourceID string) bool {
	if p.dedup == nil {
		return false
	}
	return p.dedup.IsDuplicate(sourceID)
}

// Register adds a Decomposer to the pipeline. Decomposers are tried in
// registration order; the first match wins.
func (p *IngestPipeline) Register(d Decomposer) {
	p.decomposers = append(p.decomposers, d)
}

// Ingest runs the ingestion pipeline for a single request. It iterates
// through registered decomposers, using the first one that reports it can
// handle the request. When no decomposer matches, a minimal result with
// ContentType=ContentUnknown is returned.
func (p *IngestPipeline) Ingest(ctx context.Context, req *IngestRequest) (*IngestResult, error) {
	for _, d := range p.decomposers {
		if d.CanDecompose(req) {
			slog.Debug("ingest: decomposer matched", "source", req.Source, "format", req.Format)
			return d.Decompose(ctx, req)
		}
	}

	slog.Warn("ingest: no decomposer matched, returning minimal result",
		"source", req.Source, "format", req.Format)

	return &IngestResult{
		Title:       req.Data,
		ContentType: ContentUnknown,
		Tags:        []string{},
		Source:      req.Source,
		SourceID:    req.Data,
	}, nil
}

// ---------------------------------------------------------------------------
// Ledger event emission
// ---------------------------------------------------------------------------

// EmitIngestEvent writes an ingestion event to the workspace ledger.
// This allows the observer and other agents to react to new material
// arriving in the inbox.
func EmitIngestEvent(cfg *Config, result *IngestResult, cogdocPath string) error {
	// Build the cog:// URI from the cogdoc path.
	cogdocURI := "cog:mem/" + cogdocPath

	event := map[string]any{
		"type":         IngestEventType,
		"source":       string(result.Source),
		"content_type": string(result.ContentType),
		"title":        result.Title,
		"url":          result.URL,
		"cogdoc_path":  cogdocPath,
		"cogdoc_uri":   cogdocURI,
		"source_id":    result.SourceID,
	}

	if err := EmitLedgerEvent(cfg, event); err != nil {
		slog.Error("ingest: failed to emit ledger event",
			"title", result.Title,
			"error", err)
		return err
	}

	slog.Info("ingest: ledger event emitted",
		"type", IngestEventType,
		"title", result.Title,
		"cogdoc_path", cogdocPath)

	return nil
}

func NormalizeIngestBlock(req *IngestRequest, result *IngestResult) *CogBlock {
	now := time.Now().UTC()
	raw, _ := json.Marshal(req)
	trust := TrustContext{Authenticated: true, TrustScore: 1.0, Scope: "local"}
	provenance := BlockProvenance{
		IngestedAt:   now,
		NormalizedBy: "ingest",
	}
	if req != nil {
		provenance.OriginChannel = string(req.Source)
		switch req.Source {
		case SourceFile:
			trust = TrustContext{Authenticated: true, TrustScore: 1.0, Scope: "local"}
		case SourceURL, SourceDiscord, SourceChatGPT, SourceClaude, SourceGemini:
			trust = TrustContext{Authenticated: true, TrustScore: 0.4, Scope: "network"}
		}
	}

	content := ""
	if result != nil {
		content = buildIngestContent(result)
	}

	return &CogBlock{
		ID:              uuid.NewString(),
		Timestamp:       now,
		SourceChannel:   provenance.OriginChannel,
		SourceTransport: "ingest",
		Kind:            BlockImport,
		RawPayload:      raw,
		Messages:        []ProviderMessage{{Role: "user", Content: content}},
		Provenance:      provenance,
		TrustContext:    trust,
	}
}

func ApplyMembraneDecision(defaultMemPath string, opts CogDocWriteOpts, decision IngestionResult) (string, CogDocWriteOpts, bool) {
	if opts.Extra == nil {
		opts.Extra = make(map[string]string)
	}

	switch decision.Decision {
	case Quarantine:
		opts.Extra["quarantine_reason"] = decision.QuarantineReason
		if decision.Reason != "" {
			opts.Content = "> QUARANTINED: " + decision.Reason + "\n\n" + opts.Content
		}
		return filepath.Join("quarantine", filepath.Base(defaultMemPath)), opts, true
	case Defer:
		opts.Extra["review_status"] = "deferred"
		if decision.Reason != "" {
			opts.Extra["review_reason"] = decision.Reason
			opts.Content = "> REVIEW REQUIRED: " + decision.Reason + "\n\n" + opts.Content
		}
		return filepath.Join("deferred", filepath.Base(defaultMemPath)), opts, true
	case Discard:
		return "", opts, false
	default:
		return defaultMemPath, opts, true
	}
}

type ingestionQueueState struct {
	Quarantined           int       `json:"quarantined"`
	Deferred              int       `json:"deferred"`
	LastQuarantineAt      time.Time `json:"last_quarantine_at,omitempty"`
	LastQuarantineRFC3339 string    `json:"last_quarantine,omitempty"`
}

func ReadIngestionQueueState(workspaceRoot string) ingestionQueueState {
	state := ingestionQueueState{}
	scan := func(root string, update func(os.FileInfo)) {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() {
				return nil
			}
			update(info)
			return nil
		})
	}

	scan(filepath.Join(workspaceRoot, ".cog", "mem", "quarantine"), func(info os.FileInfo) {
		state.Quarantined++
		if info.ModTime().After(state.LastQuarantineAt) {
			state.LastQuarantineAt = info.ModTime().UTC()
			state.LastQuarantineRFC3339 = state.LastQuarantineAt.Format(time.RFC3339)
		}
	})
	scan(filepath.Join(workspaceRoot, ".cog", "mem", "deferred"), func(os.FileInfo) {
		state.Deferred++
	})

	return state
}
