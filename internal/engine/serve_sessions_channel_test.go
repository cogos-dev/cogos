// serve_sessions_channel_test.go — end-to-end tests for the kernel-side
// channel-session forwarder. Each test stands up a fake mod3 via
// httptest.NewServer and exercises the kernel handler against it.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// ─── fakeMod3 — a tiny stub that canonically mirrors mod3's responses ────────

type fakeMod3 struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	captured []capturedRequest

	// Overrides — tests set these to control per-endpoint behavior.
	registerHandler   http.HandlerFunc
	deregisterHandler http.HandlerFunc
	listHandler       http.HandlerFunc
	getHandler        http.HandlerFunc
}

type capturedRequest struct {
	Method string
	Path   string
	Body   []byte
}

func newFakeMod3(t *testing.T) *fakeMod3 {
	t.Helper()
	fm := &fakeMod3{t: t}
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/sessions/register", func(w http.ResponseWriter, r *http.Request) {
		fm.capture(r)
		if fm.registerHandler != nil {
			fm.registerHandler(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		sid, _ := body["session_id"].(string)
		resp := map[string]any{
			"session_id":     sid,
			"participant_id": body["participant_id"],
			"assigned_voice": "bm_lewis",
			"voice_conflict": false,
			"output_device":  map[string]any{"name": "system-default", "live": true},
			"queue_depth":    0,
			"created":        true,
		}
		writeFakeJSON(w, http.StatusOK, resp)
	})

	mux.HandleFunc("POST /v1/sessions/{id}/deregister", func(w http.ResponseWriter, r *http.Request) {
		fm.capture(r)
		if fm.deregisterHandler != nil {
			fm.deregisterHandler(w, r)
			return
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"status":         "ok",
			"session_id":     r.PathValue("id"),
			"released_voice": "bm_lewis",
			"dropped_jobs":   0,
		})
	})

	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		fm.capture(r)
		if fm.listHandler != nil {
			fm.listHandler(w, r)
			return
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"sessions":      []any{},
			"serializer":    map[string]any{"policy": "round-robin"},
			"voice_pool":    []any{"bm_lewis", "af_bella"},
			"voice_holders": map[string]any{},
		})
	})

	mux.HandleFunc("GET /v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		fm.capture(r)
		if fm.getHandler != nil {
			fm.getHandler(w, r)
			return
		}
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"session_id":     r.PathValue("id"),
			"assigned_voice": "bm_lewis",
			"output_device":  map[string]any{"name": "system-default"},
		})
	})

	fm.srv = httptest.NewServer(mux)
	t.Cleanup(func() { fm.srv.Close() })
	return fm
}

func (fm *fakeMod3) capture(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	fm.mu.Lock()
	defer fm.mu.Unlock()
	fm.captured = append(fm.captured, capturedRequest{
		Method: r.Method, Path: r.URL.Path, Body: body,
	})
}

func (fm *fakeMod3) lastCaptured() capturedRequest {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	if len(fm.captured) == 0 {
		fm.t.Fatalf("expected at least one captured request")
	}
	return fm.captured[len(fm.captured)-1]
}

func writeFakeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// ─── newChannelServer — a Server wired just enough to exercise the routes ────

// newChannelServer builds a Server with only the fields needed by the
// channel-session handlers: cfg.Mod3URL pointing at fake mod3, a fresh
// ChannelSessionRegistry, and mux routes registered. The httptest.Client
// used by the kernel is overridden so every call targets fake mod3 regardless
// of the Mod3URL's scheme (handy for tests that want to inject network
// failures without depending on DNS).
func newChannelServer(t *testing.T, fm *fakeMod3) (*Server, *httptest.Server) {
	t.Helper()
	cfg := &Config{Mod3URL: fm.srv.URL}
	s := &Server{
		cfg:                    cfg,
		channelSessionRegistry: NewChannelSessionRegistry(),
	}
	mux := http.NewServeMux()
	s.registerChannelSessionRoutes(mux)
	front := httptest.NewServer(mux)
	t.Cleanup(func() { front.Close() })
	return s, front
}

// ─── Tests ───────────────────────────────────────────────────────────────────

