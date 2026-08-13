// context_blocks_peers_test.go — unit tests for buildPeersBlock.
//
// Test strategy mirrors context_blocks_health_test.go:
//   - Unit tests for the builder and render helpers using stub deps.
//   - One end-to-end integration test that hits POST /v1/context/foveated
//     and confirms BlockPeers appears in the response and the rendered context.
package engine

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ─── stub deps ───────────────────────────────────────────────────────────────

// noopBusReader satisfies peerAwarenessBusReader and returns empty events.
type noopBusReader struct{}

func (noopBusReader) ReadEvents(string) ([]BusBlock, error) { return nil, nil }

// singleEventBusReader returns a single tailer.block event for one bus ID.
type singleEventBusReader struct {
	busID string
	event BusBlock
}

func (r singleEventBusReader) ReadEvents(busID string) ([]BusBlock, error) {
	if busID == r.busID {
		return []BusBlock{r.event}, nil
	}
	return nil, nil
}

// noopAttnReader satisfies peerAwarenessAttentionReader.
type noopAttnReader struct{}

func (noopAttnReader) RecentSignals(int) []PeerAwarenessAttentionSignal { return nil }

// noopHandoffReader satisfies peerAwarenessHandoffReader.
type noopHandoffReader struct{}

func (noopHandoffReader) Snapshot() []*HandoffState { return nil }

// noopEmitter satisfies peerAwarenessRenderEmitter.
type noopEmitter struct{}

func (noopEmitter) AppendEvent(string, string, string, map[string]interface{}) (*BusBlock, error) {
	return nil, nil
}

// emptyPeerDeps returns a fully wired but empty deps bundle.
func emptyPeerDeps() peerAwarenessDeps {
	return peerAwarenessDeps{
		bus:      noopBusReader{},
		attn:     noopAttnReader{},
		handoffs: noopHandoffReader{},
		renderer: noopEmitter{},
	}
}

// ─── buildPeersBlock unit tests ──────────────────────────────────────────────

// TestBuildPeersBlock_EmptySidReturnsNil confirms that a missing session ID
// yields nil — the caller should omit the block entirely.
func TestBuildPeersBlock_EmptySidReturnsNil(t *testing.T) {
	blk := buildPeersBlock("", emptyPeerDeps(), DefaultPeerBlockBudget)
	if blk != nil {
		t.Fatalf("expected nil for empty sid, got %+v", blk)
	}
}

// TestBuildPeersBlock_InvalidSidReturnsNil confirms ValidateSid gate.
func TestBuildPeersBlock_InvalidSidReturnsNil(t *testing.T) {
	blk := buildPeersBlock("has spaces", emptyPeerDeps(), DefaultPeerBlockBudget)
	if blk != nil {
		t.Fatalf("expected nil for invalid sid, got %+v", blk)
	}
}

// TestBuildPeersBlock_NoPeersCollapsesToOneLine confirms the quiet-workspace
// rendering: a valid sid with no data returns a block whose content is the
// header plus a single "No active peers." line.
func TestBuildPeersBlock_NoPeersCollapsesToOneLine(t *testing.T) {
	sid := "test-session-aa"
	blk := buildPeersBlock(sid, emptyPeerDeps(), DefaultPeerBlockBudget)
	if blk == nil {
		t.Fatal("expected non-nil block even with no peers")
	}
	if blk.Name != BlockPeers {
		t.Errorf("block name: got %q want %q", blk.Name, BlockPeers)
	}
	if !strings.Contains(blk.Content, "Peer Awareness") {
		t.Errorf("block should have section header, got:\n%s", blk.Content)
	}
	if !strings.Contains(blk.Content, "No active peers.") {
		t.Errorf("empty packet should render 'No active peers.', got:\n%s", blk.Content)
	}
}

// TestBuildPeersBlock_DefaultTokenBudgetApplied confirms the block token
// estimate stays within DefaultPeerBlockBudget for an empty packet.
func TestBuildPeersBlock_DefaultTokenBudgetApplied(t *testing.T) {
	sid := "budget-test-bb"
	blk := buildPeersBlock(sid, emptyPeerDeps(), DefaultPeerBlockBudget)
	if blk == nil {
		t.Fatal("expected non-nil block")
	}
	// The empty block should be well under the budget.
	if blk.Tokens > DefaultPeerBlockBudget {
		t.Errorf("block.Tokens=%d exceeds DefaultPeerBlockBudget=%d; content:\n%s",
			blk.Tokens, DefaultPeerBlockBudget, blk.Content)
	}
}

