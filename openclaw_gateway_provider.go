// openclaw_gateway_provider.go
// Reconcilable provider that projects CogOS gateway configuration into
// OpenClaw's openclaw.json config. Manages gateway, models, channels,
// messages, commands, hooks, skills, and plugins sections.
// Distinct from openclaw-agents (agent list + bindings) and openclaw-cron
// (cron jobs).
//
// Config source: .cog/config/openclaw-gateway/config.yaml
// Live target:   ~/.openclaw/openclaw.json
//
// Key constraint: Only manages declared keys. Uses subset comparison so
// fields present in live but absent from config.yaml are never touched.
// Secret fields (token, botToken, apiKey, password) are automatically
// excluded during snapshot export.
//
// Usage:
//   cog plan openclaw-gateway
//   cog apply openclaw-gateway
//   cog snapshot openclaw-gateway   # generates config.yaml from live + refreshes state
//   cog status openclaw-gateway

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func init() {
	RegisterProvider("openclaw-gateway", &OpenClawGatewayProvider{})
}

// OpenClawGatewayProvider implements Reconcilable for OpenClaw gateway config.
type OpenClawGatewayProvider struct {
	configPath string // override for testing; defaults to ~/.openclaw/openclaw.json
}

func (p *OpenClawGatewayProvider) Type() string { return "openclaw-gateway" }

// managedSections lists the top-level openclaw.json keys this provider owns.
// agents, bindings → openclaw-agents; cron → openclaw-cron; auth, meta, wizard → never managed.
var managedSections = []string{
	"models", "channels", "gateway",
	"messages", "commands", "hooks", "skills", "plugins",
}

// secretFields are key names that contain sensitive data and must be
// excluded from snapshot config exports. Case-insensitive matching.
var secretFields = map[string]bool{
	"token":    true,
	"bottoken": true,
	"apikey":   true,
	"password": true,
	"secret":   true,
}

// ─── Config types (declared state from config.yaml) ─────────────────────────

// gatewayDeclaredConfig is the full declared state from .cog/config/openclaw-gateway/config.yaml.
type gatewayDeclaredConfig struct {
	Models   map[string]any `yaml:"models,omitempty"`
	Channels map[string]any `yaml:"channels,omitempty"`
	Gateway  map[string]any `yaml:"gateway,omitempty"`
	Messages map[string]any `yaml:"messages,omitempty"`
	Commands map[string]any `yaml:"commands,omitempty"`
	Hooks    map[string]any `yaml:"hooks,omitempty"`
	Skills   map[string]any `yaml:"skills,omitempty"`
	Plugins  map[string]any `yaml:"plugins,omitempty"`
}

// sectionMap returns all declared sections as a name→value map.
func (c *gatewayDeclaredConfig) sectionMap() map[string]map[string]any {
	return map[string]map[string]any{
		"models":   c.Models,
		"channels": c.Channels,
		"gateway":  c.Gateway,
		"messages": c.Messages,
		"commands": c.Commands,
		"hooks":    c.Hooks,
		"skills":   c.Skills,
		"plugins":  c.Plugins,
	}
}

// ─── Live types (current state in openclaw.json) ────────────────────────────

// gatewayLiveConfig represents the relevant sections from openclaw.json.
type gatewayLiveConfig struct {
	Sections map[string]map[string]any // section name → parsed content
	RawData  []byte                    // full raw JSON for safe read-modify-write
}

// ─── Reconcilable implementation ────────────────────────────────────────────

func (p *OpenClawGatewayProvider) LoadConfig(root string) (any, error) {
	cfgPath := filepath.Join(root, ".cog", "config", "openclaw-gateway", "config.yaml")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("openclaw-gateway: read config: %w", err)
	}

	var cfg gatewayDeclaredConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("openclaw-gateway: parse config: %w", err)
	}

	return &cfg, nil
}