func TestChannelSessionRegister_MintsIDWhenOmitted(t *testing.T) {
	fm := newFakeMod3(t)
	s, front := newChannelServer(t, fm)

	body, _ := json.Marshal(map[string]any{
		"participant_id": "cog",
	})
	resp, err := http.Post(front.URL+"/v1/channel-sessions/register",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d; body: %s", resp.StatusCode, raw)
	}

	var decoded channelSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decoded.Kernel == nil {
		t.Fatalf("expected kernel block, got nil")
	}
	if decoded.Kernel.SessionID == "" {
		t.Fatal("expected minted session_id, got empty string")
	}
	if !strings.HasPrefix(decoded.Kernel.SessionID, "cs-") {
		t.Fatalf("expected minted ID to carry cs- prefix, got %q", decoded.Kernel.SessionID)
	}
	if decoded.Kernel.IDSource != "minted" {
		t.Fatalf("expected id_source=minted, got %q", decoded.Kernel.IDSource)
	}

	// Mod3 should have seen the kernel-minted ID.
	cap := fm.lastCaptured()
	var forwarded map[string]any
	if err := json.Unmarshal(cap.Body, &forwarded); err != nil {
		t.Fatalf("unmarshal forwarded body: %v", err)
	}
	if forwarded["session_id"] != decoded.Kernel.SessionID {
		t.Fatalf("expected mod3 to receive minted session_id %q, got %v",
			decoded.Kernel.SessionID, forwarded["session_id"])
	}

	// Kernel registry should hold the record.
	if _, ok := s.channelSessionRegistry.Get(decoded.Kernel.SessionID); !ok {
		t.Fatal("expected kernel registry to retain record after success")
	}
}

func TestChannelSessionRegister_UsesCallerSuppliedID(t *testing.T) {
	fm := newFakeMod3(t)
	_, front := newChannelServer(t, fm)

	body, _ := json.Marshal(map[string]any{
		"session_id":              "vox-42",
		"participant_id":          "sandy",
		"participant_type":        "agent",
		"preferred_voice":         "af_bella",
		"preferred_output_device": "AirPods",
		"priority":                3,
	})
	resp, err := http.Post(front.URL+"/v1/channel-sessions/register",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d; body: %s", resp.StatusCode, raw)
	}

	var decoded channelSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Kernel.SessionID != "vox-42" {
		t.Fatalf("expected caller session_id preserved, got %q", decoded.Kernel.SessionID)
	}
	if decoded.Kernel.IDSource != "caller" {
		t.Fatalf("expected id_source=caller, got %q", decoded.Kernel.IDSource)
	}

	// Verify all caller-supplied fields made it through the forward.
	cap := fm.lastCaptured()
	var forwarded map[string]any
	_ = json.Unmarshal(cap.Body, &forwarded)
	if forwarded["session_id"] != "vox-42" ||
		forwarded["participant_id"] != "sandy" ||
		forwarded["participant_type"] != "agent" ||
		forwarded["preferred_voice"] != "af_bella" ||
		forwarded["preferred_output_device"] != "AirPods" {
		t.Fatalf("forwarded fields mismatch: %v", forwarded)
	}
	// Priority is a number in JSON; compare via float64.
	if p, _ := forwarded["priority"].(float64); int(p) != 3 {
		t.Fatalf("expected priority=3 forwarded, got %v", forwarded["priority"])
	}
}

func TestChannelSessionRegister_MergesMod3Response(t *testing.T) {
	fm := newFakeMod3(t)
	fm.registerHandler = func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, http.StatusOK, map[string]any{
			"session_id":     "cs-fixed",
			"assigned_voice": "bm_oxford",
			"voice_conflict": true,
			"queue_depth":    5,
			"created":        false,
		})
	}
	_, front := newChannelServer(t, fm)

	body, _ := json.Marshal(map[string]any{
		"session_id":     "cs-fixed",
		"participant_id": "cog",
	})
	resp, err := http.Post(front.URL+"/v1/channel-sessions/register",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST register: %v", err)
	}
	defer resp.Body.Close()

	// The merged response should carry mod3's assigned_voice and
	// voice_conflict fields intact.
	var decoded map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var mod3 map[string]any
	if err := json.Unmarshal(decoded["mod3"], &mod3); err != nil {
		t.Fatalf("decode mod3 block: %v", err)
	}
	if mod3["assigned_voice"] != "bm_oxford" {
		t.Fatalf("expected assigned_voice=bm_oxford in merge, got %v", mod3["assigned_voice"])
	}
	if mod3["voice_conflict"] != true {
		t.Fatalf("expected voice_conflict=true in merge, got %v", mod3["voice_conflict"])
	}
}

