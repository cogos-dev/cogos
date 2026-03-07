package main

import (
	"math"
	"strings"
	"testing"

	"github.com/cogos-dev/cogos/sdk/constellation"
)

func TestQueryConstellationNoAnchor(t *testing.T) {
	// With no anchor and no goal, should return empty
	result, err := QueryConstellation(t.TempDir(), "", "", 5000)
	if err != nil {
		t.Fatalf("QueryConstellation error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty result for no anchor/goal, got %q", result)
	}
}

func TestFormatNodeWithConfig(t *testing.T) {
	node := constellation.Node{
		Title:   "Test Document",
		Type:    "cogdoc",
		Sector:  "semantic",
		Status:  "active",
		Content: "This is the test document content for TAA pipeline testing.",
	}

	result := formatNodeWithConfig(node, 2000)

	if !strings.Contains(result, "## Test Document") {
		t.Error("missing title header")
	}
	if !strings.Contains(result, "Type: cogdoc") {
		t.Error("missing type metadata")
	}
	if !strings.Contains(result, "Sector: semantic") {
		t.Error("missing sector metadata")
	}
	if !strings.Contains(result, "Status: active") {
		t.Error("missing status metadata")
	}
	if !strings.Contains(result, "test document content") {
		t.Error("missing content")
	}
}

func TestFormatNodeWithConfigTruncation(t *testing.T) {
	longContent := strings.Repeat("word ", 1000)
	node := constellation.Node{
		Title:   "Long Document",
		Type:    "cogdoc",
		Content: longContent,
	}

	result := formatNodeWithConfig(node, 100)

	if !strings.Contains(result, "...(truncated)") {
		t.Error("long content should be truncated")
	}
	if len(result) > 500 { // generous upper bound
		t.Errorf("truncated result too long: %d chars", len(result))
	}
}

func TestFormatNodeWithConfigDefaultTruncation(t *testing.T) {
	longContent := strings.Repeat("x", 3000)
	node := constellation.Node{
		Title:   "Default Truncation Test",
		Type:    "cogdoc",
		Content: longContent,
	}

	// maxContentChars=0 should use default of 2000
	result := formatNodeWithConfig(node, 0)

	if !strings.Contains(result, "...(truncated)") {
		t.Error("should truncate with default limit")
	}
}

func TestFormatNodeWithConfigEmptyContent(t *testing.T) {
	node := constellation.Node{
		Title: "Empty Document",
		Type:  "cogdoc",
	}

	result := formatNodeWithConfig(node, 2000)

	if !strings.Contains(result, "## Empty Document") {
		t.Error("missing title for empty content node")
	}
}

func TestFormatNodeWithConfigNoSector(t *testing.T) {
	node := constellation.Node{
		Title:   "No Sector",
		Type:    "cogdoc",
		Content: "content",
	}

	result := formatNodeWithConfig(node, 2000)

	// Should not have "Sector:" in metadata
	if strings.Contains(result, "Sector:") {
		t.Error("should not include empty sector")
	}
}

func TestQueryConstellationWithIris_NoAnchor(t *testing.T) {
	// With no anchor and no goal, should return empty string (same as QueryConstellation behavior).
	// The function returns early before attempting any constellation query.
	result, err := QueryConstellationWithIris(t.TempDir(), "", "", 5000, 0.5)
	if err != nil {
		t.Fatalf("QueryConstellationWithIris error: %v", err)
	}
	if result != "" {
		t.Errorf("expected empty result for no anchor/goal, got %q", result)
	}
}