func (p *OpenClawGatewayProvider) FetchLive(ctx context.Context, config any) (any, error) {
	configPath := p.resolveConfigPath()

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &gatewayLiveConfig{Sections: make(map[string]map[string]any)}, nil
		}
		return nil, fmt.Errorf("openclaw-gateway: read live config: %w", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("openclaw-gateway: parse live config: %w", err)
	}

	live := &gatewayLiveConfig{
		Sections: make(map[string]map[string]any),
		RawData:  data,
	}

	// Extract all managed sections
	for _, name := range managedSections {
		if section, ok := raw[name]; ok {
			var m map[string]any
			if err := json.Unmarshal(section, &m); err == nil {
				live.Sections[name] = m
			}
		}
	}

	return live, nil
}

func (p *OpenClawGatewayProvider) ComputePlan(config any, live any, state *ReconcileState) (*ReconcilePlan, error) {
	cfg := config.(*gatewayDeclaredConfig)
	liveState := live.(*gatewayLiveConfig)

	plan := &ReconcilePlan{
		ResourceType: "openclaw-gateway",
		GeneratedAt:  time.Now().Format(time.RFC3339),
	}

	sections := cfg.sectionMap()
	for _, name := range managedSections {
		declared := sections[name]
		if declared == nil {
			continue
		}
		liveSection := liveState.Sections[name]

		// models.providers is nested one level deeper
		if name == "models" {
			declaredProviders := extractMapKey(declared, "providers")
			liveProviders := extractMapKey(liveSection, "providers")
			diffSectionSubset(plan, "models.providers", declaredProviders, liveProviders)

			// Also diff top-level models keys (e.g., "mode")
			for k, v := range declared {
				if k == "providers" {
					continue
				}
				liveVal, exists := liveSection[k]
				diffSingleKey(plan, "models", k, v, liveVal, exists)
			}
		} else {
			diffSectionSubset(plan, name, declared, liveSection)
		}
	}

	return plan, nil
}

func (p *OpenClawGatewayProvider) ApplyPlan(ctx context.Context, plan *ReconcilePlan) ([]ReconcileResult, error) {
	configPath := p.resolveConfigPath()

	data, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("openclaw-gateway: read config for apply: %w", err)
	}

	var fullConfig map[string]any
	if err := json.Unmarshal(data, &fullConfig); err != nil {
		return nil, fmt.Errorf("openclaw-gateway: parse config for apply: %w", err)
	}

	var results []ReconcileResult
	modified := false

	for _, action := range plan.Actions {
		if action.Action == ActionSkip {
			results = append(results, ReconcileResult{
				Action: string(ActionSkip),
				Name:   action.Name,
				Status: ApplySkipped,
			})
			continue
		}

		section := action.Details["section"].(string)
		key := action.Details["key"].(string)
		value := action.Details["value"]

		switch action.Action {
		case ActionCreate, ActionUpdate:
			setNestedValue(fullConfig, section, key, value)
			modified = true
			log.Printf("[openclaw-gateway] %s %s.%s", action.Action, section, key)
			results = append(results, ReconcileResult{
				Action: string(action.Action),
				Name:   action.Name,
				Status: ApplySucceeded,
			})

		case ActionDelete:
			deleteNestedValue(fullConfig, section, key)
			modified = true
			log.Printf("[openclaw-gateway] deleted %s.%s", section, key)
			results = append(results, ReconcileResult{
				Action: string(ActionDelete),
				Name:   action.Name,
				Status: ApplySucceeded,
			})
		}
	}

	if !modified {
		return results, nil
	}

	// Marshal and write back using replaceJSONValue to preserve key ordering
	// for each section we modified.
	output := data
	for _, section := range managedSections {
		if sectionData, ok := fullConfig[section]; ok {
			sectionJSON, err := json.MarshalIndent(sectionData, "  ", "  ")
			if err != nil {
				continue
			}
			replaced, err := replaceJSONValue(output, section, sectionJSON)
			if err != nil {
				// Section might not exist in file yet — skip
				continue
			}
			output = replaced
		}
	}

	if err := os.WriteFile(configPath, output, 0644); err != nil {
		return results, fmt.Errorf("openclaw-gateway: write config: %w", err)
	}

	log.Printf("[openclaw-gateway] wrote %d bytes to %s", len(output), configPath)
	return results, nil
}