func TestChannelSessionRegister_ReturnsBadGatewayWhenMod3Down(t *testing.T) {
	// Build a fakeMod3 and immediately close it so the URL refuses connections.
	fm := newFakeMod3(t)
	fm.srv.Close()
	s, front := newChannelServer(t, fm)

	body, _ := json.Marshal(map[string]any{"participant_id": "cog"})
	resp, err := http.Post(front.URL+"/v1/channel-sessions/register",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 502, got %d; body: %s", resp.StatusCode, raw)
	}
	// Kernel must NOT hold a record for a failed forward.
	if s.channelSessionRegistry.Len() != 0 {
		t.Fatalf("expected empty kernel registry after 502, got %d rows",
			s.channelSessionRegistry.Len())
	}
}

func TestChannelSessionRegister_PropagatesMod3Error(t *testing.T) {
	fm := newFakeMod3(t)
	fm.registerHandler = func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "participant_type must be agent|user",
		})
	}
	s, front := newChannelServer(t, fm)

	body, _ := json.Marshal(map[string]any{
		"participant_id":   "cog",
		"participant_type": "alien",
	})
	resp, err := http.Post(front.URL+"/v1/channel-sessions/register",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400 passthrough, got %d; body: %s", resp.StatusCode, raw)
	}
	var decoded map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	if !strings.Contains(decoded["error"], "participant_type") {
		t.Fatalf("expected mod3 error body preserved, got %v", decoded)
	}
	if s.channelSessionRegistry.Len() != 0 {
		t.Fatal("expected kernel registry empty when mod3 rejected registration")
	}
}

// TestChannelSessionRegister_ForwardsKindsAndMetadata verifies the Wave 3.5
// schema alignment: the optional `kinds` array and `metadata` object from
// the channel-provider RFC's cogos_session_register primitive flow through
// Wave 2's register endpoint and land in mod3's request body unchanged.
func TestChannelSessionRegister_ForwardsKindsAndMetadata(t *testing.T) {
	fm := newFakeMod3(t)
	s, front := newChannelServer(t, fm)

	body, _ := json.Marshal(map[string]any{
		"session_id":       "cs-kinds-http",
		"participant_id":   "mod3-provider",
		"participant_type": "provider",
		"kinds":            []string{"audio"},
		"metadata": map[string]any{
			"provider_id": "mod3-local",
			"build":       "0.5.0",
		},
	})
	resp, err := http.Post(front.URL+"/v1/channel-sessions/register",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d; body: %s", resp.StatusCode, raw)
	}

	cap := fm.lastCaptured()
	var forwarded map[string]any
	if err := json.Unmarshal(cap.Body, &forwarded); err != nil {
		t.Fatalf("decode forwarded body: %v", err)
	}
	kinds, ok := forwarded["kinds"].([]any)
	if !ok || len(kinds) != 1 || kinds[0] != "audio" {
		t.Fatalf("expected mod3 to receive kinds=[\"audio\"], got %v", forwarded["kinds"])
	}
	md, ok := forwarded["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("expected mod3 to receive metadata object, got %v (%T)",
			forwarded["metadata"], forwarded["metadata"])
	}
	if md["provider_id"] != "mod3-local" {
		t.Fatalf("expected metadata.provider_id=mod3-local forwarded, got %v", md["provider_id"])
	}

	// Kernel record must retain the RFC fields so downstream consumers
	// can filter by capability even when mod3 ignores them.
	rec, ok := s.channelSessionRegistry.Get("cs-kinds-http")
	if !ok {
		t.Fatal("expected kernel registry to hold record")
	}
	if len(rec.Kinds) != 1 || rec.Kinds[0] != "audio" {
		t.Fatalf("expected record.Kinds=[audio], got %v", rec.Kinds)
	}
	if rec.Metadata["provider_id"] != "mod3-local" {
		t.Fatalf("expected record.Metadata.provider_id=mod3-local, got %v", rec.Metadata)
	}
}

