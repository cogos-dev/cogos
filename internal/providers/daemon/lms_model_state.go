// lms_model_state.go — daemon-side health provider for the LM Studio
// model/context-state reconciler.
//
// lmsModelStateProvider implements reconcile.Reconcilable for the daemon's
// proprioception block. It scans providers(.local).yaml for any dispatch backend
// carrying an options.model_state block with manage:true. For each managed
// backend it performs a read-only GET /api/v0/models probe and reports whether
// the declared model is loaded at the declared context length.
//
// If no backend opts in (no model_state.manage:true), Health() returns
// HealthSuspended (NOT HealthMissing) — this is an opt-in feature, not a missing
// requirement. This mirrors mlx_inference.go's precedent exactly.
//
// GUARDRAILS: this daemon stub is Health()-only and strictly read-only. It never
// loads or unloads a model. Full plan/apply lives in the engine-layer
// LMSModelStateProvider (constructed by BuildRouter). The daemon only exercises
// Health() through the proprioception block.
//
// Registration: init() registers this provider as "lms-model-state" so the
// providers/all import chain triggers it automatically.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

func init() {
	reconcile.RegisterProvider("lms-model-state", &lmsModelStateProvider{stubMethods: stubMethods{name: "lms-model-state"}})
}

// lmsModelStateProvider is the daemon-side Reconcilable for LM Studio model-state.
type lmsModelStateProvider struct {
	stubMethods
}

func (p *lmsModelStateProvider) Type() string { return "lms-model-state" }

// Health inspects all opted-in backends and returns the aggregate status.
// Called by buildHealthBlock on every foveated context generation; kept cheap.
func (p *lmsModelStateProvider) Health() reconcile.ResourceStatus {
	root, bad := resolveRoot()
	if bad != nil {
		return *bad
	}

	entries := loadModelStateEntries(root)
	if len(entries) == 0 {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthSuspended,
			Operation: reconcile.OperationIdle,
			Message:   "no backend declares options.model_state.manage:true — opt-in feature",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	var issues []string
	for _, e := range entries {
		if err := probeModelStateEntry(ctx, e); err != nil {
			issues = append(issues, fmt.Sprintf("%s: %v", e.name, err))
		}
	}

	if len(issues) > 0 {
		// Unreachable/off-LAN and drift both land here as OutOfSync. The engine
		// provider distinguishes Suspended-vs-Degraded on the live path; the
		// daemon stub only reports a coarse aggregate for proprioception.
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusOutOfSync,
			Health:    reconcile.HealthDegraded,
			Operation: reconcile.OperationIdle,
			Message:   strings.Join(issues, "; "),
		}
	}

	noun := "backend"
	if len(entries) != 1 {
		noun = "backends"
	}
	return reconcile.ResourceStatus{
		Sync:      reconcile.SyncStatusSynced,
		Health:    reconcile.HealthHealthy,
		Operation: reconcile.OperationIdle,
		Message:   fmt.Sprintf("%d model_state %s at target", len(entries), noun),
	}
}

// modelStateEntry is the minimal config needed for a daemon-side health probe.
type modelStateEntry struct {
	name          string
	endpoint      string
	apiKeyEnv     string
	model         string
	contextLength int
}

// loadModelStateEntries reads providers(.local).yaml and returns entries whose
// options.model_state.manage is true. providers.local.yaml overlays the base.
func loadModelStateEntries(root string) []modelStateEntry {
	var result []modelStateEntry

	for _, filename := range []string{"providers.yaml", "providers.local.yaml"} {
		path := filepath.Join(root, ".cog", "config", filename)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		entries := parseModelStateEntriesFromYAML(data)
		existing := make(map[string]int, len(result))
		for i, e := range result {
			existing[e.name] = i
		}
		for _, e := range entries {
			if idx, ok := existing[e.name]; ok {
				result[idx] = e
			} else {
				result = append(result, e)
				existing[e.name] = len(result) - 1
			}
		}
	}
	return result
}

// msProviderFileCfg is the top-level shape used for the daemon-side model_state
// parse. Only fields relevant to the probe are decoded.
type msProviderFileCfg struct {
	Providers map[string]msProviderEntryCfg `yaml:"providers"`
}

type msProviderEntryCfg struct {
	Endpoint  string       `yaml:"endpoint"`
	APIKeyEnv string       `yaml:"api_key_env"`
	Options   msOptionsCfg `yaml:"options"`
}

type msOptionsCfg struct {
	ModelState msModelStateCfg `yaml:"model_state"`
}

type msModelStateCfg struct {
	Manage        bool   `yaml:"manage"`
	Model         string `yaml:"model"`
	ContextLength int    `yaml:"context_length"`
}

// parseModelStateEntriesFromYAML returns entries with model_state.manage:true.
func parseModelStateEntriesFromYAML(data []byte) []modelStateEntry {
	var cfg msProviderFileCfg
	if err := yaml.Unmarshal(data, &cfg); err != nil || cfg.Providers == nil {
		return nil
	}
	var entries []modelStateEntry
	for name, p := range cfg.Providers {
		if !p.Options.ModelState.Manage {
			continue
		}
		entries = append(entries, modelStateEntry{
			name:          name,
			endpoint:      p.Endpoint,
			apiKeyEnv:     p.APIKeyEnv,
			model:         p.Options.ModelState.Model,
			contextLength: p.Options.ModelState.ContextLength,
		})
	}
	return entries
}

// probeModelStateEntry checks one backend: read /api/v0/models and confirm the
// declared model is loaded at the declared context length. Read-only.
func probeModelStateEntry(ctx context.Context, e modelStateEntry) error {
	base := strings.TrimRight(e.endpoint, "/")
	if base == "" {
		base = "http://localhost:1234"
	}
	url := base + "/api/v0/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if e.apiKeyEnv != "" {
		if tok := os.Getenv(e.apiKeyEnv); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("unreachable: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/api/v0/models returned %d", resp.StatusCode)
	}

	var out struct {
		Data []struct {
			ID                  string `json:"id"`
			State               string `json:"state"`
			LoadedContextLength *int   `json:"loaded_context_length"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	for _, m := range out.Data {
		if m.ID != e.model && !strings.HasPrefix(m.ID, e.model) && !strings.HasPrefix(e.model, m.ID) {
			continue
		}
		if m.State != "loaded" {
			return fmt.Errorf("model %q state=%q (want loaded)", e.model, m.State)
		}
		if e.contextLength > 0 && (m.LoadedContextLength == nil || *m.LoadedContextLength != e.contextLength) {
			got := "null"
			if m.LoadedContextLength != nil {
				got = fmt.Sprintf("%d", *m.LoadedContextLength)
			}
			return fmt.Errorf("model %q loaded at context %s (want %d)", e.model, got, e.contextLength)
		}
		return nil
	}
	return fmt.Errorf("model %q not present", e.model)
}
