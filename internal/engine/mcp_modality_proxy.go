// mcp_modality_proxy.go — kernel-side MCP proxy for mod3 voice tools.
//
// Wave 3 of the mod3-kernel integration (ADR-082 + channel-provider RFC),
// consolidated in Wave 3.5 with Wave 2's session-ID authority.
// Wave 4.4 (this file) switches the primary speak path from the blocking
// /v1/synthesize endpoint to the queue-aware /v1/speak endpoint, so mod3's
// drain thread owns all audio scheduling and the kernel never spawns a local
// afplay/aplay process.
//
// Design locks:
//
//  1. REST transport = direct POST to mod3's /v1/speak (primary) or
//     /v1/synthesize (skip_playback=true, raw bytes). Mod3 does not expose
//     an /mcp endpoint; the prior StreamableClientTransport approach always
//     failed with "unable to connect". /v1/speak is non-blocking — it enqueues
//     and returns {job_id, queue_position, status} immediately. Mod3's drain
//     thread handles all audio; the kernel never spawns afplay/aplay for the
//     primary path. Other control handlers (/v1/stop, /v1/voices, /health)
//     continue to POST/GET against cfg.Mod3URL + "/v1/*" as before.
//  2. Session authority = kernel-owned (Wave 3.5). The session-family tools
//     (register/deregister/list) do NOT call mod3 directly — they call the
//     kernel's RegisterChannelSession / DeregisterChannelSession /
//     ListChannelSessions methods on the Server, which mint the session_id
//     and forward to mod3. Session ID minting happens in exactly one place.
//  3. SkipPlayback = callers that need raw bytes (dashboard WS, file write,
//     etc.) still use the /v1/synthesize HTTP path, which returns audio/wav.
//     The subscriber-routing path (Wave 4.3) also remains for skip_playback:
//     when a session has a live dashboard WebSocket subscriber, mod3 routes
//     WAV there independently; the kernel skips the local player.
//
// Tools registered (prefix `mod3_` to namespace against cog_* kernel tools):
//
//   - mod3_speak                — synthesize + (optionally) play      (direct to mod3)
//   - mod3_stop                 — cancel current/queued speech        (direct to mod3)
//   - mod3_voices               — list available voices               (direct to mod3)
//   - mod3_status               — mod3 /health probe + build info     (direct to mod3)
//   - mod3_register_session     — kernel-minted session registration  (via kernel)
//   - mod3_deregister_session   — session deregister                  (via kernel)
//   - mod3_list_sessions        — merged kernel+mod3 session roster   (via kernel)
//   - mod3_tail_logs            — tail chat-flow events from ring buffer (direct to mod3)
package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ─── proxy wiring on MCPServer ───────────────────────────────────────────────

// modalityProxy holds the HTTP client and playback helper used by the mod3_*
// MCP tools. Fields are exported-by-convention (capitalized where needed for
// tests) so test code can override the HTTP client and the player command.
type modalityProxy struct {
	// client is the HTTP client used for all mod3 forwards. Nil falls back
	// to defaultMod3ProxyClient.
	client *http.Client

	// player is the OS command executed for server-side audio playback.
	// Overridable in tests to a stub binary / /usr/bin/true. Empty means
	// "autodetect via runtime.GOOS" (afplay on darwin, aplay elsewhere).
	player string

	// playerArgs, when non-nil, are passed as additional command args
	// before the tempfile path. Useful for tests to pipe the wav through
	// a counting script. Nil means no extra args.
	playerArgs []string

	// disablePlayback short-circuits the player exec entirely. Tests set
	// this when they want to assert "we got the bytes" without spawning a
	// real player. Production code leaves it false.
	disablePlayback bool

	// subscriberCheck, when non-nil, is consulted before spawning the local
	// player in the fallback path of mod3_speak. If it returns (true, nil)
	// the kernel skips afplay — mod3's /ws/audio/{session_id} WebSocket is
	// already pushing the WAV to a dashboard subscriber (Wave 4.3). Errors
	// and false return values fall through to the normal playback path. Nil
	// means "use the default HTTP implementation"
	// (GET {Mod3URL}/v1/sessions/{id}/subscribers).
	subscriberCheck func(ctx context.Context, sessionID string) (bool, error)

	// speakFn, when non-nil, replaces callMod3SpeakTool's default REST POST
	// to /v1/synthesize. Tests inject this to simulate mod3's synthesize
	// responses (queue-state map) without hitting a real HTTP server.
	// Signature matches callMod3SpeakTool's return: (parsed map, error).
	//
	// Alias: the field was formerly named mcpSpeakFn; it is kept as speakFn
	// after the MCP-transport removal. Test code that assigned mcpSpeakFn
	// should be updated to speakFn; the old name is gone.
	speakFn func(ctx context.Context, in mod3SpeakInput) (map[string]any, error)
}

