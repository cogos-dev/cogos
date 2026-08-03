// Package linkfeed implements Discord link-feed ingestion + enrichment.
//
// Layer 1: pullDiscordLinkFeed() — scheduled Discord pull with pagination,
//   deduplication, and CogDoc creation.
// Layer 2: enrichLink() — lightweight URL fetch + decomposition + cross-ref.
//
// Both are registered as agent tools (pull_link_feed, enrich_link) via
// RegisterLinkFeedTools, and the helpers (ScanInbox, LinkFeedLastPull,
// BuildInboxSummaryForAPI) are called directly from the observation
// pipeline for inbox awareness.
//
// Extracted from apps/cogos/agent_linkfeed.go as wave 1a of ADR-085.
// The file retains its original logic; only the package boundary and the
// Harness interface (to decouple from *main.AgentHarness) are new.
package linkfeed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
	"gopkg.in/yaml.v3"
)

// LinkFeedChannelEnvVar overrides the #link-feed Discord channel id.
//
// The channel id is deliberately NOT a compiled-in constant: a Discord
// snowflake identifies one operator's private channel, and this repo is
// public. It is read from the same untracked auth.yaml that already holds the
// bot token (the feature cannot run without that file anyway), or from this
// environment variable.
const LinkFeedChannelEnvVar = "COGOS_LINKFEED_CHANNEL_ID"

// LinkFeedCheckInterval is how often the agent should pull new links.
const LinkFeedCheckInterval = 2 * time.Hour

// InboxLinksRelPath is the memory-relative path to the inbox.
const InboxLinksRelPath = ".cog/mem/semantic/inbox/links"

// linkFeedStateRelPath is the state file for tracking last_message_id.
const linkFeedStateRelPath = ".cog/mem/semantic/research/link-feed-state.json"

// discordAuthRelPath is the Discord auth config.
const discordAuthRelPath = ".cog/config/discord/auth.yaml"

// --- Harness coupling ---

// Harness is the minimal surface linkfeed needs from the agent harness.
// The main package adapts its *AgentHarness to this interface. Using an
// interface (rather than importing the concrete type) keeps linkfeed a
// leaf package: main depends on linkfeed, not vice versa.
//
// RegisterTool takes flat arguments (not ToolDefinition/ToolFunc structs)
// so main's adapter can translate into its own types without linkfeed
// needing to agree on a shared struct layout.
type Harness interface {
	// RegisterTool registers a callable tool with the agent's tool loop.
	RegisterTool(name, description string, parameters json.RawMessage,
		fn func(ctx context.Context, args json.RawMessage) (json.RawMessage, error))
	// GenerateJSON sends a prompt to the model in JSON mode and returns
	// the raw JSON content.
	GenerateJSON(ctx context.Context, systemPrompt, userPrompt string) (string, error)
}

// toolFn is the signature for a registered tool function.
type toolFn func(ctx context.Context, args json.RawMessage) (json.RawMessage, error)

// --- Types ---

// LinkFeedItem represents a single link extracted from a Discord message.
type LinkFeedItem struct {
	URL       string `json:"url"`
	Title     string `json:"title"`
	File      string `json:"file"`
	MessageID string `json:"message_id"`
	Author    string `json:"author"`
}

// linkFeedState is the persisted state for the link feed puller.
type linkFeedState struct {
	LastMessageID string `json:"last_message_id"`
	LastUpdate    string `json:"last_update"`
	ChannelID     string `json:"channel_id"`
	GuildID       string `json:"guild_id"`
	TotalLinks    int    `json:"total_links"`
}

// discordAuth holds the bot token and channel id from auth.yaml.
type discordAuth struct {
	Token string `yaml:"token"`
	// ChannelID is the #link-feed channel snowflake. Lives here rather than
	// in source because it names one operator's private channel; auth.yaml is
	// untracked and already required for this feature.
	ChannelID string `yaml:"channel_id"`
}

