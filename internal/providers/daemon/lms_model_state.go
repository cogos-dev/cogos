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
	"os/exec"
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
	var anyProgressing bool
	for _, e := range entries {
		progressing, err := probeModelStateEntry(ctx, e)
		if err != nil {
			issues = append(issues, fmt.Sprintf("%s: %v", e.name, err))
		} else if progressing {
			anyProgressing = true
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

	if anyProgressing {
		// A managed model is mid-load (state=="loading"). That is Progressing, not
		// a problem — a high-context load can take minutes. Matches the engine
		// provider's loading→Progressing mapping so proprioception doesn't false-alarm.
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthProgressing,
			Operation: reconcile.OperationWaiting,
			Message:   "model load in progress",
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
	parallel      int // 0 ⇒ don't watch parallel

	// local and lmsCLIPath gate the parallel probe (see checkParallelDrift):
	// `lms ps --json` is a local-only CLI (no --host flag), so parallel drift is
	// only observable on a localhost endpoint. Computed in loadModelStateEntries
	// from the endpoint host; test-constructed modelStateEntry{} literals may set
	// these directly for injection (no test-only global needed).
	local      bool
	lmsCLIPath string
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

	lmsCLI := resolveLmsCLIPath()
	for i := range result {
		result[i].local = isLocalHostEndpoint(result[i].endpoint)
		result[i].lmsCLIPath = lmsCLI
	}
	return result
}

// isLocalHostEndpoint reports whether endpoint's host is loopback (or empty,
// which probeModelStateEntry defaults to localhost). Duplicates the small
// loopback check in internal/engine/provider_lms_model_state.go's isLocalHost
// rather than cross-importing engine from daemon.
func isLocalHostEndpoint(endpoint string) bool {
	host := endpoint
	if idx := strings.Index(host, "://"); idx >= 0 {
		host = host[idx+3:]
	}
	if idx := strings.Index(host, "/"); idx >= 0 {
		host = host[:idx]
	}
	if idx := strings.LastIndex(host, ":"); idx >= 0 {
		host = host[:idx]
	}
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]", "":
		return true
	}
	return false
}

// resolveLmsCLIPath returns the local lms CLI fast-path binary path,
// best-effort. An empty result disables the parallel probe (checkParallelDrift
// treats it as unavailable — not fatal).
func resolveLmsCLIPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".lmstudio", "bin", "lms")
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
	Parallel      int    `yaml:"parallel"`
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
			parallel:      p.Options.ModelState.Parallel,
		})
	}
	return entries
}

