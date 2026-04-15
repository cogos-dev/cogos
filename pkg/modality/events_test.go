package modality_test

import (
	"testing"

	"github.com/cogos-dev/cogos/pkg/modality"
)

func TestRequireFields_AllPresent(t *testing.T) {
	err := modality.RequireFields("test.event",
		"modality", "voice",
		"channel", "ch-1",
	)
	if err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestRequireFields_MissingField(t *testing.T) {
	err := modality.RequireFields("test.event",
		"modality", "voice",
		"channel", "",
	)
	if err == nil {
		t.Fatal("expected error for missing channel")
	}
	want := "test.event: channel is required"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestRequireFields_FirstMissing(t *testing.T) {
	err := modality.RequireFields("test.event",
		"modality", "",
		"channel", "ch-1",
	)
	if err == nil {
		t.Fatal("expected error for missing modality")
	}
	if err.Error() != "test.event: modality is required" {
		t.Errorf("error = %q", err.Error())
	}
}