// resolveChannelID returns the #link-feed channel id from auth.yaml, falling
// back to LinkFeedChannelEnvVar. Returns an error naming both places rather
// than silently pulling from the wrong channel.
func resolveChannelID(auth *discordAuth) (string, error) {
	if auth != nil && auth.ChannelID != "" {
		return auth.ChannelID, nil
	}
	if v := strings.TrimSpace(os.Getenv(LinkFeedChannelEnvVar)); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("link-feed channel id not configured: set channel_id in %s or $%s",
		discordAuthRelPath, LinkFeedChannelEnvVar)
}

// discordMessage is a minimal Discord message for JSON unmarshalling.
type discordMessage struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	Author    struct {
		Username string `json:"username"`
	} `json:"author"`
	Embeds []struct {
		URL         string `json:"url"`
		Title       string `json:"title"`
		Description string `json:"description"`
	} `json:"embeds"`
}

// --- Tool Registration ---

// RegisterLinkFeedTools adds link feed tools to the agent harness.
func RegisterLinkFeedTools(h Harness, workspaceRoot string) {
	h.RegisterTool(
		"pull_link_feed",
		"Pull new links from the Discord #link-feed channel. Reads config from disk, fetches new messages since last pull, extracts URLs, deduplicates, and writes raw CogDocs to the inbox.",
		json.RawMessage(`{
			"type": "object",
			"properties": {},
			"required": []
		}`),
		newPullLinkFeedFunc(workspaceRoot),
	)
	h.RegisterTool(
		"enrich_link",
		"Enrich a raw link CogDoc in the inbox. Fetches URL content, extracts metadata, runs Tier 0+1 decomposition, searches for cross-references, and updates the CogDoc status to enriched.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"filename": {
					"type": "string",
					"description": "Filename of the raw CogDoc in inbox/links/ to enrich"
				}
			},
			"required": ["filename"]
		}`),
		newEnrichLinkFunc(h, workspaceRoot),
	)
	h.RegisterTool(
		"list_inbox",
		"List raw (unenriched) inbox files. Returns filenames you can pass to enrich_link. Use this to find items to enrich.",
		json.RawMessage(`{
			"type": "object",
			"properties": {
				"limit": {"type": "integer", "description": "Max files to return (default 10)"}
			},
			"required": []
		}`),
		newListInboxFunc(workspaceRoot),
	)
}

// --- list_inbox tool ---

func newListInboxFunc(root string) toolFn {
	return func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		var p struct {
			Limit int `json:"limit"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			p.Limit = 10
		}
		if p.Limit <= 0 {
			p.Limit = 10
		}
		if p.Limit > 50 {
			p.Limit = 50
		}

		// Read inbox directory directly for the requested limit
		dir := filepath.Join(root, InboxLinksRelPath)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return json.Marshal(map[string]string{"error": "cannot read inbox: " + err.Error()})
		}

		var rawFiles []string
		var rawCount, enrichedCount int
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".cog.md") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			status := extractFMField(string(data), "status")
			switch status {
			case "enriched":
				enrichedCount++
			case "raw", "":
				rawCount++
				if len(rawFiles) < p.Limit {
					rawFiles = append(rawFiles, e.Name())
				}
			}
		}

		return json.Marshal(map[string]interface{}{
			"raw_count":      rawCount,
			"enriched_count": enrichedCount,
			"files":          rawFiles,
			"showing":        len(rawFiles),
		})
	}
}

// --- pull_link_feed tool ---

func newPullLinkFeedFunc(root string) toolFn {
	return func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
		items, err := pullDiscordLinkFeed(ctx, root)
		if err != nil {
			return json.Marshal(map[string]interface{}{
				"error": err.Error(),
			})
		}
		return json.Marshal(map[string]interface{}{
			"new_links": len(items),
			"items":     items,
		})
	}
}

// --- enrich_link tool ---