// TestBuildPeersBlock_ZeroBudgetUsesDefault confirms that passing 0 for
// budgetTokens falls back to DefaultPeerBlockBudget rather than producing a
// nil or panic.
func TestBuildPeersBlock_ZeroBudgetUsesDefault(t *testing.T) {
	sid := "zero-budget-cc"
	blk := buildPeersBlock(sid, emptyPeerDeps(), 0)
	if blk == nil {
		t.Fatal("expected non-nil block for zero budget (should use default)")
	}
}

// TestBuildPeersBlock_WithActivityEventRendersPacket seeds the bus reader
// with a tailer.block event on the session's activity channel and confirms
// the rendered packet appears in the block content instead of the
// "No active peers." fallback.
func TestBuildPeersBlock_WithActivityEventRendersPacket(t *testing.T) {
	sid := "active-session-dd"
	busID := ActivityChannelForSid(sid)
	now := time.Now().UTC()

	deps := peerAwarenessDeps{
		bus: singleEventBusReader{
			busID: busID,
			event: BusBlock{
				ID:   "test-block-1",
				Type: evtTailerBlock,
				Ts:   now.Format(time.RFC3339Nano),
				Seq:  1,
				Payload: map[string]interface{}{
					"kind":           "tool_use",
					"source_channel": "stdio",
					"ref":            "ref-001",
				},
			},
		},
		attn:     noopAttnReader{},
		handoffs: noopHandoffReader{},
		renderer: noopEmitter{},
	}

	blk := buildPeersBlock(sid, deps, DefaultPeerBlockBudget)
	if blk == nil {
		t.Fatal("expected non-nil block with activity events")
	}
	// When there is activity data the block should NOT be the fallback line.
	if strings.Contains(blk.Content, "No active peers.") {
		t.Errorf("block with activity events should not render 'No active peers.', got:\n%s", blk.Content)
	}
	// The packet should contain the "MY RECENT ACTIVITY" section header.
	if !strings.Contains(blk.Content, "MY RECENT ACTIVITY") {
		t.Errorf("expected MY RECENT ACTIVITY section, got:\n%s", blk.Content)
	}
}

// TestBlockPeers_FrameMetadata confirms that BlockPeers gets the correct
// tier (2) and stability (50) defaults from the frame metadata tables.
func TestBlockPeers_FrameMetadata(t *testing.T) {
	sid := "frame-meta-ee"
	blk := buildPeersBlock(sid, emptyPeerDeps(), DefaultPeerBlockBudget)
	if blk == nil {
		t.Fatal("expected non-nil block")
	}
	if blk.Tier != 2 {
		t.Errorf("BlockPeers tier: got %d want 2", blk.Tier)
	}
	if blk.Stability != 50 {
		t.Errorf("BlockPeers stability: got %d want 50", blk.Stability)
	}
	if blk.Hash == "" {
		t.Error("BlockPeers hash should not be empty")
	}
}

// ─── renderPeersBlockContent unit tests ──────────────────────────────────────

func TestRenderPeersBlockContent_NilResult(t *testing.T) {
	got := renderPeersBlockContent(nil)
	if !strings.Contains(got, "No active peers.") {
		t.Errorf("nil result should render fallback, got:\n%s", got)
	}
}

func TestRenderPeersBlockContent_EmptyPacket(t *testing.T) {
	result := &PeerAwarenessResult{Packet: "", TokenCount: 0}
	got := renderPeersBlockContent(result)
	if !strings.Contains(got, "No active peers.") {
		t.Errorf("empty packet should render fallback, got:\n%s", got)
	}
}

func TestRenderPeersBlockContent_WhitespaceOnlyPacket(t *testing.T) {
	result := &PeerAwarenessResult{Packet: "   \n  ", TokenCount: 1}
	got := renderPeersBlockContent(result)
	if !strings.Contains(got, "No active peers.") {
		t.Errorf("whitespace-only packet should render fallback, got:\n%s", got)
	}
}

