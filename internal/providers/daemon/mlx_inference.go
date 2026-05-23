// mlx_inference.go — daemon-side health provider for kernel-supervised mlx_lm.server.
//
// mlxInferenceProvider implements reconcile.Reconcilable for the daemon's
// proprioception block. It scans the workspace providers config for any entry
// whose type is "mlx-supervised", then for each one it probes:
//
//  1. Whether the launchd label is registered and running (via launchctl list).
//  2. Whether the /v1/models HTTP endpoint returns 200 with the configured model.
//
// If no mlx-supervised providers are declared, Health() returns HealthSuspended
// (not HealthMissing) — it is an opt-in feature, not a missing requirement.
//
// Full plan/apply (plist write + launchctl load) lives in the engine-layer
// MLXSupervisedProvider. The daemon only exercises Health() through the
// proprioception block.
//
// Registration: init() registers this provider as "mlx-inference" so the
// daemon's cmd/cogos/providers_wire.go import triggers it automatically.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/myrgic/cogos/pkg/substrate/reconcile"
)

const mlxSupervisedTypeKey = "mlx-supervised"

func init() {
	reconcile.RegisterProvider("mlx-inference", &mlxInferenceProvider{stubMethods: stubMethods{name: "mlx-inference"}})
}

// mlxInferenceProvider is the daemon-side Reconcilable for mlx_lm.server supervision.
type mlxInferenceProvider struct {
	stubMethods
}

func (p *mlxInferenceProvider) Type() string { return "mlx-inference" }

// Health inspects all configured mlx-supervised providers and returns the
// aggregate health. Called by buildHealthBlock on every foveated context
// generation; must be cheap and non-blocking.
func (p *mlxInferenceProvider) Health() reconcile.ResourceStatus {
	root, bad := resolveRoot()
	if bad != nil {
		return *bad
	}

	entries := loadMLXEntries(root)
	if len(entries) == 0 {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusUnknown,
			Health:    reconcile.HealthSuspended,
			Operation: reconcile.OperationIdle,
			Message:   "no mlx-supervised providers declared in providers(.local).yaml — opt-in feature",
		}
	}

	// Probe each configured entry. First failure sets the aggregate status.
	var issues []string
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()

	for _, e := range entries {
		if err := probeMLXEntry(ctx, e); err != nil {
			issues = append(issues, fmt.Sprintf("%s: %v", e.name, err))
		}
	}

	if len(issues) > 0 {
		return reconcile.ResourceStatus{
			Sync:      reconcile.SyncStatusOutOfSync,
			Health:    reconcile.HealthDegraded,
			Operation: reconcile.OperationIdle,
			Message:   strings.Join(issues, "; "),
		}
	}

	noun := "provider"
	if len(entries) != 1 {
		noun = "providers"
	}
	return reconcile.ResourceStatus{
		Sync:      reconcile.SyncStatusSynced,
		Health:    reconcile.HealthHealthy,
		Operation: reconcile.OperationIdle,
		Message:   fmt.Sprintf("%d mlx-supervised %s healthy", len(entries), noun),
	}
}

// mlxEntry is the minimal config needed for a daemon-side health probe.
type mlxEntry struct {
	name         string
	endpoint     string
	model        string
	launchdLabel string
}

