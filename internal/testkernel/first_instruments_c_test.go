// first_instruments_c_test.go — Module C tests (First Instruments Stage-2).
//
// End-to-end HTTP surface test for GET /v1/kernel/rates. The bulk of Module
// C's arithmetic/echo-class/live-read tests live in
// internal/engine/rates_test.go (same package as KernelRates); this file
// covers the wiring through a real testkernel boot.
package testkernel_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/myrgic/cogos/internal/testkernel"
	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

func TestKernelRatesHTTP_FifteenRatiosWithEchoClass(t *testing.T) {
	reconcile.ResetProviders()
	defer reconcile.ResetProviders()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	k, err := testkernel.Boot(ctx, t, testkernel.WithPollInterval(1*time.Hour))
	if err != nil {
		t.Fatalf("testkernel.Boot: %v", err)
	}
	t.Cleanup(func() {
		if err := k.Stop(); err != nil {
			t.Errorf("testkernel.Stop: %v", err)
		}
	})

	resp, err := http.Get(k.Endpoint() + "/v1/kernel/rates")
	if err != nil {
		t.Fatalf("GET /v1/kernel/rates: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}

	var body struct {
		Constants []struct {
			Name          string  `json:"name"`
			Seconds       float64 `json:"seconds"`
			HasConfigKnob bool    `json:"has_config_knob"`
		} `json:"constants"`
		Ratios []struct {
			Numerator      string  `json:"numerator"`
			Denominator    string  `json:"denominator"`
			Ratio          float64 `json:"ratio"`
			EchoClass      string  `json:"echo_class"`
			SourceCitation string  `json:"source_citation"`
		} `json:"ratios"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(body.Constants) != 6 {
		t.Errorf("len(Constants) = %d; want 6", len(body.Constants))
	}
	if len(body.Ratios) != 15 {
		t.Errorf("len(Ratios) = %d; want 15", len(body.Ratios))
	}
	for _, r := range body.Ratios {
		if r.EchoClass != "deterministic_config_read" {
			t.Errorf("ratio %s/%s: echo_class = %q; want deterministic_config_read", r.Numerator, r.Denominator, r.EchoClass)
		}
	}
}

// TestKernelRatesHTTP_ReflectsDaemonPollInterval confirms the live HTTP
// surface reports the ACTUAL running daemon's PollInterval, not a static
// guess — booting with a 1h poll interval should show PollInterval=3600s.
func TestKernelRatesHTTP_ReflectsDaemonPollInterval(t *testing.T) {
	reconcile.ResetProviders()
	defer reconcile.ResetProviders()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	k, err := testkernel.Boot(ctx, t, testkernel.WithPollInterval(1*time.Hour))
	if err != nil {
		t.Fatalf("testkernel.Boot: %v", err)
	}
	t.Cleanup(func() {
		if err := k.Stop(); err != nil {
			t.Errorf("testkernel.Stop: %v", err)
		}
	})

	resp, err := http.Get(k.Endpoint() + "/v1/kernel/rates")
	if err != nil {
		t.Fatalf("GET /v1/kernel/rates: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		Constants []struct {
			Name    string  `json:"name"`
			Seconds float64 `json:"seconds"`
		} `json:"constants"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	found := false
	for _, c := range body.Constants {
		if c.Name == "PollInterval" {
			found = true
			if c.Seconds != 3600 {
				t.Errorf("PollInterval.Seconds = %v; want 3600 (1h)", c.Seconds)
			}
		}
	}
	if !found {
		t.Error("PollInterval constant not found in response")
	}
}