// defaultMod3ProxyTimeout is the per-request timeout for mod3 forwards. 30s
// covers the longest-plausible synthesis on the current Kokoro voice stack
// (~5-10s for multi-sentence input, with headroom for cold starts).
const defaultMod3ProxyTimeout = 30 * time.Second

// defaultMod3ProxyClient is the shared http.Client used when modalityProxy.client
// is nil. Lazily initialised; safe for concurrent use.
var defaultMod3ProxyClient = &http.Client{Timeout: defaultMod3ProxyTimeout}

// getModalityProxy returns the MCPServer's modality proxy, lazily creating
// one with sane defaults on first access. Tests can pre-seed m.mod3Proxy with
// their own instance before calling this.
func (m *MCPServer) getModalityProxy() *modalityProxy {
	if m.mod3Proxy == nil {
		m.mod3Proxy = &modalityProxy{}
	}
	return m.mod3Proxy
}

// ─── tool registration ───────────────────────────────────────────────────────

// registerMod3Tools installs the 7 mod3_* MCP tools. Called from
// MCPServer.registerTools after the cog_* tools so the tool index stays
// stable at the front.
func (m *MCPServer) registerMod3Tools() {
	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "mod3_speak",
		Description: "Synthesize text to speech via mod3's queue-aware /v1/speak " +
			"endpoint. Required: text. Optional: session_id, voice, speed, " +
			"emotion, skip_playback (return raw base64 bytes without queuing). " +
			"Returns mod3's queue state: status (speaking|queued), job_id, " +
			"queue_position (0=playing immediately, N=queued at position N). " +
			"Concurrent calls are serialized by mod3's drain thread — no " +
			"overlapping audio, no local afplay spawned by the kernel. " +
			"Fallback: curl -X POST http://localhost:7860/v1/speak " +
			"-H 'Content-Type: application/json' -d '{\"text\":\"...\"}'",
	}), withToolObserver(m, "mod3_speak", m.toolMod3Speak))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "mod3_stop",
		Description: "Stop current mod3 speech and/or cancel queued jobs. " +
			"Optional: session_id, job_id (cancel one specific job). Empty " +
			"cancels current playback and clears the queue. Returns mod3's " +
			"barge-in interruption context. Fallback: curl -X POST " +
			"http://localhost:7860/v1/stop",
	}), withToolObserver(m, "mod3_stop", m.toolMod3Stop))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "mod3_voices",
		Description: "List available mod3 voices, optionally scoped to a " +
			"session. Optional: session_id. Returns the voice catalogue mod3 " +
			"exposes (id, name, language, gender metadata per voice). " +
			"Fallback: curl http://localhost:7860/v1/voices",
	}), withToolObserver(m, "mod3_voices", m.toolMod3Voices))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "mod3_status",
		Description: "Probe mod3's /health endpoint. Returns the raw health " +
			"payload (model_loaded, engine info, queue_depth, etc). 502 if " +
			"mod3 is unreachable. Fallback: curl http://localhost:7860/health",
	}), withToolObserver(m, "mod3_status", m.toolMod3Status))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "mod3_register_session",
		Description: "Register a channel-participant session. Routes through " +
			"the kernel's /v1/channel-sessions/register endpoint so " +
			"session_id minting stays centralized (ADR-082 Wave 3.5). " +
			"Required: participant_id. Optional: session_id (kernel mints " +
			"a cs-* short UUID when absent), participant_type " +
			"(agent|user|provider), preferred_voice, preferred_output_device, " +
			"priority, kinds (e.g. [\"audio\"] per channel-provider RFC), " +
			"metadata (opaque pass-through). Returns the merged {kernel, " +
			"mod3} block: kernel identity record + mod3's full " +
			"SessionRegisterResponse (assigned_voice, voice_conflict, " +
			"output_device, queue_depth).",
	}), withToolObserver(m, "mod3_register_session", m.toolMod3RegisterSession))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "mod3_deregister_session",
		Description: "Deregister a channel-participant session. Routes " +
			"through the kernel's /v1/channel-sessions/{id}/deregister " +
			"endpoint so the kernel drops its identity record in sync with " +
			"mod3. Required: session_id. Returns mod3's deregister " +
			"acknowledgment (released_voice, dropped_jobs).",
	}), withToolObserver(m, "mod3_deregister_session", m.toolMod3DeregisterSession))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "mod3_list_sessions",
		Description: "List channel-participant sessions via the kernel's " +
			"/v1/channel-sessions endpoint. Returns a merged {kernel, mod3} " +
			"block: kernel identity records + mod3's live per-channel state " +
			"(voice_pool, voice_holders, serializer policy).",
	}), withToolObserver(m, "mod3_list_sessions", m.toolMod3ListSessions))

	mcp.AddTool(m.server, m.trackTool(&mcp.Tool{
		Name: "mod3_tail_logs",
		Description: "Tail recent structured chat-flow events from mod3's " +
			"in-memory ring buffer (up to 5000 events, DEBUG-level). " +
			"Each event has: ts, event_type, session_id, message_id, " +
			"from_seat, to_seats, content_hash, content_preview, direction. " +
			"Optional: session_id (filter to one session), " +
			"event_type (comma-separated, e.g. chat.message_received,chat.fan_out), " +
			"since (ISO timestamp or relative like 5m), " +
			"limit (default 50, max 500). " +
			"Fallback: curl 'http://localhost:7860/v1/logs/chat-flow?limit=20'",
	}), withToolObserver(m, "mod3_tail_logs", m.toolMod3TailLogs))
}