func TestChannelSessionRegister_RequiresParticipantID(t *testing.T) {
	fm := newFakeMod3(t)
	_, front := newChannelServer(t, fm)

	body, _ := json.Marshal(map[string]any{})
	resp, err := http.Post(front.URL+"/v1/channel-sessions/register",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	// No forward should have happened.
	fm.mu.Lock()
	if len(fm.captured) != 0 {
		t.Fatalf("expected no forward to mod3 when participant_id missing, got %d", len(fm.captured))
	}
	fm.mu.Unlock()
}

func TestChannelSessionDeregister_Forwards(t *testing.T) {
	fm := newFakeMod3(t)
	s, front := newChannelServer(t, fm)

	// Pre-populate the kernel registry so we can verify deletion.
	s.channelSessionRegistry.Put(ChannelSessionRecord{
		SessionID: "cs-to-drop", ParticipantID: "cog",
	})

	req, _ := http.NewRequest(http.MethodPost,
		front.URL+"/v1/channel-sessions/cs-to-drop/deregister", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("deregister: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d; body: %s", resp.StatusCode, raw)
	}
	if _, ok := s.channelSessionRegistry.Get("cs-to-drop"); ok {
		t.Fatal("expected kernel registry to drop record after successful deregister")
	}
	cap := fm.lastCaptured()
	if cap.Path != "/v1/sessions/cs-to-drop/deregister" || cap.Method != http.MethodPost {
		t.Fatalf("unexpected forward: %+v", cap)
	}
}

func TestChannelSessionDeregister_Returns502WhenMod3Down(t *testing.T) {
	fm := newFakeMod3(t)
	fm.srv.Close()
	s, front := newChannelServer(t, fm)

	s.channelSessionRegistry.Put(ChannelSessionRecord{SessionID: "keeper"})

	req, _ := http.NewRequest(http.MethodPost,
		front.URL+"/v1/channel-sessions/keeper/deregister", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("deregister: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502, got %d", resp.StatusCode)
	}
	// On transport failure the kernel keeps the record — nothing authoritative
	// changed. The caller can retry.
	if _, ok := s.channelSessionRegistry.Get("keeper"); !ok {
		t.Fatal("expected kernel to preserve record on transport failure")
	}
}

func TestChannelSessionList_MergesSnapshots(t *testing.T) {
	fm := newFakeMod3(t)
	s, front := newChannelServer(t, fm)

	// Seed kernel registry so we can see the kernel block in the merged response.
	s.channelSessionRegistry.Put(ChannelSessionRecord{
		SessionID: "cs-seed", ParticipantID: "cog",
	})

	resp, err := http.Get(front.URL + "/v1/channel-sessions")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var decoded channelSessionListResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded.Kernel) != 1 || decoded.Kernel[0].SessionID != "cs-seed" {
		t.Fatalf("expected kernel snapshot of 1, got %+v", decoded.Kernel)
	}
	if len(decoded.Mod3) == 0 {
		t.Fatal("expected mod3 block populated from fake mod3")
	}
}

func TestChannelSessionGet_MergesKernelAndMod3(t *testing.T) {
	fm := newFakeMod3(t)
	s, front := newChannelServer(t, fm)
	s.channelSessionRegistry.Put(ChannelSessionRecord{
		SessionID: "cs-detail", ParticipantID: "cog", PreferredVoice: "bm_lewis",
	})

	resp, err := http.Get(front.URL + "/v1/channel-sessions/cs-detail")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var decoded channelSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Kernel == nil || decoded.Kernel.SessionID != "cs-detail" {
		t.Fatalf("expected kernel record returned, got %+v", decoded.Kernel)
	}
	if len(decoded.Mod3) == 0 {
		t.Fatal("expected mod3 block populated from fake mod3")
	}
}

func TestChannelSessionGet_Returns404WhenMod3NotFound(t *testing.T) {
	fm := newFakeMod3(t)
	fm.getHandler = func(w http.ResponseWriter, r *http.Request) {
		writeFakeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	}
	_, front := newChannelServer(t, fm)

	resp, err := http.Get(front.URL + "/v1/channel-sessions/does-not-exist")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 passthrough, got %d", resp.StatusCode)
	}
}

// ─── forwardMod3 direct tests ────────────────────────────────────────────────

func TestForwardMod3_ErrorsWhenURLUnset(t *testing.T) {
	s := &Server{cfg: &Config{Mod3URL: ""}}
	_, _, err := s.forwardMod3(context.Background(), http.MethodGet, "/v1/sessions", nil)
	if err == nil {
		t.Fatal("expected error when Mod3URL is empty")
	}
}