func TestRenderPeersBlockContent_NonEmptyPacket(t *testing.T) {
	result := &PeerAwarenessResult{
		Packet:     "MY RECENT ACTIVITY:\n  13:42 tool_use: stdio",
		TokenCount: 10,
	}
	got := renderPeersBlockContent(result)
	if strings.Contains(got, "No active peers.") {
		t.Errorf("non-empty packet should NOT render fallback, got:\n%s", got)
	}
	if !strings.Contains(got, "MY RECENT ACTIVITY") {
		t.Errorf("packet content should be present, got:\n%s", got)
	}
	if !strings.Contains(got, "Peer Awareness") {
		t.Errorf("section header should always be present, got:\n%s", got)
	}
}

// ─── end-to-end integration test ─────────────────────────────────────────────

// TestFoveatedContext_IncludesPeersBlock is the end-to-end integration test:
// POST /v1/context/foveated with a valid session_id, confirm BlockPeers
// appears in the response.blocks list and in the rendered context string.
// When no peers are active the block still appears (collapsed to one line).
func TestFoveatedContext_IncludesPeersBlock(t *testing.T) {
	tmp := t.TempDir()
	cfg := &Config{
		WorkspaceRoot:               tmp,
		CogDir:                      tmp + "/.cog",
		Port:                        0,
		SalienceDaysWindow:          90,
		WriteRouteGrantAuthDisabled: true,
	}
	nucleus := &Nucleus{Name: "test-peers", Card: "peers integration"}
	process := NewProcess(cfg, nucleus)
	srv := NewServer(cfg, nucleus, process)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body, _ := json.Marshal(foveatedRequest{
		Prompt:    "who else is working right now",
		Iris:      irisSignal{Size: 200000, Used: 5000},
		Profile:   "claude-code",
		SessionID: "peers-integration-test",
	})

	resp, err := http.Post(ts.URL+"/v1/context/foveated", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}

	var result foveatedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal("decode:", err)
	}

	// The peers block must appear in the rendered context.
	if !strings.Contains(result.Context, "Peer Awareness") {
		t.Errorf("rendered context missing 'Peer Awareness' block:\n%s", result.Context)
	}

	// When no peers are active, the collapsed line should be present.
	if !strings.Contains(result.Context, "No active peers.") {
		t.Errorf("expected 'No active peers.' in quiet workspace:\n%s", result.Context)
	}

	// The block should also appear in the structured response.blocks list.
	var found *foveatedBlock
	for i := range result.Blocks {
		if result.Blocks[i].Name == BlockPeers {
			found = &result.Blocks[i]
			break
		}
	}
	if found == nil {
		names := make([]string, 0, len(result.Blocks))
		for _, b := range result.Blocks {
			names = append(names, b.Name)
		}
		t.Fatalf("peers block not in response.blocks; got %v", names)
	}
	if found.Tier != "tier2" {
		t.Errorf("peers tier=%q; want tier2", found.Tier)
	}
	if found.Stability != 50 {
		t.Errorf("peers stability=%d; want 50", found.Stability)
	}
	if found.Hash == "" {
		t.Error("peers block hash empty")
	}

	t.Logf("peers block preview: %s", found.Preview)
}

// TestFoveatedContext_NoPeersBlockWhenNoSessionID confirms that when no
// session_id is provided the peers block is absent from the response — the
// block requires a sid to render anything useful.
func TestFoveatedContext_NoPeersBlockWhenNoSessionID(t *testing.T) {
	tmp := t.TempDir()
	cfg := &Config{
		WorkspaceRoot:               tmp,
		CogDir:                      tmp + "/.cog",
		Port:                        0,
		SalienceDaysWindow:          90,
		WriteRouteGrantAuthDisabled: true,
	}
	nucleus := &Nucleus{Name: "no-sid-test", Card: "no sid"}
	process := NewProcess(cfg, nucleus)
	srv := NewServer(cfg, nucleus, process)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body, _ := json.Marshal(foveatedRequest{
		Prompt:  "what's going on",
		Iris:    irisSignal{Size: 200000, Used: 5000},
		Profile: "claude-code",
		// SessionID intentionally omitted
	})

	resp, err := http.Post(ts.URL+"/v1/context/foveated", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var result foveatedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal("decode:", err)
	}

	for _, b := range result.Blocks {
		if b.Name == BlockPeers {
			t.Errorf("peers block should be absent when session_id is empty, but found it: %+v", b)
		}
	}
}
