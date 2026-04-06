//go:build mcpserver

// ingest_url.go — URL decomposer for the ingestion pipeline.
//
// Fetches a URL, extracts structured metadata from HTML (title, meta tags,
// Open Graph, Twitter cards), classifies content by domain, and returns a
// normalized IngestResult.
package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// maxBodyRead limits how much of the response body we consume.
const maxBodyRead = 256 * 1024 // 256 KB

// ---------------------------------------------------------------------------
// URLDecomposer
// ---------------------------------------------------------------------------

// URLDecomposer implements Decomposer for URL inputs. It fetches the page,
// parses HTML metadata, and classifies the content by domain heuristics.
type URLDecomposer struct {
	client        *http.Client
	workspaceRoot string
}

// NewURLDecomposer creates a URLDecomposer with a 10-second HTTP timeout.
// workspaceRoot is the absolute path to the workspace directory (used for
// downloading artefacts like PDFs). Pass "" if downloading is not needed.
func NewURLDecomposer(workspaceRoot string) *URLDecomposer {
	return &URLDecomposer{
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
		workspaceRoot: workspaceRoot,
	}
}

// CanDecompose reports true when the request format is FormatURL or the data
// looks like a URL (starts with http:// or https://).
func (d *URLDecomposer) CanDecompose(req *IngestRequest) bool {
	if req.Format == FormatURL {
		return true
	}
	trimmed := strings.TrimSpace(req.Data)
	return strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://")
}

// Decompose fetches the URL, extracts HTML metadata, classifies the content,
// and returns a normalized IngestResult. HTTP and parse errors are handled
// gracefully — the result will contain whatever metadata can be derived from
// the URL itself.
func (d *URLDecomposer) Decompose(ctx context.Context, req *IngestRequest) (*IngestResult, error) {
	rawURL := strings.TrimSpace(req.Data)

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("ingest_url: invalid URL %q: %w", rawURL, err)
	}

	domain := strings.ToLower(parsed.Hostname())
	contentType := classifyDomain(domain)
	tags := domainTags(domain, contentType)

	result := &IngestResult{
		Title:       rawURL,
		URL:         rawURL,
		Domain:      domain,
		ContentType: contentType,
		Tags:        tags,
		Source:      req.Source,
		SourceID:    rawURL,
		Fields:      make(map[string]string),
	}

	// Fetch the page.
	body, fetchErr := d.fetch(ctx, rawURL)
	if fetchErr != nil {
		// Return what we have from the URL alone.
		return result, nil
	}

	// Extract metadata from the HTML body.
	meta := extractHTMLMeta(body)

	// Populate result from extracted metadata, preferring OG > Twitter > plain.
	if t := firstNonEmpty(meta["og:title"], meta["twitter:title"], meta["title"]); t != "" {
		result.Title = t
	}
	if desc := firstNonEmpty(meta["og:description"], meta["twitter:description"], meta["description"]); desc != "" {
		result.Summary = truncate(desc, 500)
	}

	// Stash interesting fields.
	for _, key := range []string{
		"og:image", "og:type", "og:site_name", "author",
		"twitter:title", "twitter:description",
	} {
		if v := meta[key]; v != "" {
			result.Fields[key] = v
		}
	}

	// Domain-specific enrichment.
	switch result.ContentType {
	case ContentRepo:
		enrichGitHub(result, meta, parsed.Path)
	case ContentPaper:
		d.enrichArxiv(ctx, result, meta, parsed.Path)
	case ContentVideo:
		enrichYouTube(result, meta, rawURL)
	default:
		enrichArticle(result, meta)
	}

	return result, nil
}

// ---------------------------------------------------------------------------
// HTTP fetch
// ---------------------------------------------------------------------------

// fetch retrieves the URL body, reading at most maxBodyRead bytes.
func (d *URLDecomposer) fetch(ctx context.Context, rawURL string) ([]byte, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("User-Agent", "CogOS/3 ingestion-pipeline")
	httpReq.Header.Set("Accept", "text/html, application/xhtml+xml, */*;q=0.8")

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// Limit read size.
	lr := io.LimitReader(resp.Body, maxBodyRead)
	return io.ReadAll(lr)
}

// ---------------------------------------------------------------------------
// HTML metadata extraction (using x/net/html tokenizer)
// ---------------------------------------------------------------------------