func TestForwardMod3_TimeoutYieldsTransportError(t *testing.T) {
	// A listener that accepts connections but never writes — guarantees the
	// 8s per-request timeout fires. Use a short deadline so the test runs
	// quickly.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	s := &Server{
		cfg:        &Config{Mod3URL: "http://" + l.Addr().String()},
		mod3Client: &http.Client{Timeout: 50 * 1e6}, // 50ms
	}
	_, _, err = s.forwardMod3(context.Background(), http.MethodGet, "/v1/sessions", nil)
	if err == nil {
		t.Fatal("expected transport error on stalled server")
	}
	// Deadline exceeded / i/o timeout / context canceled are all acceptable shapes.
	msg := err.Error()
	if !strings.Contains(strings.ToLower(msg), "timeout") &&
		!strings.Contains(strings.ToLower(msg), "deadline") &&
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected timeout-ish error, got %v", err)
	}
}

// ─── minor sanity tests for minting + helpers ────────────────────────────────

func TestMintChannelSessionID_ShapeAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := mintChannelSessionID()
		if !strings.HasPrefix(id, "cs-") {
			t.Fatalf("expected cs- prefix, got %q", id)
		}
		if len(id) != 3+12 {
			t.Fatalf("expected 15-char ID (cs- + 12 hex), got len=%d (%q)", len(id), id)
		}
		if seen[id] {
			t.Fatalf("mint collision after %d iterations: %q", i, id)
		}
		seen[id] = true
	}
}

// TestChannelSessionRegister_IssSub_StoredOnRecord verifies Fix A — that
// iss/sub identity claims supplied via the mod3 seat callback land on the
// kernel ChannelSessionRecord.
//
// This is the kernel half of the session-registration-unification spec: a
// seat-register that mod3 proxies in must produce a kernel record carrying
// the WHO (iss/sub) so downstream consumers (cog_list_sessions, the HUD,
// reconcilers) can see identity without a second mod3 round-trip.
func TestChannelSessionRegister_IssSub_StoredOnRecord(t *testing.T) {
	fm := newFakeMod3(t)
	s, front := newChannelServer(t, fm)

	body, _ := json.Marshal(map[string]any{
		"session_id":     "cs-identity-test",
		"participant_id": "channel-client::claude-code-channel",
		"iss":            "https://cogos.local",
		"sub":            "test-subject",
	})
	resp, err := http.Post(front.URL+"/v1/channel-sessions/register",
		"application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d; body: %s", resp.StatusCode, raw)
	}

	// Kernel record must carry the identity claims.
	rec, ok := s.channelSessionRegistry.Get("cs-identity-test")
	if !ok {
		t.Fatal("expected kernel registry to hold record after register")
	}
	if rec.Iss != "https://cogos.local" {
		t.Fatalf("expected Iss=https://cogos.local on record, got %q", rec.Iss)
	}
	if rec.Sub != "test-subject" {
		t.Fatalf("expected Sub=test-subject on record, got %q", rec.Sub)
	}
}

// TestChannelSessionRegister_IssSub_IdempotentOnReregister verifies that a
// re-register with the same session_id overwrites the kernel record, preserving
// the idempotency invariant required by the mod3 → kernel callback.
// (ChannelSessionRegistry.Put overwrites by session_id — safe to call on
// every seat re-register.)
func TestChannelSessionRegister_IssSub_IdempotentOnReregister(t *testing.T) {
	fm := newFakeMod3(t)
	s, front := newChannelServer(t, fm)

	registerWith := func(sub string) {
		body, _ := json.Marshal(map[string]any{
			"session_id":     "cs-idem-test",
			"participant_id": "channel-client::claude-code-channel",
			"iss":            "https://cogos.local",
			"sub":            sub,
		})
		resp, err := http.Post(front.URL+"/v1/channel-sessions/register",
			"application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("POST register (sub=%s): %v", sub, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 (sub=%s), got %d", sub, resp.StatusCode)
		}
	}

	registerWith("test-subject")
	rec, _ := s.channelSessionRegistry.Get("cs-idem-test")
	if rec.Sub != "test-subject" {
		t.Fatalf("expected Sub=test-subject after first register, got %q", rec.Sub)
	}

	// Re-register with updated sub — Put overwrites.
	registerWith("test-subject-updated")
	rec, _ = s.channelSessionRegistry.Get("cs-idem-test")
	if rec.Sub != "test-subject-updated" {
		t.Fatalf("expected Sub=test-subject-updated after re-register, got %q", rec.Sub)
	}
}
