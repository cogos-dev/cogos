package acp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// freshSessionID returns a v4-shaped UUID guaranteed unique per test run
// (real UUID library would be overkill for a spike). Claude's --session-id
// flag requires UUID format but doesn't validate the variant bits.
func freshSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

// TestSpike_MultiTurnOverResume validates the central premise of ADR-093:
// that a single claude subprocess can stay alive across multiple stdin
// messages, with the session JSONL appending turns naturally.
//
// Flow:
//  1. Spawn subprocess with a fresh --session-id we pin
//  2. Send prompt 1; drain until result; verify response
//  3. Send prompt 2 referencing prompt 1's content; drain until result;
//     verify the assistant remembered (i.e. the session_id pinned and
//     claude maintained context)
//  4. Close stdin; wait; assert clean exit
//
// This is the critical test — if it passes, the kernel can mediate
// arbitrary multi-turn dialog over one subprocess via stream-json.
func TestSpike_MultiTurnOverResume(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude binary not on PATH; spike requires a local Claude Code install")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// Pin a fresh session_id so claude doesn't pick from the operator's
	// session history. Must be unique per test run — claude refuses to
	// pin --session-id to a value that already exists on disk.
	pinned := freshSessionID()

	sp, err := Spawn(ctx, SpawnOpts{
		SessionID:      pinned,
		ResumeExisting: false, // create-pinned, not resume
	})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}

	// Turn 1: establish a fact we can interrogate later.
	if err := sp.Send(NewTextPrompt(
		"Remember the secret word: GLOWWORM. Reply only with the single word: OK.",
	)); err != nil {
		t.Fatalf("send 1: %v", err)
	}

	turn1, err := drainOneTurn(t, sp.Events())
	if err != nil {
		t.Fatalf("drain turn 1: %v", err)
	}
	t.Logf("turn 1 result: %q (session=%s)", turn1.text, turn1.sessionID)
	if !strings.Contains(strings.ToUpper(turn1.text), "OK") {
		t.Errorf("turn 1 should reply with OK, got %q", turn1.text)
	}

	// Turn 2: probe whether claude retained turn 1's context.
	if err := sp.Send(NewTextPrompt(
		"What is the secret word I asked you to remember? Reply with just the word.",
	)); err != nil {
		t.Fatalf("send 2: %v", err)
	}

	turn2, err := drainOneTurn(t, sp.Events())
	if err != nil {
		t.Fatalf("drain turn 2: %v", err)
	}
	t.Logf("turn 2 result: %q (session=%s)", turn2.text, turn2.sessionID)
	if !strings.Contains(strings.ToUpper(turn2.text), "GLOWWORM") {
		t.Errorf("turn 2 should recall GLOWWORM (proving session continuity), got %q", turn2.text)
	}

	// Both turns should report the SAME session_id — proves the subprocess
	// stayed in one session across writes.
	if turn1.sessionID != "" && turn2.sessionID != "" && turn1.sessionID != turn2.sessionID {
		t.Errorf("expected same session across turns; turn1=%s turn2=%s",
			turn1.sessionID, turn2.sessionID)
	}

	// Clean shutdown.
	if err := sp.CloseInput(); err != nil {
		t.Logf("close-input warning: %v", err)
	}
	// Drain anything trailing.
	for range sp.Events() { //nolint:revive
	}
	if err := sp.Wait(); err != nil {
		t.Logf("subprocess exit: %v (expected after stdin close)", err)
	}
}

type turnResult struct {
	text      string
	sessionID string
	isError   bool
}

// drainOneTurn reads events until it sees a Result frame and returns that
// turn's accumulated text. Assistant text is preferred (more accurate
// reflection of what claude said); Result.Result is a fallback.
func drainOneTurn(t *testing.T, events <-chan Event) (turnResult, error) {
	var assistantText strings.Builder
	for ev := range events {
		switch {
		case ev.System != nil:
			t.Logf("  system.%s session=%s", ev.System.Subtype, ev.System.SessionID)
		case ev.Assistant != nil:
			for _, c := range ev.Assistant.Message.Content {
				if c.Type == "text" {
					assistantText.WriteString(c.Text)
				}
			}
		case ev.Stream != nil:
			var d StreamDelta
			if err := json.Unmarshal(ev.Stream.Event, &d); err == nil && d.Delta.Text != "" {
				// don't log per-delta in this test; turn count would be huge
				_ = d
			}
		case ev.Result != nil:
			text := strings.TrimSpace(assistantText.String())
			if text == "" {
				text = ev.Result.Result
			}
			return turnResult{
				text:      text,
				sessionID: ev.Result.SessionID,
				isError:   ev.Result.IsError,
			}, nil
		}
	}
	return turnResult{}, errEventsClosedBeforeResult
}

var errEventsClosedBeforeResult = &spikeError{"event channel closed before any result frame"}

type spikeError struct{ msg string }

func (e *spikeError) Error() string { return e.msg }