func TestQueryConstellationWithIris_ScoreThresholdScaling(t *testing.T) {
	// Verify the iris threshold math used inside QueryConstellationWithIris:
	//   pressureScale = irisPressure^2
	//   fullThreshold = topScore * (0.6 + 0.4 * pressureScale)
	//   sectionThreshold = topScore * (0.3 + 0.7 * pressureScale)
	//
	// This tests the formula directly since the thresholds are computed
	// internally and not exposed. We replicate the exact arithmetic from
	// the function body and verify key properties.

	const (
		fullBase    = 0.6
		sectionBase = 0.3
		tolerance   = 1e-9
	)

	cases := []struct {
		name            string
		topScore        float64
		irisPressure    float64
		wantFull        float64
		wantSection     float64
	}{
		{
			name:         "low pressure (0.1)",
			topScore:     1.0,
			irisPressure: 0.1,
			// pressureScale = 0.01
			// full  = 1.0 * (0.6 + 0.4*0.01) = 0.604
			// section = 1.0 * (0.3 + 0.7*0.01) = 0.307
			wantFull:    0.604,
			wantSection: 0.307,
		},
		{
			name:         "high pressure (0.9)",
			topScore:     1.0,
			irisPressure: 0.9,
			// pressureScale = 0.81
			// full  = 1.0 * (0.6 + 0.4*0.81) = 0.924
			// section = 1.0 * (0.3 + 0.7*0.81) = 0.867
			wantFull:    0.924,
			wantSection: 0.867,
		},
		{
			name:         "zero pressure",
			topScore:     1.0,
			irisPressure: 0.0,
			// pressureScale = 0.0
			// full  = 1.0 * (0.6 + 0.0) = 0.6
			// section = 1.0 * (0.3 + 0.0) = 0.3
			wantFull:    0.6,
			wantSection: 0.3,
		},
		{
			name:         "max pressure (1.0)",
			topScore:     1.0,
			irisPressure: 1.0,
			// pressureScale = 1.0
			// full  = 1.0 * (0.6 + 0.4) = 1.0
			// section = 1.0 * (0.3 + 0.7) = 1.0
			wantFull:    1.0,
			wantSection: 1.0,
		},
		{
			name:         "scaled top score (2.5) with mid pressure (0.5)",
			topScore:     2.5,
			irisPressure: 0.5,
			// pressureScale = 0.25
			// full  = 2.5 * (0.6 + 0.4*0.25) = 2.5 * 0.7 = 1.75
			// section = 2.5 * (0.3 + 0.7*0.25) = 2.5 * 0.475 = 1.1875
			wantFull:    1.75,
			wantSection: 1.1875,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Replicate the exact formula from QueryConstellationWithIris
			pressureScale := tc.irisPressure * tc.irisPressure
			fullThreshold := tc.topScore * (fullBase + (1.0-fullBase)*pressureScale)
			sectionThreshold := tc.topScore * (sectionBase + (1.0-sectionBase)*pressureScale)

			if math.Abs(fullThreshold-tc.wantFull) > tolerance {
				t.Errorf("fullThreshold: got %f, want %f", fullThreshold, tc.wantFull)
			}
			if math.Abs(sectionThreshold-tc.wantSection) > tolerance {
				t.Errorf("sectionThreshold: got %f, want %f", sectionThreshold, tc.wantSection)
			}

			// Key invariant: at any pressure, full threshold >= section threshold
			if fullThreshold < sectionThreshold {
				t.Errorf("fullThreshold (%f) should be >= sectionThreshold (%f)", fullThreshold, sectionThreshold)
			}

			// Key invariant: higher pressure produces higher thresholds
			// (verified structurally — pressureScale is monotonically increasing)
			if pressureScale < 0 || pressureScale > 1 {
				t.Errorf("pressureScale should be in [0,1], got %f", pressureScale)
			}
		})
	}

	// Verify monotonicity: increasing pressure produces increasing thresholds
	topScore := 1.0
	prevFull := 0.0
	prevSection := 0.0
	for p := 0.0; p <= 1.0; p += 0.1 {
		ps := p * p
		full := topScore * (fullBase + (1.0-fullBase)*ps)
		section := topScore * (sectionBase + (1.0-sectionBase)*ps)
		if full < prevFull-tolerance {
			t.Errorf("fullThreshold not monotonic at pressure=%.1f: %f < %f", p, full, prevFull)
		}
		if section < prevSection-tolerance {
			t.Errorf("sectionThreshold not monotonic at pressure=%.1f: %f < %f", p, section, prevSection)
		}
		prevFull = full
		prevSection = section
	}
}

func TestQueryConstellationWithIris_FallbackToStandard(t *testing.T) {
	// When embedding is not enabled (the default), QueryConstellationWithIris
	// should fall back to standard QueryConstellation behavior.
	//
	// Both functions share the same getConstellation() call, so their behavior
	// is equivalent when the constellation DB has the same availability.
	// We verify that the iris variant produces the same result+error pair as
	// the standard variant for several inputs.
	tmpDir := t.TempDir()

	// Reset cached config so LoadTAAConfig picks up defaults (embedding disabled)
	taaConfigMutex.Lock()
	cachedTAAConfig = nil
	taaConfigMutex.Unlock()

	cases := []struct {
		name   string
		anchor string
		goal   string
	}{
		{"both empty", "", ""},
		{"anchor only", "test anchor", ""},
		{"goal only", "", "test goal"},
		{"both set", "test anchor", "test goal"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			irisResult, irisErr := QueryConstellationWithIris(tmpDir, tc.anchor, tc.goal, 5000, 0.5)
			stdResult, stdErr := QueryConstellation(tmpDir, tc.anchor, tc.goal, 5000)

			// Both should produce the same result (either both empty or both with content)
			if irisResult != stdResult {
				t.Errorf("iris result %q differs from standard result %q", irisResult, stdResult)
			}

			// Both should produce the same error status (both nil or both non-nil)
			if (irisErr == nil) != (stdErr == nil) {
				t.Errorf("error mismatch: iris=%v, standard=%v", irisErr, stdErr)
			}
		})
	}
}