func (p *OpenClawGatewayProvider) BuildState(config any, live any, existing *ReconcileState) (*ReconcileState, error) {
	liveState := live.(*gatewayLiveConfig)

	state := &ReconcileState{
		Version:      1,
		Serial:       1,
		Lineage:      "openclaw-gateway",
		ResourceType: "openclaw-gateway",
		GeneratedAt:  time.Now().Format(time.RFC3339),
	}

	now := time.Now().Format(time.RFC3339)

	// Track models.providers
	if models, ok := liveState.Sections["models"]; ok {
		if providers, ok := models["providers"]; ok {
			if provMap, ok := providers.(map[string]any); ok {
				for name := range provMap {
					state.Resources = append(state.Resources, ReconcileResource{
						Address:       "models.providers." + name,
						Type:          "gateway-model-provider",
						Mode:          ModeManaged,
						ExternalID:    name,
						Name:          name,
						Attributes:    map[string]any{"section": "models.providers"},
						LastRefreshed: now,
					})
				}
			}
		}
	}

	// Track channels
	if channels, ok := liveState.Sections["channels"]; ok {
		for name := range channels {
			state.Resources = append(state.Resources, ReconcileResource{
				Address:       "channels." + name,
				Type:          "gateway-channel",
				Mode:          ModeManaged,
				ExternalID:    name,
				Name:          name,
				Attributes:    map[string]any{"section": "channels"},
				LastRefreshed: now,
			})
		}
	}

	// Track gateway settings
	if gw, ok := liveState.Sections["gateway"]; ok {
		for name := range gw {
			state.Resources = append(state.Resources, ReconcileResource{
				Address:       "gateway." + name,
				Type:          "gateway-setting",
				Mode:          ModeManaged,
				ExternalID:    name,
				Name:          name,
				Attributes:    map[string]any{"section": "gateway"},
				LastRefreshed: now,
			})
		}
	}

	// Track other sections as single resources
	for _, section := range []string{"messages", "commands", "hooks", "skills", "plugins"} {
		if _, ok := liveState.Sections[section]; ok {
			state.Resources = append(state.Resources, ReconcileResource{
				Address:       section,
				Type:          "gateway-section",
				Mode:          ModeManaged,
				ExternalID:    section,
				Name:          section,
				Attributes:    map[string]any{"section": section},
				LastRefreshed: now,
			})
		}
	}

	return state, nil
}

func (p *OpenClawGatewayProvider) Health() ResourceStatus {
	configPath := p.resolveConfigPath()
	if _, err := os.Stat(configPath); err != nil {
		return ResourceStatus{
			Sync:      SyncStatusUnknown,
			Health:    HealthMissing,
			Operation: OperationIdle,
			Message:   "openclaw.json not found",
		}
	}
	return NewResourceStatus(SyncStatusUnknown, HealthHealthy)
}

// ─── Config Export (ConfigExporter interface) ───────────────────────────────