// probeModelStateEntry checks one backend: read /api/v0/models and report whether
// the declared model is loaded at the declared context. Read-only.
//
// Returns (progressing, err). progressing==true means the model is mid-load
// (state=="loading"), which is Progressing — NOT a health issue: a high-context
// load can take minutes and must never read as Degraded. A non-nil err is a real
// problem (unreachable / wrong context / not loaded / absent). Mirrors the engine
// provider's findModelRow (prefer the state=="loaded" row) and ComputePlan
// (loading is not drift) so the daemon proprioception matches the engine's Health().
func probeModelStateEntry(ctx context.Context, e modelStateEntry) (bool, error) {
	base := strings.TrimRight(e.endpoint, "/")
	if base == "" {
		base = "http://localhost:1234"
	}
	url := base + "/api/v0/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, fmt.Errorf("build request: %w", err)
	}
	if e.apiKeyEnv != "" {
		if tok := os.Getenv(e.apiKeyEnv); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("unreachable: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("/api/v0/models returned %d", resp.StatusCode)
	}

	var out struct {
		Data []struct {
			ID                  string `json:"id"`
			State               string `json:"state"`
			LoadedContextLength *int   `json:"loaded_context_length"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("decode: %w", err)
	}

	// Collect id-matching rows, preferring state=="loaded" so a not-loaded/loading
	// duplicate row cannot shadow the real loaded one (LM Studio can return both).
	loadedIdx, loadingIdx, otherIdx := -1, -1, -1
	for i := range out.Data {
		m := out.Data[i]
		if m.ID != e.model && !strings.HasPrefix(m.ID, e.model) && !strings.HasPrefix(e.model, m.ID) {
			continue
		}
		switch m.State {
		case "loaded":
			if loadedIdx == -1 {
				loadedIdx = i
			}
		case "loading":
			if loadingIdx == -1 {
				loadingIdx = i
			}
		default:
			if otherIdx == -1 {
				otherIdx = i
			}
		}
	}

	switch {
	case loadedIdx != -1:
		m := out.Data[loadedIdx]
		if e.contextLength > 0 && (m.LoadedContextLength == nil || *m.LoadedContextLength != e.contextLength) {
			got := "null"
			if m.LoadedContextLength != nil {
				got = fmt.Sprintf("%d", *m.LoadedContextLength)
			}
			return false, fmt.Errorf("model %q loaded at context %s (want %d)", e.model, got, e.contextLength)
		}
		if e.parallel > 0 && e.local {
			if err := checkParallelDrift(ctx, e, m.ID); err != nil {
				return false, err
			}
		}
		return false, nil
	case loadingIdx != -1:
		return true, nil // mid-load — Progressing, not an issue
	case otherIdx != -1:
		return false, fmt.Errorf("model %q state=%q (want loaded)", e.model, out.Data[otherIdx].State)
	default:
		return false, fmt.Errorf("model %q not present", e.model)
	}
}

// msPsRow mirrors `lms ps --json`'s row shape (the local-only lms CLI — no
// --host flag, so remote backends cannot be probed this way). Confirmed live:
// {"identifier":"ornith-1.0-35b",...,"parallel":1}.
type msPsRow struct {
	Identifier string `json:"identifier"`
	ModelKey   string `json:"modelKey"`
	Parallel   int    `json:"parallel"`
}

// msParallelProbeTimeout bounds the `lms ps --json` shell-out. Deliberately its
// own short budget rather than sharing the outer 4s Health() timeout across all
// entries — a hung lms CLI must not block the whole proprioception cycle.
const msParallelProbeTimeout = 2 * time.Second

// checkParallelDrift shells `lms ps --json` and compares the observed parallel
// value for the row matching loadedID (or e.model, prefix-either-direction —
// mirrors the /api/v0/models matching above) against e.parallel. A probe
// failure (binary missing, non-zero exit, bad JSON) or no matching row is
// NON-FATAL — skip the check rather than erroring, the same "unobserved, not
// wrong" treatment the engine-side provider gives a nil Parallel. Returns a
// non-nil error ONLY on a genuine observed mismatch, which folds into the
// existing (progressing, err) issues aggregation in Health().
func checkParallelDrift(ctx context.Context, e modelStateEntry, loadedID string) error {
	if e.lmsCLIPath == "" {
		return nil
	}
	psCtx, cancel := context.WithTimeout(ctx, msParallelProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(psCtx, e.lmsCLIPath, "ps", "--json").Output()
	if err != nil {
		return nil // lms CLI unavailable — unobserved, not an issue
	}
	var rows []msPsRow
	if json.Unmarshal(out, &rows) != nil {
		return nil // unparseable — unobserved
	}
	for _, r := range rows {
		id := r.Identifier
		if id == "" {
			id = r.ModelKey
		}
		if id == "" {
			continue
		}
		if !(id == loadedID || strings.HasPrefix(id, loadedID) || strings.HasPrefix(loadedID, id) ||
			id == e.model || strings.HasPrefix(id, e.model) || strings.HasPrefix(e.model, id)) {
			continue
		}
		if r.Parallel != e.parallel {
			return fmt.Errorf("model %q loaded with parallel %d (want %d)", e.model, r.Parallel, e.parallel)
		}
		return nil
	}
	return nil // no matching row — unobserved
}
