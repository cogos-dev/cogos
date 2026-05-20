package acp

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestSpike_OnePromptOneResponse is the architectural validation: spawn a
// real claude subprocess via stream-json, send one user message on stdin,
// drain stdout until result frame, assert the assistant produced text.
//
// Skipped automatically when `claude` is not on PATH so this doesn't break
// CI on machines without Claude Code installed.
func TestSpike_OnePromptOneResponse(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH; spike requires a local Claude Code install")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	sp, err := Spawn(ctx, SpawnOpts{
		// Fresh session — no --resume — so the spike doesn't accidentally
		// inject prompts into the operator's real session.
		// Keep ExtraArgs minimal; claude defaults are fine for the smoke.
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	if err := sp.Send(NewTextPrompt("Reply with exactly the four characters: PASS")); err != nil {
		t.Fatalf("send: %v", err)
	}
	// Close stdin so claude finishes its turn and exits cleanly.
	if err := sp.CloseInput(); err != nil {
		t.Logf("close-input warning (not fatal): %v", err)
	}

	var (
		sawSystemInit bool
		sawAssistant  bool
		sawResult     bool
		assistantText strings.Builder
		resultText    string
	)

	for ev := range sp.Events() {
		switch {
		case ev.System != nil:
			if ev.System.Subtype == "init" {
				sawSystemInit = true
				t.Logf("system.init session_id=%s", ev.System.SessionID)
			}
		case ev.Assistant != nil:
			sawAssistant = true
			for _, c := range ev.Assistant.Message.Content {
				if c.Type == "text" {
					assistantText.WriteString(c.Text)
				}
			}
		case ev.Stream != nil:
			// Mid-turn delta — parse just enough to log the per-token
			// arrival so we know streaming worked.
			var delta StreamDelta
			if err := json.Unmarshal(ev.Stream.Event, &delta); err == nil && delta.Delta.Text != "" {
				t.Logf("stream delta: %q", delta.Delta.Text)
			}
		case ev.Result != nil:
			sawResult = true
			resultText = ev.Result.Result
			t.Logf("result: is_error=%v duration_ms=%d num_turns=%d text=%q",
				ev.Result.IsError, ev.Result.DurationMs, ev.Result.NumTurns, ev.Result.Result)
		case ev.Unknown != nil:
			t.Logf("unknown event type=%q raw=%q", ev.Unknown.Type, truncate(string(ev.Unknown.Raw), 200))
		}
	}

	if err := sp.Wait(); err != nil {
		t.Logf("subprocess exited with: %v (may be fine — stdin closed)", err)
	}

	if !sawSystemInit {
		t.Errorf("expected system.init frame; never saw one")
	}
	if !sawAssistant {
		t.Errorf("expected at least one assistant frame; never saw one")
	}
	if !sawResult {
		t.Errorf("expected a result frame; never saw one")
	}

	final := strings.TrimSpace(assistantText.String())
	if final == "" {
		final = strings.TrimSpace(resultText)
	}
	if !strings.Contains(strings.ToUpper(final), "PASS") {
		t.Errorf("expected assistant reply to contain PASS, got %q", final)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