// ExportConfig reads the live openclaw.json, extracts managed sections,
// redacts secret fields, and writes config.yaml. This is the "snapshot"
// operation that establishes the declared state baseline.
func (p *OpenClawGatewayProvider) ExportConfig(root string) error {
	configPath := p.resolveConfigPath()
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("openclaw-gateway: read live config for export: %w", err)
	}

	var full map[string]any
	if err := json.Unmarshal(data, &full); err != nil {
		return fmt.Errorf("openclaw-gateway: parse live config for export: %w", err)
	}

	// Build snapshot with only managed sections, secrets redacted
	snapshot := make(map[string]any)
	for _, name := range managedSections {
		if section, ok := full[name]; ok {
			snapshot[name] = redactSecrets(section)
		}
	}

	// Marshal to YAML
	yamlData, err := yaml.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("openclaw-gateway: marshal snapshot: %w", err)
	}

	// Build output with header comment
	var buf []byte
	buf = append(buf, []byte("# OpenClaw Gateway Configuration\n")...)
	buf = append(buf, []byte("# Generated by: cog snapshot openclaw-gateway\n")...)
	buf = append(buf, []byte(fmt.Sprintf("# Generated at: %s\n", time.Now().Format(time.RFC3339)))...)
	buf = append(buf, []byte("#\n")...)
	buf = append(buf, []byte("# Only keys listed here are managed by the reconciler.\n")...)
	buf = append(buf, []byte("# Secret fields (token, botToken, apiKey, password) are excluded.\n")...)
	buf = append(buf, []byte("# Fields present in live but absent here are never touched.\n")...)
	buf = append(buf, []byte("\n")...)
	buf = append(buf, yamlData...)

	outPath := filepath.Join(root, ".cog", "config", "openclaw-gateway", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		return fmt.Errorf("openclaw-gateway: create config dir: %w", err)
	}
	if err := os.WriteFile(outPath, buf, 0644); err != nil {
		return fmt.Errorf("openclaw-gateway: write config.yaml: %w", err)
	}

	return nil
}

// ─── Helpers ────────────────────────────────────────────────────────────────

func (p *OpenClawGatewayProvider) resolveConfigPath() string {
	if p.configPath != "" {
		return p.configPath
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".openclaw", "openclaw.json")
}

// redactSecrets recursively removes secret fields from a value.
// Returns a deep copy with sensitive keys removed.
func redactSecrets(v any) any {
	switch val := v.(type) {
	case map[string]any:
		result := make(map[string]any, len(val))
		for k, child := range val {
			if secretFields[strings.ToLower(k)] {
				continue // omit entirely
			}
			result[k] = redactSecrets(child)
		}
		return result
	case []any:
		result := make([]any, len(val))
		for i, item := range val {
			result[i] = redactSecrets(item)
		}
		return result
	default:
		return v
	}
}

// extractMapKey safely extracts a key from a map[string]any as map[string]any.
func extractMapKey(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	v, ok := m[key]
	if !ok {
		return nil
	}
	if sub, ok := v.(map[string]any); ok {
		return sub
	}
	return nil
}

// diffSectionSubset compares declared vs live for a named section.
// Uses subset semantics: only fields present in declared are compared.
// Fields in live but not in declared are ignored (unmanaged).
func diffSectionSubset(plan *ReconcilePlan, sectionPath string, declared, live map[string]any) {
	if declared == nil {
		return
	}

	liveMap := live
	if liveMap == nil {
		liveMap = make(map[string]any)
	}

	for key, declaredVal := range declared {
		liveVal, exists := liveMap[key]
		diffSingleKey(plan, sectionPath, key, declaredVal, liveVal, exists)
	}
}

// diffSingleKey compares a single declared key against its live counterpart.
func diffSingleKey(plan *ReconcilePlan, sectionPath, key string, declaredVal, liveVal any, liveExists bool) {
	if !liveExists {
		plan.Actions = append(plan.Actions, ReconcileAction{
			Action:       ActionCreate,
			ResourceType: "gateway-config",
			Name:         sectionPath + "." + key,
			Details: map[string]any{
				"section": sectionPath,
				"key":     key,
				"value":   declaredVal,
			},
		})
		plan.Summary.Creates++
		return
	}

	declNorm := normalizeForCompare(declaredVal)
	liveNorm := normalizeForCompare(liveVal)

	if !deepSubsetEqual(declNorm, liveNorm) {
		plan.Actions = append(plan.Actions, ReconcileAction{
			Action:       ActionUpdate,
			ResourceType: "gateway-config",
			Name:         sectionPath + "." + key,
			Details: map[string]any{
				"section": sectionPath,
				"key":     key,
				"value":   declaredVal,
				"current": liveVal,
			},
		})
		plan.Summary.Updates++
	} else {
		plan.Actions = append(plan.Actions, ReconcileAction{
			Action:       ActionSkip,
			ResourceType: "gateway-config",
			Name:         sectionPath + "." + key,
			Details: map[string]any{
				"section": sectionPath,
				"key":     key,
			},
		})
		plan.Summary.Skipped++
	}
}