func newEnrichLinkFunc(h Harness, root string) toolFn {
	return func(ctx context.Context, args json.RawMessage) (json.RawMessage, error) {
		var p struct {
			Filename string `json:"filename"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return nil, fmt.Errorf("parse args: %w", err)
		}
		if p.Filename == "" {
			return json.Marshal(map[string]string{"error": "filename is required"})
		}
		result, err := enrichLink(ctx, h, root, p.Filename)
		if err != nil {
			return json.Marshal(map[string]interface{}{
				"error":    err.Error(),
				"filename": p.Filename,
			})
		}
		return json.Marshal(result)
	}
}

// --- Core: pullDiscordLinkFeed ---

// pullDiscordLinkFeed fetches new messages from Discord, extracts URLs,
// deduplicates, and writes raw CogDocs. Returns the list of new items.
func pullDiscordLinkFeed(ctx context.Context, root string) ([]LinkFeedItem, error) {
	// 1. Read auth
	auth, err := ReadDiscordAuth(root)
	if err != nil {
		return nil, fmt.Errorf("discord auth: %w", err)
	}
	channelID, err := resolveChannelID(auth)
	if err != nil {
		return nil, err
	}

	// 2. Read state
	state, err := readLinkFeedState(root)
	if err != nil {
		// Fresh start — no state file yet
		state = &linkFeedState{
			ChannelID: channelID,
		}
	}

	// 3. Build existing URL set for dedup
	existingURLs, err := buildExistingURLSet(root)
	if err != nil {
		log.Printf("[linkfeed] warning: could not read inbox for dedup: %v", err)
		existingURLs = make(map[string]bool)
	}

	// 4. Fetch messages with pagination
	var allMessages []discordMessage
	afterID := state.LastMessageID
	for {
		messages, err := fetchDiscordMessages(ctx, auth.Token, channelID, afterID)
		if err != nil {
			return nil, fmt.Errorf("discord API: %w", err)
		}
		if len(messages) == 0 {
			break
		}
		allMessages = append(allMessages, messages...)
		// Discord returns oldest first when using "after", so last item is newest
		afterID = messages[len(messages)-1].ID
		// If we got fewer than 100, no more pages
		if len(messages) < 100 {
			break
		}
	}

	if len(allMessages) == 0 {
		return nil, nil
	}

	// 5. Extract URLs and write CogDocs
	inboxDir := filepath.Join(root, InboxLinksRelPath)
	if err := os.MkdirAll(inboxDir, 0o755); err != nil {
		return nil, fmt.Errorf("create inbox dir: %w", err)
	}

	var items []LinkFeedItem
	var newestID string
	urlRegex := regexp.MustCompile(`https?://[^\s<>]+`)

	for _, msg := range allMessages {
		// Track newest message ID
		if newestID == "" || msg.ID > newestID {
			newestID = msg.ID
		}

		// Extract URLs: embeds first, then regex on content
		var urls []string
		seen := make(map[string]bool)

		for _, embed := range msg.Embeds {
			if embed.URL != "" && !seen[embed.URL] {
				urls = append(urls, embed.URL)
				seen[embed.URL] = true
			}
		}
		for _, u := range urlRegex.FindAllString(msg.Content, -1) {
			// Clean trailing punctuation
			u = strings.TrimRight(u, ".,;:!?)")
			if !seen[u] {
				urls = append(urls, u)
				seen[u] = true
			}
		}

		for _, rawURL := range urls {
			// Deduplicate against existing inbox
			if existingURLs[rawURL] {
				continue
			}
			existingURLs[rawURL] = true

			// Determine title from embed or domain
			title := urlDomain(rawURL)
			for _, embed := range msg.Embeds {
				if embed.URL == rawURL && embed.Title != "" {
					title = embed.Title
					break
				}
			}

			// Build filename
			ts := parseDiscordTimestamp(msg.Timestamp)
			slug := sanitizeFilename(title)
			if len(slug) > 60 {
				slug = slug[:60]
			}
			filename := fmt.Sprintf("discord-%s-%s.cog.md", ts.Format("2006-01-02"), slug)

			// Build CogDoc content
			cogdoc := buildRawLinkCogDoc(rawURL, title, msg.Author.Username, msg.ID, msg.Timestamp, ts)

			path := filepath.Join(inboxDir, filename)
			if err := os.WriteFile(path, []byte(cogdoc), 0o644); err != nil {
				log.Printf("[linkfeed] failed to write %s: %v", filename, err)
				continue
			}

			items = append(items, LinkFeedItem{
				URL:       rawURL,
				Title:     title,
				File:      filename,
				MessageID: msg.ID,
				Author:    msg.Author.Username,
			})
		}
	}

	// 6. Update state
	if newestID != "" {
		state.LastMessageID = newestID
		state.LastUpdate = time.Now().Format("2006-01-02")
		state.TotalLinks += len(items)
		if err := writeLinkFeedState(root, state); err != nil {
			log.Printf("[linkfeed] warning: failed to update state: %v", err)
		}
	}

	log.Printf("[linkfeed] pulled %d new links from %d messages", len(items), len(allMessages))
	return items, nil
}

// --- Core: enrichLink ---

// enrichResult is the structured output from enrichment.
type enrichResult struct {
	Filename    string   `json:"filename"`
	URL         string   `json:"url"`
	Status      string   `json:"status"`
	Title       string   `json:"title"`
	Summary     string   `json:"summary,omitempty"`
	ContentType string   `json:"content_type,omitempty"`
	KeyTerms    []string `json:"key_terms,omitempty"`
	Refs        []string `json:"refs,omitempty"`
	Connections int      `json:"connections"`
}

// enrichLink fetches, decomposes, and cross-references a raw inbox link.
func enrichLink(ctx context.Context, h Harness, root, filename string) (*enrichResult, error) {
	// 30-second timeout for the entire enrichment
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	path := filepath.Join(root, InboxLinksRelPath, filepath.Base(filename))
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read cogdoc: %w", err)
	}

	contentStr := string(content)

	// Extract URL from frontmatter
	linkURL := extractFMField(contentStr, "url")
	if linkURL == "" {
		return nil, fmt.Errorf("no url field in %s", filename)
	}

	result := &enrichResult{
		Filename: filename,
		URL:      linkURL,
		Status:   "enriched",
	}

	// Fetch URL content (lightweight — just meta tags)
	fetchedTitle, fetchedDesc := fetchURLMeta(ctx, linkURL)
	if fetchedTitle != "" {
		result.Title = fetchedTitle
	} else {
		result.Title = extractFMField(contentStr, "title")
	}

	// Build text for decomposition
	decompInput := fmt.Sprintf("Title: %s\nURL: %s\n", result.Title, linkURL)
	if fetchedDesc != "" {
		decompInput += "Description: " + fetchedDesc + "\n"
	}

	// Run Tier 0 + Tier 1 decomposition via the harness
	var summary string
	var keyTerms []string
	var contentType string

	decompCtx, decompCancel := context.WithTimeout(ctx, 15*time.Second)
	defer decompCancel()

	tier1Prompt := `Analyze this link and respond as JSON:
{"summary": "one sentence summary", "key_terms": ["term1", "term2"], "content_type": "article|paper|repo|video|tool|discussion"}

Content types: article (blog/news), paper (academic), repo (GitHub/code), video (YouTube/media), tool (software/service), discussion (forum/thread)`

	tier1JSON, err := h.GenerateJSON(decompCtx, tier1Prompt, decompInput)
	if err == nil {
		var t1 struct {
			Summary     string   `json:"summary"`
			KeyTerms    []string `json:"key_terms"`
			ContentType string   `json:"content_type"`
		}
		if json.Unmarshal([]byte(tier1JSON), &t1) == nil {
			summary = t1.Summary
			keyTerms = t1.KeyTerms
			contentType = t1.ContentType
		}
	}

	result.Summary = summary
	result.KeyTerms = keyTerms
	result.ContentType = contentType

	// Search workspace memory for cross-references
	var refs []string
	if len(keyTerms) > 0 {
		searchQuery := strings.Join(keyTerms, " ")
		searchOut, err := runCogCommand(ctx, root, "memory", "search", searchQuery)
		if err == nil {
			var searchResult struct {
				Output string `json:"output"`
			}
			if json.Unmarshal(searchOut, &searchResult) == nil {
				// Parse search results — each line is a path
				for _, line := range strings.Split(searchResult.Output, "\n") {
					line = strings.TrimSpace(line)
					if line != "" && !strings.Contains(line, "inbox/links/") {
						refs = append(refs, line)
						if len(refs) >= 3 {
							break
						}
					}
				}
			}
		}
	}
	result.Refs = refs
	result.Connections = len(refs)

	// Update the CogDoc
	updatedDoc := updateCogDocEnriched(contentStr, result)
	if err := os.WriteFile(path, []byte(updatedDoc), 0o644); err != nil {
		return nil, fmt.Errorf("write enriched cogdoc: %w", err)
	}

	return result, nil
}

// --- Inbox scanning ---

// InboxSummary holds counts for the inbox awareness observation.
type InboxSummary struct {
	RawCount      int      `json:"raw_count"`
	EnrichedCount int      `json:"enriched_count"`
	FailedCount   int      `json:"failed_count"`
	TotalCount    int      `json:"total_count"`
	NewestRaw     []string `json:"newest_raw,omitempty"`
}

// ScanInbox reads the inbox directory and returns a summary.
func ScanInbox(root string) *InboxSummary {
	dir := filepath.Join(root, InboxLinksRelPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return &InboxSummary{}
	}

	summary := &InboxSummary{}
	type fileInfo struct {
		name    string
		modTime time.Time
	}
	var rawFiles []fileInfo

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".cog.md") {
			continue
		}
		summary.TotalCount++

		// Quick frontmatter status check
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		status := extractFMField(string(data), "status")
		switch status {
		case "raw":
			summary.RawCount++
			info, _ := e.Info()
			mt := time.Time{}
			if info != nil {
				mt = info.ModTime()
			}
			rawFiles = append(rawFiles, fileInfo{name: e.Name(), modTime: mt})
		case "enriched":
			summary.EnrichedCount++
		case "fetch_failed":
			summary.FailedCount++
		default:
			// Treat unknown status as raw
			summary.RawCount++
		}
	}

	// Get newest 5 raw files
	// Sort by mod time descending (simple insertion sort for small N)
	for i := 1; i < len(rawFiles); i++ {
		for j := i; j > 0 && rawFiles[j].modTime.After(rawFiles[j-1].modTime); j-- {
			rawFiles[j], rawFiles[j-1] = rawFiles[j-1], rawFiles[j]
		}
	}
	for i := 0; i < len(rawFiles) && i < 5; i++ {
		summary.NewestRaw = append(summary.NewestRaw, rawFiles[i].name)
	}

	return summary
}