// extractHTMLMeta parses HTML and extracts title and meta tag values.
// It returns a map keyed by tag name/property (e.g. "title", "og:title",
// "description", "author", "twitter:description").
func extractHTMLMeta(body []byte) map[string]string {
	meta := make(map[string]string)

	tokenizer := html.NewTokenizer(bytes.NewReader(body))
	var inTitle bool

	for {
		tt := tokenizer.Next()
		switch tt {
		case html.ErrorToken:
			return meta

		case html.StartTagToken, html.SelfClosingTagToken:
			tn, hasAttr := tokenizer.TagName()
			tagName := string(tn)

			switch tagName {
			case "title":
				inTitle = true

			case "meta":
				if hasAttr {
					parseMeta(tokenizer, meta)
				}

			case "body":
				// Stop parsing once we leave <head>.
				return meta
			}

		case html.TextToken:
			if inTitle {
				text := strings.TrimSpace(string(tokenizer.Text()))
				if text != "" {
					meta["title"] = text
				}
			}

		case html.EndTagToken:
			tn, _ := tokenizer.TagName()
			if string(tn) == "title" {
				inTitle = false
			}
			if string(tn) == "head" {
				return meta
			}
		}
	}
}

// parseMeta extracts name/property and content from a <meta> tag's attributes.
func parseMeta(z *html.Tokenizer, meta map[string]string) {
	var name, property, content string

	for {
		key, val, more := z.TagAttr()
		k := string(key)
		v := string(val)

		switch k {
		case "name":
			name = strings.ToLower(v)
		case "property":
			property = strings.ToLower(v)
		case "content":
			content = v
		}

		if !more {
			break
		}
	}

	if content == "" {
		return
	}

	// Store by property (OG/Twitter) or name (description, author).
	if property != "" {
		meta[property] = content
	}
	if name != "" {
		meta[name] = content
	}
}

// ---------------------------------------------------------------------------
// Domain classification & tagging
// ---------------------------------------------------------------------------

// classifyDomain maps a hostname to a ContentType using simple heuristics.
func classifyDomain(domain string) ContentType {
	// Strip "www." prefix for matching.
	d := strings.TrimPrefix(domain, "www.")

	switch {
	case d == "github.com" || strings.HasSuffix(d, ".github.com"):
		return ContentRepo
	case d == "arxiv.org" || strings.HasSuffix(d, ".arxiv.org"):
		return ContentPaper
	case d == "scholar.google.com":
		return ContentPaper
	case d == "youtube.com" || strings.HasSuffix(d, ".youtube.com") || d == "youtu.be":
		return ContentVideo
	case d == "reddit.com" || strings.HasSuffix(d, ".reddit.com"):
		return ContentDiscussion
	case d == "news.ycombinator.com":
		return ContentDiscussion
	default:
		return ContentArticle
	}
}

// domainTags generates tags from the domain and its classified content type.
func domainTags(domain string, contentType ContentType) []string {
	d := strings.TrimPrefix(domain, "www.")
	tags := []string{}

	// Add domain-based short name.
	parts := strings.Split(d, ".")
	if len(parts) >= 2 {
		tags = append(tags, parts[len(parts)-2]) // e.g. "github", "arxiv"
	}

	// Add content type as a tag.
	tags = append(tags, string(contentType))

	return tags
}

// ---------------------------------------------------------------------------
// Domain-specific enrichment
// ---------------------------------------------------------------------------

// enrichGitHub extracts owner, repo, and reference type from GitHub URLs.
func enrichGitHub(result *IngestResult, meta map[string]string, urlPath string) {
	// Split path into segments, filtering empty strings from leading/trailing slashes.
	segments := splitPath(urlPath)

	if len(segments) >= 2 {
		owner := segments[0]
		repo := segments[1]
		result.Fields["owner"] = owner
		result.Fields["repo"] = repo
		addTag(result, "github", "opensource", repo)
	}

	// Try to extract language from og:description (GitHub often includes it).
	if desc := meta["og:description"]; desc != "" {
		// GitHub og:description often starts with "repo - description" or
		// contains language info. We stash the raw description as a field.
		result.Fields["gh_description"] = desc
	}

	// Classify sub-paths.
	for i, seg := range segments {
		switch seg {
		case "blob", "tree":
			result.Fields["ref_type"] = seg // file or directory reference
		case "issues", "pull":
			result.ContentType = ContentDiscussion
			if i+1 < len(segments) {
				result.Fields["number"] = segments[i+1]
			}
		}
	}
}

// ---------------------------------------------------------------------------
// arXiv Atom API types
// ---------------------------------------------------------------------------

