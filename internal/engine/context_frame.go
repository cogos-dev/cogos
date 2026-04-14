// context_frame.go — structured output type for the foveated context rendering pipeline
//
// ContextFrame is the intermediate representation between context assembly and
// rendering. Each block is named, tiered by priority, annotated with a stability
// hint (for KV cache optimization), and carries its rendered content.
//
// The rendering pipeline (serve_foveated.go) can compose, prioritize, and
// budget-fit blocks before emitting the final HTML comment block stream.
package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// ContextFrame is the structured output of the foveated context rendering pipeline.
// Each block has a name, tier (priority), stability (KV cache hint), and content.
type ContextFrame struct {
	Blocks     []ContextBlock `json:"blocks"`
	Budget     int            `json:"budget"`
	UsedTokens int            `json:"used_tokens"`
	Anchor     string         `json:"anchor,omitempty"`
	Goal       string         `json:"goal,omitempty"`
}

// ContextBlock is a single named section of the context frame.
type ContextBlock struct {
	Name      string `json:"name"`      // e.g. "nucleus", "project", "knowledge", "node", "field", "events"
	Tier      int    `json:"tier"`      // 0=fixed, 1=priority, 2=flexible, 3=expendable
	Stability int    `json:"stability"` // 0-100: higher = less likely to change between turns (KV cache hint)
	Content   string `json:"content"`   // Rendered markdown/text
	Tokens    int    `json:"tokens"`    // Estimated token count
	Hash      string `json:"hash"`      // Content hash for change detection
}

// BlockName constants identify the well-known context blocks.
const (
	BlockNucleus   = "nucleus"   // Identity card
	BlockProject   = "project"   // CLAUDE.md content
	BlockKnowledge = "knowledge" // Foveated CogDocs
	BlockNode      = "node"      // Sibling service health
	BlockField     = "field"     // Attentional field top-N
	BlockEvents    = "events"    // Recent ledger events
	BlockFocus     = "focus"     // Current anchor/intent
)

// DefaultTiers maps block names to their default tier.
var DefaultTiers = map[string]int{
	BlockNucleus:   0, // Always present
	BlockProject:   0, // Always present (CLAUDE.md)
	BlockKnowledge: 1, // Priority — foveated docs
	BlockNode:      2, // Flexible — node health
	BlockField:     2, // Flexible — attentional field
	BlockEvents:    2, // Flexible — recent events
	BlockFocus:     2, // Flexible — anchor/intent
}

// DefaultStability maps block names to stability hints (0-100).
var DefaultStability = map[string]int{
	BlockNucleus:   95, // Almost never changes within a session
	BlockProject:   90, // CLAUDE.md rarely changes
	BlockKnowledge: 30, // Changes every turn based on query
	BlockNode:      70, // Changes on 60s heartbeat
	BlockField:     40, // Changes as salience shifts
	BlockEvents:    20, // Changes every turn
	BlockFocus:     10, // Changes every turn
}

// NewBlock creates a ContextBlock with defaults from the tier/stability maps.
func NewBlock(name, content string) ContextBlock {
	tokens := estimateBlockTokens(content)
	return ContextBlock{
		Name:      name,
		Tier:      DefaultTiers[name],
		Stability: DefaultStability[name],
		Content:   content,
		Tokens:    tokens,
		Hash:      contentHash(content),
	}
}

// estimateBlockTokens uses the fast chars/4 heuristic.
// This mirrors estTokens from context_assembly.go but is kept as a local
// function to avoid coupling the ContextFrame API to the assembly internals.
func estimateBlockTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	return len(s) / 4
}

// contentHash returns a short SHA256 prefix for change detection.
func contentHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:4])
}

// Render serializes the frame as HTML comment blocks for hook injection.
// Blocks are sorted by stability descending (most stable first — KV cache friendly).
// Each block is emitted as:
//
//	<!-- block:{tier}:{name} hash:{hash} tokens:{tokens} stability:{stability} -->
//	{content}
//	---
func (f *ContextFrame) Render() string {
	if len(f.Blocks) == 0 {
		return ""
	}

	// Sort by stability descending (most stable first — KV cache friendly).
	sorted := make([]ContextBlock, len(f.Blocks))
	copy(sorted, f.Blocks)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Stability > sorted[j].Stability
	})

	var sb strings.Builder
	for i, b := range sorted {
		fmt.Fprintf(&sb, "<!-- block:%d:%s hash:%s tokens:%d stability:%d -->\n",
			b.Tier, b.Name, b.Hash, b.Tokens, b.Stability)
		sb.WriteString(b.Content)
		sb.WriteString("\n---\n")
		if i < len(sorted)-1 {
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// FitBudget evicts lowest-tier blocks until total tokens <= budget.
// Tier 0 blocks are never removed. Among blocks of equal tier, the least
// stable block is evicted first.
func (f *ContextFrame) FitBudget(budget int) {
	if budget <= 0 {
		return
	}

	// Compute current total.
	total := 0
	for _, b := range f.Blocks {
		total += b.Tokens
	}
	if total <= budget {
		f.Budget = budget
		f.UsedTokens = total
		return
	}

	// Sort by tier descending (most expendable first), then stability ascending
	// (least stable first within same tier) for eviction ordering.
	type indexed struct {
		idx   int
		block ContextBlock
	}
	items := make([]indexed, len(f.Blocks))
	for i, b := range f.Blocks {
		items[i] = indexed{idx: i, block: b}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].block.Tier != items[j].block.Tier {
			return items[i].block.Tier > items[j].block.Tier
		}
		return items[i].block.Stability < items[j].block.Stability
	})

	// Evict from the front (highest tier / least stable) until within budget.
	evict := make(map[int]bool)
	for _, item := range items {
		if total <= budget {
			break
		}
		// Never remove tier 0 blocks.
		if item.block.Tier == 0 {
			continue
		}
		evict[item.idx] = true
		total -= item.block.Tokens
	}

	// Rebuild blocks list preserving original order.
	kept := make([]ContextBlock, 0, len(f.Blocks)-len(evict))
	for i, b := range f.Blocks {
		if !evict[i] {
			kept = append(kept, b)
		}
	}
	f.Blocks = kept
	f.Budget = budget
	f.UsedTokens = total
}
