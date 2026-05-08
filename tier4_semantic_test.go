package main

import (
	"math"
	"strings"
	"testing"

	"github.com/myrgic/cogos/sdk/constellation"
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
	//   pressureScale = 2*irisPressure - irisPressure² (isometry defect from SRC covariance)
	//   fullThreshold = topScore * (0.6 + 0.4 * pressureScale)
	//   sectionThreshold = topScore * (0.3 + 0.7 * pressureScale)
	//
	// The isometry defect δ(p) = 2p - p² derives from ρ²(r) = (2/3)·e^(-2r)
	// under the pressure-delay mapping r = -ln(1-p). It equals 1-(1-p)².
	// More aggressive than p² at moderate pressure (front-loaded fidelity loss).

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
			// pressureScale = 2*0.1 - 0.01 = 0.19
			// full  = 1.0 * (0.6 + 0.4*0.19) = 0.676
			// section = 1.0 * (0.3 + 0.7*0.19) = 0.433
			wantFull:    0.676,
			wantSection: 0.433,
		},
		{
			name:         "high pressure (0.9)",
			topScore:     1.0,
			irisPressure: 0.9,
			// pressureScale = 2*0.9 - 0.81 = 0.99
			// full  = 1.0 * (0.6 + 0.4*0.99) = 0.996
			// section = 1.0 * (0.3 + 0.7*0.99) = 0.993
			wantFull:    0.996,
			wantSection: 0.993,
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
			// pressureScale = 2*1.0 - 1.0 = 1.0
			// full  = 1.0 * (0.6 + 0.4) = 1.0
			// section = 1.0 * (0.3 + 0.7) = 1.0
			wantFull:    1.0,
			wantSection: 1.0,
		},
		{
			name:         "scaled top score (2.5) with mid pressure (0.5)",
			topScore:     2.5,
			irisPressure: 0.5,
			// pressureScale = 2*0.5 - 0.25 = 0.75
			// full  = 2.5 * (0.6 + 0.4*0.75) = 2.5 * 0.9 = 2.25
			// section = 2.5 * (0.3 + 0.7*0.75) = 2.5 * 0.825 = 2.0625
			wantFull:    2.25,
			wantSection: 2.0625,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Replicate the exact formula from QueryConstellationWithIris
			// Isometry defect: δ(p) = 2p - p²
			pressureScale := 2*tc.irisPressure - tc.irisPressure*tc.irisPressure
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

			// Key invariant: pressureScale in [0,1] for irisPressure in [0,1]
			if pressureScale < 0 || pressureScale > 1 {
				t.Errorf("pressureScale should be in [0,1], got %f", pressureScale)
			}
		})
	}

	// Verify monotonicity: increasing pressure produces increasing thresholds
	// 2p - p² is monotonically increasing on [0,1] (derivative = 2-2p ≥ 0)
	topScore := 1.0
	prevFull := 0.0
	prevSection := 0.0
	for p := 0.0; p <= 1.0; p += 0.1 {
		ps := 2*p - p*p
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

func TestResolutionTierSelection(t *testing.T) {
	// Tests the score-based resolution tier logic from QueryConstellationWithIris.
	// Given a set of scored candidates and threshold parameters, verify that each
	// candidate renders at the correct resolution: full, section, or metadata-only.

	makeNode := func(title, content string) constellation.Node {
		return constellation.Node{
			Title:   title,
			Type:    "cogdoc",
			Sector:  "semantic",
			Content: content,
		}
	}

	// Simulate the threshold computation from QueryConstellationWithIris
	const (
		fullBase    = 0.6
		sectionBase = 0.3
	)

	cases := []struct {
		name         string
		pressure     float64
		topScore     float64
		scores       []float64 // CombinedScore for each candidate
		wantTiers    []string  // "full", "section", or "metadata"
	}{
		{
			name:      "low pressure — most candidates at full resolution",
			pressure:  0.1,
			topScore:  1.0,
			scores:    []float64{1.0, 0.8, 0.7, 0.5, 0.3},
			// pressureScale = 2*0.1 - 0.01 = 0.19
			// fullThreshold = 1.0 * (0.6 + 0.4*0.19) = 0.676
			// sectionThreshold = 1.0 * (0.3 + 0.7*0.19) = 0.433
			wantTiers: []string{"full", "full", "full", "section", "metadata"},
		},
		{
			name:      "high pressure — only top candidate at full",
			pressure:  0.9,
			topScore:  1.0,
			scores:    []float64{1.0, 0.99, 0.95, 0.8, 0.5},
			// pressureScale = 2*0.9 - 0.81 = 0.99
			// fullThreshold = 1.0 * (0.6 + 0.4*0.99) = 0.996
			// sectionThreshold = 1.0 * (0.3 + 0.7*0.99) = 0.993
			wantTiers: []string{"full", "metadata", "metadata", "metadata", "metadata"},
		},
		{
			name:      "zero pressure — lowest thresholds",
			pressure:  0.0,
			topScore:  1.0,
			scores:    []float64{1.0, 0.65, 0.55, 0.35, 0.25},
			// pressureScale = 0
			// fullThreshold = 0.6
			// sectionThreshold = 0.3
			wantTiers: []string{"full", "full", "section", "section", "metadata"},
		},
		{
			name:      "tau2 boundary (0.75) — aggressive culling",
			pressure:  0.75,
			topScore:  1.0,
			scores:    []float64{1.0, 0.98, 0.96, 0.90, 0.70},
			// pressureScale = 2*0.75 - 0.5625 = 0.9375
			// fullThreshold = 1.0 * (0.6 + 0.4*0.9375) = 0.975
			// sectionThreshold = 1.0 * (0.3 + 0.7*0.9375) = 0.95625
			wantTiers: []string{"full", "full", "section", "metadata", "metadata"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pressureScale := 2*tc.pressure - tc.pressure*tc.pressure
			fullThreshold := tc.topScore * (fullBase + (1.0-fullBase)*pressureScale)
			sectionThreshold := tc.topScore * (sectionBase + (1.0-sectionBase)*pressureScale)

			for i, score := range tc.scores {
				node := makeNode(
					"Doc "+strings.Repeat("A", i+1),
					"# Section One\nFirst section content.\n\n## Section Two\nSecond section content.",
				)

				var tier string
				var rendered string
				switch {
				case score >= fullThreshold:
					tier = "full"
					rendered = formatNodeWithConfig(node, 2000)
				case score >= sectionThreshold:
					tier = "section"
					rendered = formatNodeSection(node, 1000)
				default:
					tier = "metadata"
					rendered = formatNodeMetadataOnly(node)
				}

				if tier != tc.wantTiers[i] {
					t.Errorf("candidate %d (score=%.2f): got tier %q, want %q (fullThreshold=%.4f sectionThreshold=%.4f)",
						i, score, tier, tc.wantTiers[i], fullThreshold, sectionThreshold)
				}

				// Verify rendered content has expected characteristics
				if !strings.Contains(rendered, node.Title) {
					t.Errorf("candidate %d: rendered output missing title %q", i, node.Title)
				}

				switch tier {
				case "full":
					if !strings.Contains(rendered, "First section content") {
						t.Errorf("candidate %d: full render should include content body", i)
					}
				case "section":
					// Section format includes metadata and longest section
					if !strings.Contains(rendered, "Type: cogdoc") {
						t.Errorf("candidate %d: section render should include metadata", i)
					}
				case "metadata":
					// Metadata-only should NOT include content body
					if strings.Contains(rendered, "First section content") {
						t.Errorf("candidate %d: metadata render should not include content body", i)
					}
				}
			}
		})
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
	//
	// irisPressure must be <= 0.05 so the whitespace-compression branch inside
	// QueryConstellationWithIris's heuristic fallback is skipped.  Using a
	// non-zero pressure (e.g. 0.5) causes compression to run, producing output
	// that diverges from QueryConstellation whenever the constellation DB
	// returns actual content — making the test environment-dependent and flaky.
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
			// irisPressure=0.0: no iris signal, so heuristic fallback skips whitespace
			// compression (pressure <= 0.05 guard) and the two functions are identical.
			irisResult, irisErr := QueryConstellationWithIris(tmpDir, tc.anchor, tc.goal, 5000, 0.0)
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
