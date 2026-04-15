package modality_test

import (
	"testing"
	"time"

	"github.com/cogos-dev/cogos/pkg/modality"
)

func TestSalience_OnEvent(t *testing.T) {
	s := modality.NewSalience()

	s.OnEvent(modality.EventInput, map[string]any{"channel": "voice-1"})
	s.OnEvent(modality.EventInput, map[string]any{"channel": "voice-1"})
	s.OnEvent(modality.EventError, map[string]any{"channel": "voice-1"})

	// Input events boost by 1.0 each, error by 1.5.
	score := s.Score("modality:modality.input:voice-1")
	if score != 2.0 {
		t.Errorf("input score = %f, want 2.0", score)
	}

	errScore := s.Score("modality:modality.error:voice-1")
	if errScore != 1.5 {
		t.Errorf("error score = %f, want 1.5", errScore)
	}
}

func TestSalience_TopN(t *testing.T) {
	s := modality.NewSalience()

	// Create entries with different scores.
	for i := 0; i < 5; i++ {
		s.OnEvent(modality.EventInput, map[string]any{"channel": "ch-a"})
	}
	s.OnEvent(modality.EventError, map[string]any{"channel": "ch-b"})

	top := s.TopN(1)
	if len(top) != 1 {
		t.Fatalf("TopN(1) returned %d entries, want 1", len(top))
	}
	// 5 inputs at 1.0 each = 5.0, which is more than 1 error at 1.5.
	if top[0].Score != 5.0 {
		t.Errorf("top score = %f, want 5.0", top[0].Score)
	}
}

func TestSalience_Decay(t *testing.T) {
	s := modality.NewSalience()

	s.OnEvent(modality.EventInput, map[string]any{"channel": "ch-1"})

	// Decay after one half-life should halve the score.
	now := time.Now().Add(modality.DefaultDecayHalfLife)
	s.Decay(now, modality.DefaultDecayHalfLife)

	score := s.Score("modality:modality.input:ch-1")
	if score < 0.45 || score > 0.55 {
		t.Errorf("score after one half-life = %f, want ~0.5", score)
	}
}

func TestSalience_DecayPrune(t *testing.T) {
	s := modality.NewSalience()

	s.OnEvent(modality.EventGate, map[string]any{"channel": "ch-1"}) // boost = 0.3

	// Decay far into the future to prune.
	farFuture := time.Now().Add(24 * time.Hour)
	s.Decay(farFuture, modality.DefaultDecayHalfLife)

	snap := s.Snapshot()
	if len(snap) != 0 {
		t.Errorf("expected empty snapshot after heavy decay, got %d entries", len(snap))
	}
}

func TestSalience_Snapshot(t *testing.T) {
	s := modality.NewSalience()
	s.OnEvent(modality.EventOutput, map[string]any{"channel": "ch-x"})

	snap := s.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot has %d entries, want 1", len(snap))
	}
	if snap["modality:modality.output:ch-x"] != 0.8 {
		t.Errorf("score = %f, want 0.8", snap["modality:modality.output:ch-x"])
	}
}

func TestSalience_UnknownChannel(t *testing.T) {
	s := modality.NewSalience()
	s.OnEvent(modality.EventInput, map[string]any{}) // no channel

	score := s.Score("modality:modality.input:unknown")
	if score != 1.0 {
		t.Errorf("score = %f, want 1.0 (unknown channel fallback)", score)
	}
}