// LinkFeedLastPull returns how long ago the last pull happened.
func LinkFeedLastPull(root string) (time.Duration, error) {
	state, err := readLinkFeedState(root)
	if err != nil {
		return 0, err
	}
	if state.LastUpdate == "" {
		return 0, fmt.Errorf("no last_update in state")
	}
	t, err := time.Parse("2006-01-02", state.LastUpdate)
	if err != nil {
		return 0, err
	}
	return time.Since(t), nil
}

// --- Helpers ---

// ReadDiscordAuth reads the bot token from .cog/config/discord/auth.yaml.
// Exported so the observation pipeline can probe "configured?" status.
func ReadDiscordAuth(root string) (*discordAuth, error) {
	path := filepath.Join(root, discordAuthRelPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", discordAuthRelPath, err)
	}
	var auth discordAuth
	if err := yaml.Unmarshal(data, &auth); err != nil {
		return nil, fmt.Errorf("parse auth.yaml: %w", err)
	}
	if auth.Token == "" {
		return nil, fmt.Errorf("empty token in auth.yaml")
	}
	return &auth, nil
}

func readLinkFeedState(root string) (*linkFeedState, error) {
	path := filepath.Join(root, linkFeedStateRelPath)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state linkFeedState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func writeLinkFeedState(root string, state *linkFeedState) error {
	path := filepath.Join(root, linkFeedStateRelPath)
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func buildExistingURLSet(root string) (map[string]bool, error) {
	dir := filepath.Join(root, InboxLinksRelPath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	urls := make(map[string]bool, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".cog.md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		u := extractFMField(string(data), "url")
		if u != "" {
			urls[u] = true
		}
	}
	return urls, nil
}

func fetchDiscordMessages(ctx context.Context, token, channelID, afterID string) ([]discordMessage, error) {
	apiURL := fmt.Sprintf("https://discord.com/api/v10/channels/%s/messages?limit=100", channelID)
	if afterID != "" {
		apiURL += "&after=" + afterID
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bot "+token)
	req.Header.Set("User-Agent", "CogOS-LinkFeed/1.0")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("discord API %d: %s", resp.StatusCode, string(body))
	}

	var messages []discordMessage
	if err := json.Unmarshal(body, &messages); err != nil {
		return nil, fmt.Errorf("parse messages: %w", err)
	}

	return messages, nil
}

func urlDomain(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return u.Hostname()
}

func parseDiscordTimestamp(ts string) time.Time {
	// Discord timestamps: "2026-04-15T12:34:56.789000+00:00"
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		// Try alternate format
		t, err = time.Parse("2006-01-02T15:04:05.999999+00:00", ts)
		if err != nil {
			return time.Now()
		}
	}
	return t
}

func buildRawLinkCogDoc(rawURL, title, author, messageID, timestamp string, ts time.Time) string {
	domain := urlDomain(rawURL)
	now := time.Now().Format(time.RFC3339)
	sourceID := fmt.Sprintf("discord-%s", messageID)

	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("title: %q\n", title))
	sb.WriteString("type: link\n")
	sb.WriteString("source: discord\n")
	sb.WriteString(fmt.Sprintf("url: %q\n", rawURL))
	sb.WriteString(fmt.Sprintf("domain: %q\n", domain))
	sb.WriteString("status: raw\n")
	sb.WriteString(fmt.Sprintf("source_id: %q\n", sourceID))
	sb.WriteString(fmt.Sprintf("discord_author: %q\n", author))
	sb.WriteString(fmt.Sprintf("created: %q\n", timestamp))
	sb.WriteString(fmt.Sprintf("ingested: %q\n", now))
	sb.WriteString("memory_sector: semantic\n")
	sb.WriteString("tags: [link-feed, raw]\n")
	sb.WriteString("---\n\n")
	sb.WriteString(fmt.Sprintf("# %s\n\n", title))
	sb.WriteString(fmt.Sprintf("URL: %s\n", rawURL))
	sb.WriteString(fmt.Sprintf("Source: Discord #link-feed by %s\n", author))
	sb.WriteString(fmt.Sprintf("Posted: %s\n", ts.Format("2006-01-02 15:04 MST")))

	return sb.String()
}

// fetchURLMeta does a lightweight HTTP GET and extracts title + description
// from HTML meta tags. 10-second timeout, no full page download.
func fetchURLMeta(ctx context.Context, rawURL string) (title, description string) {
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("User-Agent", "CogOS-LinkFeed/1.0 (metadata only)")

	resp, err := client.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()

	// Read at most 64KB — enough for meta tags in the <head>
	limited := io.LimitReader(resp.Body, 64*1024)
	doc, err := html.Parse(limited)
	if err != nil {
		return "", ""
	}

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			if n.Data == "title" && n.FirstChild != nil && title == "" {
				title = n.FirstChild.Data
			}
			if n.Data == "meta" {
				var name, content string
				for _, a := range n.Attr {
					switch strings.ToLower(a.Key) {
					case "name", "property":
						name = strings.ToLower(a.Val)
					case "content":
						content = a.Val
					}
				}
				if (name == "description" || name == "og:description") && description == "" {
					description = content
				}
				if name == "og:title" && title == "" {
					title = content
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return title, description
}

// updateCogDocEnriched updates a raw CogDoc with enrichment data.
func updateCogDocEnriched(content string, r *enrichResult) string {
	// Update status
	content = strings.Replace(content, "status: raw", "status: enriched", 1)

	// Update title if we have a better one
	if r.Title != "" {
		oldTitle := extractFMField(content, "title")
		if oldTitle != "" && r.Title != oldTitle {
			content = strings.Replace(content, fmt.Sprintf("title: %q", oldTitle), fmt.Sprintf("title: %q", r.Title), 1)
		}
	}

	// Add content_type if not present
	if r.ContentType != "" && !strings.Contains(content, "content_type:") {
		content = strings.Replace(content, "status: enriched", fmt.Sprintf("content_type: %s\nstatus: enriched", r.ContentType), 1)
	}

	// Add key terms to tags
	if len(r.KeyTerms) > 0 {
		oldTags := "tags: [link-feed, raw]"
		terms := make([]string, 0, len(r.KeyTerms)+2)
		terms = append(terms, "link-feed", "enriched")
		terms = append(terms, r.KeyTerms...)
		newTags := fmt.Sprintf("tags: [%s]", strings.Join(terms, ", "))
		content = strings.Replace(content, oldTags, newTags, 1)
	}

	// Append enrichment section after the existing content
	var sb strings.Builder
	sb.WriteString(content)
	if r.Summary != "" {
		sb.WriteString(fmt.Sprintf("\n## Summary\n%s\n", r.Summary))
	}
	if len(r.Refs) > 0 {
		sb.WriteString("\n## Cross-References\n")
		for _, ref := range r.Refs {
			sb.WriteString(fmt.Sprintf("- %s\n", ref))
		}
	}

	return sb.String()
}

// --- Agent Inbox Summary for Status API ---

// AgentInboxSummary is the enrichment data for the status API.
type AgentInboxSummary struct {
	RawCount          int                    `json:"raw_count"`
	EnrichedCount     int                    `json:"enriched_count"`
	FailedCount       int                    `json:"failed_count"`
	TotalCount        int                    `json:"total_count"`
	LastPull          string                 `json:"last_pull,omitempty"`
	LastPullAgo       string                 `json:"last_pull_ago,omitempty"`
	NextPullIn        string                 `json:"next_pull_in,omitempty"`
	RecentEnrichments []RecentEnrichmentItem `json:"recent_enrichments,omitempty"`
}

// RecentEnrichmentItem is a single recent enrichment for the dashboard.
type RecentEnrichmentItem struct {
	Title       string `json:"title"`
	Connections int    `json:"connections"`
	Ago         string `json:"ago"`
}

// BuildInboxSummaryForAPI constructs the inbox summary for the status endpoint.
func BuildInboxSummaryForAPI(root string) *AgentInboxSummary {
	inbox := ScanInbox(root)
	summary := &AgentInboxSummary{
		RawCount:      inbox.RawCount,
		EnrichedCount: inbox.EnrichedCount,
		FailedCount:   inbox.FailedCount,
		TotalCount:    inbox.TotalCount,
	}

	// Last pull timing
	ago, err := LinkFeedLastPull(root)
	if err == nil {
		summary.LastPullAgo = formatAgo(ago)
		remaining := LinkFeedCheckInterval - ago
		if remaining > 0 {
			summary.NextPullIn = formatAgo(remaining)
		} else {
			summary.NextPullIn = "overdue"
		}
	} else {
		summary.LastPullAgo = "never"
		summary.NextPullIn = "overdue"
	}

	state, err := readLinkFeedState(root)
	if err == nil {
		summary.LastPull = state.LastUpdate
	}

	return summary
}

// --- Private helpers duplicated from main (kept local to break cycle) ---

// extractFMField pulls a simple field value from YAML frontmatter.
// Duplicated from main's agent_tools_propose.go to keep linkfeed a leaf
// package. The original stays in main for its many other callers.
func extractFMField(content, field string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, field+":") {
			val := strings.TrimSpace(strings.TrimPrefix(line, field+":"))
			val = strings.Trim(val, `"`)
			return val
		}
	}
	return ""
}

// sanitizeFilename makes a string safe for use as a filename.
// Duplicated from main's agent_tools_propose.go for the same reason as
// extractFMField.
func sanitizeFilename(s string) string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		if r == ' ' {
			return '-'
		}
		return -1
	}, s)
	if len(s) > 50 {
		s = s[:50]
	}
	return s
}

// runCogCommand executes ./scripts/cog with the given arguments and returns
// the output as JSON. Duplicated from main's agent_tools.go.
func runCogCommand(ctx context.Context, root string, args ...string) (json.RawMessage, error) {
	cogScript := filepath.Join(root, "scripts", "cog")
	cmd := exec.CommandContext(ctx, cogScript, args...)
	cmd.Dir = root

	out, err := cmd.CombinedOutput()
	if err != nil {
		return json.Marshal(map[string]string{
			"error":  err.Error(),
			"output": string(out),
		})
	}
	return json.Marshal(map[string]string{"output": string(out)})
}

// formatAgo produces a human-readable duration like "5m" or "2h".
// Duplicated from main's agent_decompose.go.
func formatAgo(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}