// ─── input / output types ────────────────────────────────────────────────────

type mod3SpeakInput struct {
	Text      string  `json:"text"`
	SessionID string  `json:"session_id,omitempty"`
	Voice     string  `json:"voice,omitempty"`
	Speed     float64 `json:"speed,omitempty"`
	Emotion   float64 `json:"emotion,omitempty"`
	// Blocking waits for the spawned player to exit before returning the
	// tool result. Default false — fire-and-forget so multi-second audio
	// doesn't block the MCP call.
	Blocking bool `json:"blocking,omitempty"`
	// SkipPlayback returns the wav bytes (base64) without attempting local
	// playback. Useful for callers routing audio elsewhere (dashboard WS,
	// file write, etc). Default false.
	SkipPlayback bool `json:"skip_playback,omitempty"`
}

type mod3StopInput struct {
	SessionID string `json:"session_id,omitempty"`
	JobID     string `json:"job_id,omitempty"`
}

type mod3VoicesInput struct {
	SessionID string `json:"session_id,omitempty"`
}

type mod3StatusInput struct{}

// mod3TailLogsInput controls the chat-flow log query parameters for mod3_tail_logs.
type mod3TailLogsInput struct {
	// SessionID filters to a single mod3 session (optional).
	SessionID string `json:"session_id,omitempty"`
	// EventType is a comma-separated list of event types to include, e.g.
	// "chat.message_received,chat.fan_out". Empty means all types.
	EventType string `json:"event_type,omitempty"`
	// Since is an ISO 8601 timestamp or a relative duration like "5m" or "30s".
	// Only events at or after this time are returned.
	Since string `json:"since,omitempty"`
	// Limit caps the number of events returned (default 50, max 500).
	Limit int `json:"limit,omitempty"`
}