// deepSubsetEqual returns true if every field in declared matches the
// corresponding field in live. Fields in live but not in declared are
// ignored. For non-map values, uses reflect.DeepEqual.
func deepSubsetEqual(declared, live any) bool {
	declMap, declIsMap := declared.(map[string]any)
	liveMap, liveIsMap := live.(map[string]any)

	if declIsMap && liveIsMap {
		for k, declVal := range declMap {
			liveVal, exists := liveMap[k]
			if !exists {
				return false
			}
			if !deepSubsetEqual(declVal, liveVal) {
				return false
			}
		}
		return true
	}

	// For slices and scalars, require exact match
	return reflect.DeepEqual(declared, live)
}

// normalizeForCompare converts YAML-parsed values to JSON-roundtripped form
// so that numeric types match (YAML: int, JSON: float64).
func normalizeForCompare(v any) any {
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var normalized any
	if err := json.Unmarshal(data, &normalized); err != nil {
		return v
	}
	return normalized
}

// setNestedValue sets a value at section.key in a nested map.
// section format: "models.providers" → fullConfig["models"]["providers"][key] = value
// When both the existing and new values are maps, deepMergeMap is used so
// that keys present in the live config but absent from the declared config
// (e.g. redacted secrets) are preserved.
func setNestedValue(root map[string]any, section, key string, value any) {
	parts := splitDotPath(section)
	current := root
	for _, part := range parts {
		next, ok := current[part]
		if !ok {
			next = make(map[string]any)
			current[part] = next
		}
		if m, ok := next.(map[string]any); ok {
			current = m
		} else {
			return
		}
	}
	// Deep-merge maps so unmanaged keys (tokens, secrets) survive.
	if existingMap, ok := current[key].(map[string]any); ok {
		if newMap, ok := value.(map[string]any); ok {
			deepMergeMap(existingMap, newMap)
			return
		}
	}
	current[key] = value
}

// deepMergeMap recursively merges src into dst. Keys in src overwrite keys
// in dst; keys in dst that are absent from src are left untouched.
func deepMergeMap(dst, src map[string]any) {
	for k, srcVal := range src {
		dstVal, exists := dst[k]
		if exists {
			dstMap, dstIsMap := dstVal.(map[string]any)
			srcMap, srcIsMap := srcVal.(map[string]any)
			if dstIsMap && srcIsMap {
				deepMergeMap(dstMap, srcMap)
				continue
			}
		}
		dst[k] = srcVal
	}
}

// deleteNestedValue removes a key at section.key in a nested map.
func deleteNestedValue(root map[string]any, section, key string) {
	parts := splitDotPath(section)
	current := root
	for _, part := range parts {
		next, ok := current[part]
		if !ok {
			return
		}
		if m, ok := next.(map[string]any); ok {
			current = m
		} else {
			return
		}
	}
	delete(current, key)
}

// splitDotPath splits "models.providers" into ["models", "providers"].
func splitDotPath(path string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(path); i++ {
		if path[i] == '.' {
			if i > start {
				parts = append(parts, path[start:i])
			}
			start = i + 1
		}
	}
	if start < len(path) {
		parts = append(parts, path[start:])
	}
	return parts
}