type arxivFeed struct {
	XMLName xml.Name     `xml:"feed"`
	Entries []arxivEntry `xml:"entry"`
}

type arxivEntry struct {
	Title   string        `xml:"title"`
	Summary string        `xml:"summary"`
	Authors []arxivAuthor `xml:"author"`
}

type arxivAuthor struct {
	Name string `xml:"name"`
}

// maxPDFRead limits how much of a PDF download we consume (50 MB).
const maxPDFRead = 50 * 1024 * 1024

// enrichArxiv extracts paper ID and academic metadata from arXiv URLs.
// It fetches structured metadata from the arXiv Atom API and downloads
// the PDF to the workspace inbox.
func (d *URLDecomposer) enrichArxiv(ctx context.Context, result *IngestResult, meta map[string]string, urlPath string) {
	result.ContentType = ContentPaper

	segments := splitPath(urlPath)

	// Extract paper ID: /abs/2301.12345 or /pdf/2301.12345
	for i, seg := range segments {
		if (seg == "abs" || seg == "pdf") && i+1 < len(segments) {
			paperID := strings.TrimSuffix(segments[i+1], ".pdf")
			result.Fields["arxiv_id"] = paperID
			break
		}
	}

	// Extract authors from citation_author meta tags if available.
	if author := meta["citation_author"]; author != "" {
		result.Fields["authors"] = author
	}

	// Use description as abstract.
	if abs := firstNonEmpty(meta["og:description"], meta["description"]); abs != "" {
		result.Summary = truncate(abs, 500)
	}

	addTag(result, "arxiv", "paper", "research")

	// ── Atom API enrichment ─────────────────────────────────────────────
	paperID := result.Fields["arxiv_id"]
	if paperID == "" {
		return
	}

	d.fetchArxivMetadata(ctx, result, paperID)
	d.downloadArxivPDF(ctx, result, paperID)
}

// fetchArxivMetadata queries the arXiv Atom API for structured metadata
// (title, authors, abstract) and updates the result. Errors are logged
// but do not prevent the rest of enrichment from proceeding.
func (d *URLDecomposer) fetchArxivMetadata(ctx context.Context, result *IngestResult, paperID string) {
	apiURL := "https://export.arxiv.org/api/query?id_list=" + paperID

	apiCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(apiCtx, http.MethodGet, apiURL, nil)
	if err != nil {
		slog.Warn("arxiv: failed to build API request", "paper_id", paperID, "error", err)
		return
	}
	req.Header.Set("User-Agent", "CogOS/3 ingestion-pipeline")

	resp, err := d.client.Do(req)
	if err != nil {
		slog.Warn("arxiv: API request failed", "paper_id", paperID, "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		slog.Warn("arxiv: API returned error", "paper_id", paperID, "status", resp.StatusCode)
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	if err != nil {
		slog.Warn("arxiv: failed to read API response", "paper_id", paperID, "error", err)
		return
	}

	var feed arxivFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		slog.Warn("arxiv: failed to parse Atom feed", "paper_id", paperID, "error", err)
		return
	}

	if len(feed.Entries) == 0 {
		slog.Warn("arxiv: no entries in API response", "paper_id", paperID)
		return
	}

	entry := feed.Entries[0]

	// Override title with the API title (cleaner than HTML scrape).
	if title := strings.TrimSpace(entry.Title); title != "" {
		result.Title = title
	}

	// Use the Atom summary (abstract) — it's more complete than og:description.
	if summary := strings.TrimSpace(entry.Summary); summary != "" {
		result.Summary = summary
	}

	// Build comma-separated author list.
	var names []string
	for _, a := range entry.Authors {
		if name := strings.TrimSpace(a.Name); name != "" {
			names = append(names, name)
		}
	}
	if len(names) > 0 {
		result.Fields["authors"] = strings.Join(names, ", ")
	}

	result.Fields["arxiv_id"] = paperID

	slog.Info("arxiv: metadata fetched", "paper_id", paperID, "title", result.Title, "authors", len(names))
}

// downloadArxivPDF downloads the paper PDF and stores it in the blob store.
// A blob pointer CogDoc is NOT created here — that's done by the caller
// when writing the main CogDoc. The blob hash and size are recorded in
// result.Fields so the CogDoc can reference them.
func (d *URLDecomposer) downloadArxivPDF(ctx context.Context, result *IngestResult, paperID string) {
	if d.workspaceRoot == "" {
		return
	}

	bs := NewBlobStore(d.workspaceRoot)

	// Check if already stored by looking for a known blob hash in a previous run.
	// We use the paper ID as a secondary check via the manifest.
	// For now, just attempt the download — Store() is idempotent.

	pdfURL := "https://arxiv.org/pdf/" + paperID + ".pdf"

	dlCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, pdfURL, nil)
	if err != nil {
		slog.Warn("arxiv: failed to build PDF request", "paper_id", paperID, "error", err)
		return
	}
	req.Header.Set("User-Agent", "CogOS/3 ingestion-pipeline")

	resp, err := d.client.Do(req)
	if err != nil {
		slog.Error("arxiv: PDF download failed", "paper_id", paperID, "error", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		slog.Error("arxiv: PDF download returned error", "paper_id", paperID, "status", resp.StatusCode)
		return
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPDFRead))
	if err != nil {
		slog.Error("arxiv: failed to read PDF body", "paper_id", paperID, "error", err)
		return
	}

	// Store in blob store instead of writing to git-tracked directory.
	if err := bs.Init(); err != nil {
		slog.Error("arxiv: blob store init failed", "error", err)
		return
	}

	ref := fmt.Sprintf("cog://mem/semantic/inbox/papers/%s.pdf", paperID)
	hash, err := bs.Store(data, "application/pdf", ref)
	if err != nil {
		slog.Error("arxiv: blob store failed", "paper_id", paperID, "error", err)
		return
	}

	// Record blob metadata in result fields so the CogDoc can reference it.
	result.Fields["blob_hash"] = hash
	result.Fields["blob_size"] = fmt.Sprintf("%d", len(data))
	result.Fields["blob_content_type"] = "application/pdf"
	result.Fields["reasoning_queue"] = "true"

	slog.Info("arxiv: PDF stored in blob store",
		"paper_id", paperID,
		"hash", hash[:12],
		"bytes", len(data),
	)
}

