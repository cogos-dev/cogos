// rates_test.go — Module C tests (First Instruments Stage-2).
package engine

import (
	"testing"
	"time"
)

// TestKernelRates_FifteenRatios confirms the complete C(6,2)=15-ratio table
// (IMPL-SPEC C1: all pairwise ratios, not a 6-of-15 subset).
func TestKernelRates_FifteenRatios(t *testing.T) {
	report := KernelRates(&Config{ConsolidationInterval: 3600, HeartbeatInterval: 60}, 30*time.Second)
	if len(report.Constants) != 6 {
		t.Fatalf("len(Constants) = %d; want 6", len(report.Constants))
	}
	if len(report.Ratios) != 15 {
		t.Fatalf("len(Ratios) = %d; want 15 (C(6,2))", len(report.Ratios))
	}
}

// TestKernelRates_ArithmeticAgainstKnownConfig confirms the ratio values are
// correct for a known configuration (kernel defaults).
func TestKernelRates_ArithmeticAgainstKnownConfig(t *testing.T) {
	report := KernelRates(&Config{ConsolidationInterval: 3600, HeartbeatInterval: 60}, 30*time.Second)

	find := func(num, den string) (RateRatio, bool) {
		for _, r := range report.Ratios {
			if r.Numerator == num && r.Denominator == den {
				return r, true
			}
		}
		return RateRatio{}, false
	}

	r, ok := find("ConsolidationInterval", "HeartbeatInterval")
	if !ok {
		t.Fatal("ConsolidationInterval/HeartbeatInterval ratio not found")
	}
	if r.Ratio != 60.0 {
		t.Errorf("ConsolidationInterval/HeartbeatInterval = %v; want 60.0 (3600/60)", r.Ratio)
	}
	if r.ClusterID != "sixty" {
		t.Errorf("ClusterID = %q; want %q", r.ClusterID, "sixty")
	}

	r2, ok := find("ConsolidationInterval", "ActiveWindow")
	if !ok {
		t.Fatal("ConsolidationInterval/ActiveWindow ratio not found")
	}
	if r2.Ratio != 6.0 {
		t.Errorf("ConsolidationInterval/ActiveWindow = %v; want 6.0 (3600/600)", r2.Ratio)
	}
	if r2.ClusterID != "six" {
		t.Errorf("ClusterID = %q; want %q", r2.ClusterID, "six")
	}

	r3, ok := find("HeartbeatInterval", "PollInterval")
	if !ok {
		t.Fatal("HeartbeatInterval/PollInterval ratio not found")
	}
	const want = 60.0 / 30.0
	if r3.Ratio != want {
		t.Errorf("HeartbeatInterval/PollInterval = %v; want %v (60/30)", r3.Ratio, want)
	}
}

// TestKernelRates_ReadsLiveConfig confirms KernelRates reads the LIVE
// configured values rather than hardcoded defaults: mutating the config
// between two calls changes the ratios that involve the mutated constants.
func TestKernelRates_ReadsLiveConfig(t *testing.T) {
	cfg := &Config{ConsolidationInterval: 3600, HeartbeatInterval: 60}
	before := KernelRates(cfg, 30*time.Second)

	cfg.ConsolidationInterval = 7200 // double it
	after := KernelRates(cfg, 30*time.Second)

	find := func(report RateReport, num, den string) float64 {
		for _, r := range report.Ratios {
			if r.Numerator == num && r.Denominator == den {
				return r.Ratio
			}
		}
		t.Fatalf("ratio %s/%s not found", num, den)
		return 0
	}

	beforeRatio := find(before, "ConsolidationInterval", "HeartbeatInterval")
	afterRatio := find(after, "ConsolidationInterval", "HeartbeatInterval")
	if beforeRatio == afterRatio {
		t.Errorf("ratio unchanged after mutating ConsolidationInterval: before=%v after=%v", beforeRatio, afterRatio)
	}
	if afterRatio != 120.0 {
		t.Errorf("ConsolidationInterval/HeartbeatInterval after doubling C = %v; want 120.0 (7200/60)", afterRatio)
	}

	// PollInterval is also a live-read argument (not baked into cfg); confirm
	// varying it changes ratios that involve PollInterval.
	pollBefore := KernelRates(cfg, 30*time.Second)
	pollAfter := KernelRates(cfg, 60*time.Second)
	pbRatio := find(pollBefore, "HeartbeatInterval", "PollInterval")
	paRatio := find(pollAfter, "HeartbeatInterval", "PollInterval")
	if pbRatio == paRatio {
		t.Errorf("ratio unchanged after varying pollInterval: before=%v after=%v", pbRatio, paRatio)
	}
}