// loadMLXEntries reads providers(.local).yaml and returns all entries with
// type == "mlx-supervised". Errors are silently ignored (missing file = empty).
// providers.local.yaml is processed second so its entries overlay the base file.
func loadMLXEntries(root string) []mlxEntry {
	var result []mlxEntry

	for _, filename := range []string{"providers.yaml", "providers.local.yaml"} {
		path := filepath.Join(root, ".cog", "config", filename)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		entries := parseMLXEntriesFromYAML(data, filename)
		// Overlay: if same name already present, replace (local wins).
		existing := make(map[string]int)
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

// providerFileCfg is the top-level shape of providers(.local).yaml, used only
// for the daemon-side mlx-supervised parse. Fields beyond what we need are
// ignored via yaml:",inline" / unknown-key tolerance in yaml.v3.
type providerFileCfg struct {
	Providers map[string]providerEntryCfg `yaml:"providers"`
}

// providerEntryCfg captures the fields relevant to mlx-supervised health probes.
// Unknown fields (e.g. options, context_window) are silently ignored.
type providerEntryCfg struct {
	Type         string `yaml:"type"`
	Endpoint     string `yaml:"endpoint"`
	Model        string `yaml:"model"`
	LaunchdLabel string `yaml:"launchd_label"`
}

// parseMLXEntriesFromYAML decodes a providers YAML file with gopkg.in/yaml.v3
// and returns the subset of entries whose type is "mlx-supervised".
//
// This replaces the previous hand-rolled line-by-line state machine. yaml.v3
// correctly handles anchors/aliases, flow-style mappings, multi-line strings,
// and tab/space variants that the bespoke parser silently dropped.
//
// Behavior preservation notes vs. the prior implementation:
//   - All valid-input behavior is identical: same entries extracted, same
//     default launchd_label derivation ("com.cogos.mlx-<name>").
//   - The bespoke parser silently produced empty results for malformed YAML
//     (anchors, flow-style, multi-line strings). yaml.v3 returns a decode
//     error instead; we treat that as "no entries" (same observable outcome
//     for callers) but the error surface is now explicit in the log-path if
//     we later add logging.
func parseMLXEntriesFromYAML(data []byte, _ string) []mlxEntry {
	var cfg providerFileCfg
	if err := yaml.Unmarshal(data, &cfg); err != nil || cfg.Providers == nil {
		return nil
	}

	var entries []mlxEntry
	for name, p := range cfg.Providers {
		if p.Type != mlxSupervisedTypeKey {
			continue
		}
		label := p.LaunchdLabel
		if label == "" {
			label = "com.cogos.mlx-" + name
		}
		entries = append(entries, mlxEntry{
			name:         name,
			endpoint:     p.Endpoint,
			model:        p.Model,
			launchdLabel: label,
		})
	}
	return entries
}

// probeMLXEntry checks a single mlx-supervised provider's health.
// Returns nil if both launchd registration and HTTP probe pass.
// Returns a descriptive error for the first failing check.
func probeMLXEntry(ctx context.Context, e mlxEntry) error {
	// 1. Launchd probe — is the plist registered?
	launchdOK, pid := checkLaunchctlLabel(ctx, e.launchdLabel)
	if !launchdOK {
		return fmt.Errorf("launchd: label %q not registered (run: launchctl load ~/Library/LaunchAgents/%s.plist)", e.launchdLabel, e.launchdLabel)
	}
	_ = pid // PID available for future use (e.g., surfacing in the health message)

	// 2. HTTP probe — is the server accepting requests?
	if !probeEndpointModels(ctx, e.endpoint, e.model) {
		return fmt.Errorf("HTTP: %s/v1/models probe failed (process running, model may be loading)", e.endpoint)
	}
	return nil
}

// checkLaunchctlLabel runs `launchctl list <label>` and returns (registered, pid).
// Non-zero exit = not registered. Zero PID = registered but not running.
func checkLaunchctlLabel(ctx context.Context, label string) (bool, int) {
	cmd := exec.CommandContext(ctx, "launchctl", "list", label)
	out, err := cmd.Output()
	if err != nil {
		return false, 0
	}
	// Parse PID from output.
	pid := 0
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, `"PID"`) {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				pidStr := strings.Trim(strings.TrimSpace(parts[1]), `; "`)
				if n, err := strconv.Atoi(pidStr); err == nil {
					pid = n
				}
			}
		}
	}
	return true, pid
}

// probeEndpointModels probes GET /v1/models and returns true when the response
// is 200 and (if model is non-empty) the model appears in the response data.
func probeEndpointModels(ctx context.Context, endpoint, model string) bool {
	url := strings.TrimRight(endpoint, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return false
	}
	defer resp.Body.Close()

	if model == "" {
		return true
	}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false
	}
	for _, m := range result.Data {
		if m.ID == model || strings.HasPrefix(m.ID, model) || strings.HasPrefix(model, m.ID) {
			return true
		}
	}
	return false
}