// enrichYouTube extracts video ID and channel info from YouTube URLs.
func enrichYouTube(result *IngestResult, meta map[string]string, rawURL string) {
	result.ContentType = ContentVideo

	videoID := extractYouTubeID(rawURL)
	if videoID != "" {
		result.Fields["video_id"] = videoID
	}

	if channel := firstNonEmpty(meta["og:site_name"], meta["author"]); channel != "" {
		result.Fields["channel"] = channel
	}

	if desc := firstNonEmpty(meta["og:description"], meta["description"]); desc != "" {
		result.Summary = truncate(desc, 500)
	}

	addTag(result, "youtube", "video")
}

// enrichArticle adds generic metadata for blog posts and articles.
func enrichArticle(result *IngestResult, meta map[string]string) {
	if author := meta["author"]; author != "" {
		result.Fields["author"] = author
	}

	if site := meta["og:site_name"]; site != "" {
		result.Fields["site"] = site
		addTag(result, strings.ToLower(site))
	}
}

// extractYouTubeID parses a video ID from various YouTube URL formats.
func extractYouTubeID(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}

	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")

	// youtu.be/VIDEO_ID
	if host == "youtu.be" {
		id := strings.TrimPrefix(u.Path, "/")
		// Strip anything after a slash (shouldn't exist, but be safe).
		if idx := strings.Index(id, "/"); idx != -1 {
			id = id[:idx]
		}
		return id
	}

	// youtube.com/watch?v=VIDEO_ID
	if v := u.Query().Get("v"); v != "" {
		return v
	}

	// youtube.com/embed/VIDEO_ID or youtube.com/v/VIDEO_ID
	segments := splitPath(u.Path)
	for i, seg := range segments {
		if (seg == "embed" || seg == "v") && i+1 < len(segments) {
			return segments[i+1]
		}
	}

	return ""
}

// splitPath splits a URL path into non-empty segments.
func splitPath(path string) []string {
	var segments []string
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			segments = append(segments, s)
		}
	}
	return segments
}

// addTag appends tags to result, skipping duplicates.
func addTag(result *IngestResult, tags ...string) {
	existing := make(map[string]bool, len(result.Tags))
	for _, t := range result.Tags {
		existing[t] = true
	}
	for _, t := range tags {
		if !existing[t] {
			result.Tags = append(result.Tags, t)
			existing[t] = true
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// firstNonEmpty returns the first non-empty string from the arguments.
func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// truncate is defined in debug.go — reused here for summary trimming.