// TestKernelRates_EchoClassAndClusterAnnotations confirms every ratio is
// tagged deterministic_config_read (IMPL-SPEC C2), and that the three
// "sixty" and three "six" cluster ratios are present and correctly tagged.
func TestKernelRates_EchoClassAndClusterAnnotations(t *testing.T) {
	report := KernelRates(&Config{ConsolidationInterval: 3600, HeartbeatInterval: 60}, 30*time.Second)

	sixtyCount, sixCount := 0, 0
	for _, r := range report.Ratios {
		if r.EchoClass != "deterministic_config_read" {
			t.Errorf("ratio %s/%s: EchoClass = %q; want %q", r.Numerator, r.Denominator, r.EchoClass, "deterministic_config_read")
		}
		if r.SourceCitation == "" {
			t.Errorf("ratio %s/%s: SourceCitation is empty", r.Numerator, r.Denominator)
		}
		switch r.ClusterID {
		case "sixty":
			sixtyCount++
		case "six":
			sixCount++
		}
	}
	if sixtyCount != 3 {
		t.Errorf("sixty-cluster count = %d; want 3", sixtyCount)
	}
	if sixCount != 3 {
		t.Errorf("six-cluster count = %d; want 3", sixCount)
	}
}

// TestKernelRates_HasConfigKnobFlags confirms the three constants with no
// live config knob are correctly flagged, matching the verified absence of
// an ActiveWindow/HandoffTTL/IdleRecheck config field on current main.
func TestKernelRates_HasConfigKnobFlags(t *testing.T) {
	report := KernelRates(&Config{ConsolidationInterval: 3600, HeartbeatInterval: 60}, 30*time.Second)

	want := map[string]bool{
		"ConsolidationInterval": true,
		"HeartbeatInterval":     true,
		"PollInterval":          true,
		"ActiveWindow":          false,
		"HandoffTTL":            false,
		"IdleRecheck":           false,
	}
	got := map[string]bool{}
	for _, c := range report.Constants {
		got[c.Name] = c.HasConfigKnob
	}
	for name, wantKnob := range want {
		if got[name] != wantKnob {
			t.Errorf("%s.HasConfigKnob = %v; want %v", name, got[name], wantKnob)
		}
	}
}

// TestKernelRates_SideEffectFree confirms KernelRates mutates no kernel
// state: the same cfg pointer produces identical ratios across repeated
// calls, and the config values themselves are unchanged after the call.
func TestKernelRates_SideEffectFree(t *testing.T) {
	cfg := &Config{ConsolidationInterval: 3600, HeartbeatInterval: 60}
	_ = KernelRates(cfg, 30*time.Second)

	if cfg.ConsolidationInterval != 3600 {
		t.Errorf("cfg.ConsolidationInterval mutated: got %d, want 3600", cfg.ConsolidationInterval)
	}
	if cfg.HeartbeatInterval != 60 {
		t.Errorf("cfg.HeartbeatInterval mutated: got %d, want 60", cfg.HeartbeatInterval)
	}

	r1 := KernelRates(cfg, 30*time.Second)
	r2 := KernelRates(cfg, 30*time.Second)
	if len(r1.Ratios) != len(r2.Ratios) {
		t.Fatalf("ratio count differs across repeated calls: %d vs %d", len(r1.Ratios), len(r2.Ratios))
	}
	for i := range r1.Ratios {
		if r1.Ratios[i].Ratio != r2.Ratios[i].Ratio {
			t.Errorf("ratio[%d] differs across repeated calls: %v vs %v", i, r1.Ratios[i].Ratio, r2.Ratios[i].Ratio)
		}
	}
}