type mod3RegisterSessionInput struct {
	SessionID             string `json:"session_id,omitempty"`
	ParticipantID         string `json:"participant_id"`
	ParticipantType       string `json:"participant_type,omitempty"`
	PreferredVoice        string `json:"preferred_voice,omitempty"`
	PreferredOutputDevice string `json:"preferred_output_device,omitempty"`
	Priority              int    `json:"priority,omitempty"`
	// Kinds / Metadata are the channel-provider RFC fields that flow
	// through to mod3 unchanged. See cogos_session_register primitive.
	Kinds    []string       `json:"kinds,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type mod3DeregisterSessionInput struct {
	SessionID string `json:"session_id"`
}

type mod3ListSessionsInput struct{}

// ─── handlers ────────────────────────────────────────────────────────────────

func (m *MCPServer) toolMod3Speak(ctx context.Context, req *mcp.CallToolRequest, in mod3SpeakInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Text) == "" {
		return textResult("text is required")
	}

	// SkipPlayback: caller wants raw audio bytes (dashboard WS, file write,
	// etc.). Fall through to /v1/synthesize directly so the caller gets bytes
	// back in the response rather than triggering local playback.
	if in.SkipPlayback {
		return m.toolMod3SpeakRawBytes(ctx, in)
	}

	// Primary path: POST to mod3's /v1/speak REST endpoint (non-blocking,
	// queue-aware). callMod3SpeakTool handles the HTTP round-trip and returns
	// the JSON response {job_id, queue_position, status}. Mod3's drain thread
	// owns all audio playback — the kernel never spawns afplay/aplay here.
	speakResult, speakErr := m.callMod3SpeakTool(ctx, in)
	if speakErr != nil {
		return mod3ErrorResult(fmt.Sprintf("mod3 unreachable: %v", speakErr))
	}

	// Echo session_id for observability consistency.
	speakResult["session_id"] = in.SessionID
	return marshalResult(speakResult)
}

// toolMod3SpeakRawBytes handles mod3_speak with skip_playback=true.
// It calls /v1/synthesize directly (not the MCP queue path) because the
// caller explicitly wants audio bytes, not queued server-side playback.
// The subscriber-routing logic (Wave 4.3) is preserved here: if the session
// has a live dashboard WebSocket subscriber, mod3 already routed the WAV
// there so we return routed_ws instead of double-playing.
func (m *MCPServer) toolMod3SpeakRawBytes(ctx context.Context, in mod3SpeakInput) (*mcp.CallToolResult, any, error) {
	body := buildSynthesizeBody(in)
	raw, _ := json.Marshal(body)

	audio, headers, status, err := m.proxyMod3Bytes(ctx, http.MethodPost,
		"/v1/synthesize", bytes.NewReader(raw), "application/json")
	if err != nil {
		return mod3ErrorResult(fmt.Sprintf("mod3 unreachable: %v", err))
	}
	if status < 200 || status >= 300 {
		return mod3ErrorResult(fmt.Sprintf("mod3 returned %d: %s", status, truncate(string(audio), 400)))
	}

	metrics := extractMod3Metrics(headers)
	result := map[string]any{
		"ok":           true,
		"bytes":        len(audio),
		"metrics":      metrics,
		"session_id":   in.SessionID,
		"audio_base64": base64.StdEncoding.EncodeToString(audio),
		"playback_status": "skipped",
	}
	return marshalResult(result)
}

// toolMod3SpeakLocalFallback is retained for backward compatibility with
// tests that exercise the audio+playback path directly. Production code
// routes through toolMod3Speak → callMod3SpeakTool (REST) → local player.
// This function is no longer called from toolMod3Speak; its body is
// preserved so existing test helpers can call it to validate the playback
// plumbing independently.
func (m *MCPServer) toolMod3SpeakLocalFallback(ctx context.Context, in mod3SpeakInput, fallbackReason error) (*mcp.CallToolResult, any, error) {
	body := buildSynthesizeBody(in)
	raw, _ := json.Marshal(body)

	audio, headers, status, err := m.proxyMod3Bytes(ctx, http.MethodPost,
		"/v1/synthesize", bytes.NewReader(raw), "application/json")
	if err != nil {
		reasonStr := "http unreachable"
		if fallbackReason != nil {
			reasonStr = fmt.Sprintf("primary: %v; http: %v", fallbackReason, err)
		}
		return mod3ErrorResult(fmt.Sprintf("mod3 unreachable: %s", reasonStr))
	}
	if status < 200 || status >= 300 {
		return mod3ErrorResult(fmt.Sprintf("mod3 returned %d: %s", status, truncate(string(audio), 400)))
	}

	metrics := extractMod3Metrics(headers)
	result := map[string]any{
		"ok":         true,
		"bytes":      len(audio),
		"metrics":    metrics,
		"session_id": in.SessionID,
	}
	if fallbackReason != nil {
		result["fallback_reason"] = fallbackReason.Error()
	}

	p := m.getModalityProxy()
	if p.disablePlayback {
		result["playback_status"] = "disabled"
		return marshalResult(result)
	}

	if in.SessionID != "" {
		subscribed, checkErr := m.checkSessionSubscriber(ctx, in.SessionID)
		if checkErr != nil {
			slog.Debug("mod3 proxy: subscriber check failed (fallback path)",
				"session_id", in.SessionID, "err", checkErr)
			result["subscriber_check_error"] = checkErr.Error()
		}
		if subscribed {
			result["playback_status"] = "routed_ws"
			return marshalResult(result)
		}
	}

	playErr := p.playAudio(audio, in.Blocking)
	switch {
	case playErr == nil && in.Blocking:
		result["playback_status"] = "played"
	case playErr == nil:
		result["playback_status"] = "spawned"
	default:
		result["playback_status"] = "error"
		result["playback_error"] = playErr.Error()
	}
	return marshalResult(result)
}

// callMod3SpeakTool POSTs to mod3's /v1/speak REST endpoint (non-blocking,
// queue-aware) and returns the parsed JSON response {job_id, queue_position,
// status}. Mod3's drain thread owns all audio playback — the kernel never
// spawns afplay/aplay for this path.
//
// Injectable via modalityProxy.speakFn for tests that want deterministic
// queue-state responses without hitting a real HTTP server.
func (m *MCPServer) callMod3SpeakTool(ctx context.Context, in mod3SpeakInput) (map[string]any, error) {
	p := m.getModalityProxy()
	if p.speakFn != nil {
		return p.speakFn(ctx, in)
	}

	if m.cfg == nil {
		return nil, errors.New("Mod3URL not configured (cfg nil)")
	}
	base := strings.TrimRight(m.cfg.Mod3URL, "/")
	if base == "" {
		return nil, errors.New("Mod3URL not configured")
	}

	body := buildSpeakBody(in)
	raw, _ := json.Marshal(body)

	respBytes, _, status, err := m.proxyMod3Bytes(ctx, http.MethodPost,
		"/v1/speak", bytes.NewReader(raw), "application/json")
	if err != nil {
		return nil, fmt.Errorf("mod3 /v1/speak: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("mod3 returned %d: %s", status, truncate(string(respBytes), 400))
	}

	// Parse the JSON response — {job_id, queue_position, status}.
	var result map[string]any
	if jsonErr := json.Unmarshal(respBytes, &result); jsonErr != nil {
		return nil, fmt.Errorf("mod3 /v1/speak: decode response: %w (raw=%s)", jsonErr, truncate(string(respBytes), 200))
	}
	return result, nil
}

// buildSpeakBody constructs the JSON body for a /v1/speak request.
// Used by callMod3SpeakTool for the primary queue-aware speak path.
func buildSpeakBody(in mod3SpeakInput) map[string]any {
	body := map[string]any{"text": in.Text}
	if in.Voice != "" {
		body["voice"] = in.Voice
	}
	if in.Speed > 0 {
		body["speed"] = in.Speed
	}
	if in.Emotion > 0 {
		body["emotion"] = in.Emotion
	}
	if in.SessionID != "" {
		body["session_id"] = in.SessionID
	}
	return body
}

// buildSynthesizeBody constructs the JSON body for a /v1/synthesize request
// from a mod3SpeakInput. Used only by the raw-bytes path (skip_playback=true).
func buildSynthesizeBody(in mod3SpeakInput) map[string]any {
	body := map[string]any{"text": in.Text}
	if in.Voice != "" {
		body["voice"] = in.Voice
	}
	if in.Speed > 0 {
		body["speed"] = in.Speed
	}
	if in.Emotion > 0 {
		body["emotion"] = in.Emotion
	}
	if in.SessionID != "" {
		body["session_id"] = in.SessionID
	}
	return body
}

// checkSessionSubscriber asks mod3 whether ``sessionID`` has at least one
// active dashboard WebSocket subscriber for audio playback. Returns
// ``(subscribed, nil)`` on success, ``(false, err)`` on transport failure.
// ``(false, nil)`` — the default when the proxy has no check configured —
// also suppresses the routing path, so legacy callers see the exact same
// afplay behavior as before.
//
// Injectable via modalityProxy.subscriberCheck for tests. The default is a
// GET against mod3's /v1/sessions/{id}/subscribers endpoint with a 1.5s
// timeout inherited from defaultMod3ProxyTimeout.
func (m *MCPServer) checkSessionSubscriber(ctx context.Context, sessionID string) (bool, error) {
	p := m.getModalityProxy()
	if p.subscriberCheck != nil {
		return p.subscriberCheck(ctx, sessionID)
	}
	// Default implementation — HTTP GET. Scoped to 1.5s so a wedged mod3
	// can't block a speak for more than that; falls back to afplay on timeout.
	checkCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	raw, _, status, err := m.proxyMod3Bytes(checkCtx, http.MethodGet,
		"/v1/sessions/"+url.PathEscape(sessionID)+"/subscribers", nil, "")
	if err != nil {
		return false, err
	}
	if status < 200 || status >= 300 {
		return false, fmt.Errorf("mod3 returned %d: %s", status, truncate(string(raw), 200))
	}
	var body struct {
		Subscribed bool `json:"subscribed"`
		Count      int  `json:"count"`
	}
	if unmarshalErr := json.Unmarshal(raw, &body); unmarshalErr != nil {
		return false, fmt.Errorf("decode subscribers response: %w", unmarshalErr)
	}
	return body.Subscribed, nil
}

func (m *MCPServer) toolMod3Stop(ctx context.Context, req *mcp.CallToolRequest, in mod3StopInput) (*mcp.CallToolResult, any, error) {
	path := "/v1/stop"
	q := url.Values{}
	if in.JobID != "" {
		q.Set("job_id", in.JobID)
	}
	if in.SessionID != "" {
		q.Set("session_id", in.SessionID)
	}
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	return m.proxyMod3JSONAsMCP(ctx, http.MethodPost, path, nil)
}

func (m *MCPServer) toolMod3Voices(ctx context.Context, req *mcp.CallToolRequest, in mod3VoicesInput) (*mcp.CallToolResult, any, error) {
	path := "/v1/voices"
	if in.SessionID != "" {
		path += "?session_id=" + url.QueryEscape(in.SessionID)
	}
	return m.proxyMod3JSONAsMCP(ctx, http.MethodGet, path, nil)
}

func (m *MCPServer) toolMod3Status(ctx context.Context, req *mcp.CallToolRequest, in mod3StatusInput) (*mcp.CallToolResult, any, error) {
	return m.proxyMod3JSONAsMCP(ctx, http.MethodGet, "/health", nil)
}

// toolMod3TailLogs queries mod3's /v1/logs/chat-flow endpoint and returns
// the structured event array. Supports the same filter params as the HTTP
// endpoint: session_id, event_type, since, limit.
//
// Relative "since" values like "5m" or "30s" are resolved against the current
// time before forwarding so mod3 receives a well-formed ISO timestamp.
func (m *MCPServer) toolMod3TailLogs(ctx context.Context, req *mcp.CallToolRequest, in mod3TailLogsInput) (*mcp.CallToolResult, any, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	q := url.Values{}
	if in.SessionID != "" {
		q.Set("session_id", in.SessionID)
	}
	if in.EventType != "" {
		q.Set("event_type", in.EventType)
	}
	if in.Since != "" {
		resolved := resolveSince(in.Since)
		q.Set("since", resolved)
	}
	q.Set("limit", strconv.Itoa(limit))

	path := "/v1/logs/chat-flow?" + q.Encode()
	return m.proxyMod3JSONAsMCP(ctx, http.MethodGet, path, nil)
}

// resolveSince converts a relative duration string ("5m", "30s", "1h") to an
// ISO 8601 UTC timestamp. If the input is already an ISO timestamp (contains
// 'T' or '-') it is returned unchanged. Unrecognised formats pass through.
func resolveSince(s string) string {
	if s == "" {
		return s
	}
	// If it looks like an ISO timestamp already, return as-is.
	if strings.Contains(s, "T") || (len(s) > 4 && s[4] == '-') {
		return s
	}
	// Try to parse as a Go duration (e.g. "5m", "30s", "1h").
	d, err := time.ParseDuration(s)
	if err != nil {
		return s // pass through unmodified
	}
	return time.Now().UTC().Add(-d).Format(time.RFC3339)
}

// toolMod3RegisterSession routes through the kernel's shared
// RegisterChannelSession so session_id minting happens in exactly one place
// (ADR-082 Wave 3.5). The previous Wave 3 implementation called mod3's
// /v1/sessions/register directly, which bypassed Wave 2's kernel-owned
// minting authority; that path is now gone.
func (m *MCPServer) toolMod3RegisterSession(ctx context.Context, req *mcp.CallToolRequest, in mod3RegisterSessionInput) (*mcp.CallToolResult, any, error) {
	if in.ParticipantID == "" {
		return textResult("participant_id is required")
	}
	if m.channelSessionBackend == nil {
		return mod3ErrorResult("channel-session backend not configured")
	}
	resp, ferr := m.channelSessionBackend.RegisterChannelSession(ctx, channelSessionRegisterRequest{
		SessionID:             in.SessionID,
		ParticipantID:         in.ParticipantID,
		ParticipantType:       in.ParticipantType,
		PreferredVoice:        in.PreferredVoice,
		PreferredOutputDevice: in.PreferredOutputDevice,
		Priority:              in.Priority,
		Kinds:                 in.Kinds,
		Metadata:              in.Metadata,
	})
	if ferr != nil {
		return mod3ErrorResult(channelSessionForwardErrorText(ferr))
	}
	return marshalResult(resp)
}

func (m *MCPServer) toolMod3DeregisterSession(ctx context.Context, req *mcp.CallToolRequest, in mod3DeregisterSessionInput) (*mcp.CallToolResult, any, error) {
	if in.SessionID == "" {
		return textResult("session_id is required")
	}
	if m.channelSessionBackend == nil {
		return mod3ErrorResult("channel-session backend not configured")
	}
	mod3Resp, status, ferr := m.channelSessionBackend.DeregisterChannelSession(ctx, in.SessionID)
	if ferr != nil {
		return mod3ErrorResult(channelSessionForwardErrorText(ferr))
	}
	// Parse mod3's JSON body; surface mod3's non-2xx bodies intact as
	// tool errors. The HTTP handler passes these through verbatim; the
	// MCP tool wraps them so the caller sees the mod3 body text.
	var parsed any
	if len(mod3Resp) > 0 {
		if jsonErr := json.Unmarshal(mod3Resp, &parsed); jsonErr != nil {
			parsed = map[string]any{"raw": string(mod3Resp)}
		}
	}
	if status < 200 || status >= 300 {
		return mod3ErrorResult(fmt.Sprintf("mod3 returned %d: %v", status, parsed))
	}
	return marshalResult(parsed)
}

func (m *MCPServer) toolMod3ListSessions(ctx context.Context, req *mcp.CallToolRequest, in mod3ListSessionsInput) (*mcp.CallToolResult, any, error) {
	if m.channelSessionBackend == nil {
		return mod3ErrorResult("channel-session backend not configured")
	}
	resp, _, ferr := m.channelSessionBackend.ListChannelSessions(ctx)
	if ferr != nil {
		return mod3ErrorResult(channelSessionForwardErrorText(ferr))
	}
	return marshalResult(resp)
}

// channelSessionForwardErrorText renders a *channelSessionForwardError into
// the "mod3 unreachable" / "mod3 returned N: body" shape the legacy MCP
// tool paths used, keeping error surfaces stable for callers that previously
// matched on those strings.
func channelSessionForwardErrorText(ferr *channelSessionForwardError) string {
	switch ferr.Kind {
	case "mod3_unreachable":
		return ferr.Message
	case "mod3_rejected":
		var parsed any
		if len(ferr.Mod3Body) > 0 {
			if jsonErr := json.Unmarshal(ferr.Mod3Body, &parsed); jsonErr != nil {
				parsed = map[string]any{"raw": string(ferr.Mod3Body)}
			}
		}
		return fmt.Sprintf("mod3 returned %d: %v", ferr.HTTPStatus, parsed)
	default:
		return ferr.Message
	}
}

// ─── HTTP forwarder primitives ───────────────────────────────────────────────

// proxyMod3Bytes issues an HTTP request to mod3 and returns the raw body,
// response headers, HTTP status, and a transport error. Caller owns the body
// bytes; they may be audio/wav (mod3_speak) or JSON (everything else).
func (m *MCPServer) proxyMod3Bytes(ctx context.Context, method, path string, body io.Reader, contentType string) ([]byte, http.Header, int, error) {
	if m.cfg == nil {
		return nil, nil, 0, errors.New("Mod3URL not configured (cfg nil)")
	}
	base := strings.TrimRight(m.cfg.Mod3URL, "/")
	if base == "" {
		return nil, nil, 0, errors.New("Mod3URL not configured")
	}

	reqCtx, cancel := context.WithTimeout(ctx, defaultMod3ProxyTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, base+path, body)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("build request: %w", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// Accept both audio and JSON so a single client path covers synthesize
	// (audio/wav) and the rest (application/json).
	req.Header.Set("Accept", "audio/wav, application/json")

	client := m.getModalityProxy().client
	if client == nil {
		client = defaultMod3ProxyClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, 0, err
	}
	defer resp.Body.Close()

	// 16 MB cap — more than enough for multi-minute Kokoro wav at 24kHz
	// (~2 MB per 30s), safety net against an upstream that never closes.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, resp.Header, resp.StatusCode, fmt.Errorf("read response body: %w", err)
	}
	return raw, resp.Header, resp.StatusCode, nil
}

// proxyMod3JSONAsMCP is a convenience wrapper for tools whose response is
// JSON (everything except mod3_speak). Reads the body, parses it as JSON if
// possible, and returns an mcp.CallToolResult; on non-2xx status returns a
// mod3-error marshalled result so the caller sees the mod3 body intact.
func (m *MCPServer) proxyMod3JSONAsMCP(ctx context.Context, method, path string, body io.Reader) (*mcp.CallToolResult, any, error) {
	contentType := ""
	if body != nil {
		contentType = "application/json"
	}
	raw, _, status, err := m.proxyMod3Bytes(ctx, method, path, body, contentType)
	if err != nil {
		return mod3ErrorResult(fmt.Sprintf("mod3 unreachable: %v", err))
	}
	// Try to parse as JSON; if parse fails, surface the body as text so the
	// caller at least sees what mod3 said.
	var parsed any
	if len(raw) > 0 {
		if jsonErr := json.Unmarshal(raw, &parsed); jsonErr != nil {
			parsed = map[string]any{"raw": string(raw)}
		}
	}
	if status < 200 || status >= 300 {
		return mod3ErrorResult(fmt.Sprintf("mod3 returned %d: %v", status, parsed))
	}
	return marshalResult(parsed)
}

// mod3ErrorResult returns an IsError=true CallToolResult so the observer
// wrapper records the tool invocation as a failure in the ledger.
func mod3ErrorResult(msg string) (*mcp.CallToolResult, any, error) {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: msg}},
		IsError: true,
	}, nil, nil
}

// extractMod3Metrics pulls the X-Mod3-* headers into a metrics map. Numeric
// fields are parsed when possible; unknown headers pass through as strings
// so future mod3 headers surface without code changes.
func extractMod3Metrics(h http.Header) map[string]any {
	out := map[string]any{}
	for key, values := range h {
		lk := strings.ToLower(key)
		if !strings.HasPrefix(lk, "x-mod3-") || len(values) == 0 {
			continue
		}
		short := strings.TrimPrefix(lk, "x-mod3-")
		v := values[0]
		// Try numeric parse for the common metric headers.
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			// Preserve integers as int for nicer JSON output.
			if i, ierr := strconv.ParseInt(v, 10, 64); ierr == nil {
				out[short] = i
			} else {
				out[short] = f
			}
			continue
		}
		out[short] = v
	}
	return out
}

// ─── playback helper ─────────────────────────────────────────────────────────

// playAudio writes the wav bytes to a tempfile and spawns the platform's
// default player. When blocking==false the function returns immediately
// after the process starts; a goroutine waits for exit so the tempfile can
// be cleaned up. When blocking==true the function waits for exit and
// surfaces any non-zero return as an error.
//
// In tests, set modalityProxy.player to "/usr/bin/true" (or similar) to
// avoid actually playing audio.
func (p *modalityProxy) playAudio(wav []byte, blocking bool) error {
	if p.disablePlayback {
		return nil
	}
	f, err := os.CreateTemp("", "mod3-speak-*.wav")
	if err != nil {
		return fmt.Errorf("tempfile: %w", err)
	}
	path := f.Name()
	if _, err := f.Write(wav); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("write tempfile: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("close tempfile: %w", err)
	}

	player := p.player
	if player == "" {
		player = defaultPlayerCommand()
	}
	if player == "" {
		os.Remove(path)
		return fmt.Errorf("no audio player available for GOOS=%s", runtime.GOOS)
	}

	args := append([]string{}, p.playerArgs...)
	args = append(args, path)
	cmd := exec.Command(player, args...)

	if err := cmd.Start(); err != nil {
		os.Remove(path)
		return fmt.Errorf("start %s: %w", player, err)
	}

	if blocking {
		err := cmd.Wait()
		_ = os.Remove(path)
		if err != nil {
			return fmt.Errorf("player %s exited: %w", player, err)
		}
		return nil
	}
	// Fire-and-forget: reap the child so the tempfile gets cleaned and the
	// process isn't a zombie. Log errors; don't propagate (the MCP call
	// already returned successfully).
	go func() {
		if werr := cmd.Wait(); werr != nil {
			slog.Debug("mod3 proxy: player exited non-zero",
				"player", player, "path", path, "err", werr)
		}
		_ = os.Remove(path)
	}()
	return nil
}

// defaultPlayerCommand returns the preferred platform player, or "" when
// none is available in PATH. Resolved lazily per call so tests that change
// PATH take effect.
func defaultPlayerCommand() string {
	candidates := map[string][]string{
		"darwin":  {"afplay"},
		"linux":   {"aplay", "paplay"},
		"freebsd": {"aplay"},
	}
	for _, name := range candidates[runtime.GOOS] {
		if _, err := exec.LookPath(name); err == nil {
			return name
		}
	}
	// Final fallback: if neither platform default is present, see if the
	// caller has exposed one via PATH under its canonical name.
	for _, name := range []string{"afplay", "aplay", "paplay", "ffplay"} {
		if _, err := exec.LookPath(name); err == nil {
			return name
		}
	}
	return ""
}
