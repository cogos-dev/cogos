package session_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/substrate/session"
)

// TestDefaultManagerConfig_Values verifies the documented defaults haven't
// drifted. These are operator-tunable; defaults are the documented contract.
func TestDefaultManagerConfig_Values(t *testing.T) {
	got := session.DefaultManagerConfig()
	if got.MaxTurnsBeforeRotation != 50 {
		t.Errorf("MaxTurnsBeforeRotation = %d, want 50", got.MaxTurnsBeforeRotation)
	}
	if got.MaxTokensBeforeRotation != 500_000 {
		t.Errorf("MaxTokensBeforeRotation = %d, want 500_000", got.MaxTokensBeforeRotation)
	}
	if got.IdleTimeout != 30*time.Minute {
		t.Errorf("IdleTimeout = %v, want 30m", got.IdleTimeout)
	}
}

// TestWorkingMemory_JSONRoundtrip verifies the wire format: serialize +
// deserialize must produce the same WorkingMemory (modulo time precision).
// WorkingMemory is the only type with JSON tags — it crosses persistence
// boundaries (writes that survive Claude session rotation).
func TestWorkingMemory_JSONRoundtrip(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	wm := session.WorkingMemory{
		ActiveTopics:    []string{"architecture", "substrate"},
		KeyDecisions:    []string{"adopt slug naming"},
		ActiveArtifacts: []string{"cog://architecture/adrs/eigen-as-universal-self-harness"},
		UserPreferences: map[string]string{"verbosity": "low"},
		Summary:         "Ongoing substrate-coupling work.",
		UpdatedAt:       now,
	}

	b, err := json.Marshal(wm)
	if err != nil {
		t.Fatal(err)
	}

	var got session.WorkingMemory
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Summary != wm.Summary {
		t.Errorf("Summary drift: got %q, want %q", got.Summary, wm.Summary)
	}
	if len(got.ActiveTopics) != 2 || got.ActiveTopics[0] != "architecture" {
		t.Errorf("ActiveTopics drift: %+v", got.ActiveTopics)
	}
	if !got.UpdatedAt.Equal(wm.UpdatedAt) {
		t.Errorf("UpdatedAt drift: %v vs %v", got.UpdatedAt, wm.UpdatedAt)
	}
}

// TestWorkingMemory_OmitEmpty verifies optional fields are dropped when
// empty. The .cog/state/conversations/ jsonl files use this struct; empty
// fields should not pollute the stored form.
func TestWorkingMemory_OmitEmpty(t *testing.T) {
	wm := session.WorkingMemory{UpdatedAt: time.Unix(0, 0).UTC()}
	b, err := json.Marshal(wm)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, field := range []string{
		`"active_topics"`,
		`"key_decisions"`,
		`"active_artifacts"`,
		`"user_preferences"`,
		`"summary"`,
	} {
		if contains(s, field) {
			t.Errorf("expected %s to be omitted; got: %s", field, s)
		}
	}
}

// TestStateHoldsRotationHistory verifies the State.History slice + Rotation
// shape compose: a State can hold its previous Claude sessions as past
// rotations.
func TestStateHoldsRotationHistory(t *testing.T) {
	state := session.State{
		ID:        "cog-session-1",
		TurnCount: 5,
		History: []session.Rotation{
			{
				ClaudeSessionID: "claude-1",
				StartedAt:       time.Unix(100, 0).UTC(),
				EndedAt:         time.Unix(200, 0).UTC(),
				Reason:          "pressure",
				TurnCount:       50,
			},
		},
	}
	if len(state.History) != 1 {
		t.Fatalf("History length wrong: %d", len(state.History))
	}
	if state.History[0].Reason != "pressure" {
		t.Errorf("Rotation.Reason = %q, want %q", state.History[0].Reason, "pressure")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
