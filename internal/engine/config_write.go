// config_write.go — kernel.yaml mutation plumbing for CogOS v3.
//
// Implements the Config Mutation API per Agent O's design
// (`cog://mem/semantic/surveys/2026-04-21-consolidation/agent-O-config-mutation-design`).
//
// Core exports consumed by MCP / HTTP surfaces:
//
//	ReadConfigOnDisk(root)         — parse kernel.yaml into kernelConfig
//	WriteConfigPatch(root, patch,) — validate + atomic-write with rotating backups
//	RollbackConfig(root, name)     — restore a prior .bak-<ts>
//	ResolveFromKernelConfig(kc)    — project kernelConfig → effective *Config
//	DefaultKernelYAML()            — hardcoded defaults for diff surfaces
//
// Merge semantics follow RFC 7396 (JSON Merge Patch): explicit null deletes the
// key (restoring LoadConfig's default on next boot); missing keys are preserved.
//
// Concurrency: package-level writeConfigMu serializes all mutating operations.
// v1 intentionally writes-to-disk + `requires_restart: true` — we never mutate
// live *Config pointers under the hot path.
package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// writeConfigMu serializes all kernel.yaml mutations across MCP + HTTP surfaces.
// Last-writer-wins; callers that race simply land in some order.
var writeConfigMu sync.Mutex

const (
	// kernelYAMLRel is the workspace-relative path to the writable kernel config.
	kernelYAMLRel = ".cog/config/kernel.yaml"
	// backupPrefix is prepended to `.bak-<timestamp>` files.
	backupPrefix = "kernel.yaml.bak-"
	// backupKeep is the number of rotating backups kept.
	backupKeep = 10
	// backupTimeLayout is the RFC3339-like layout used in backup filenames.
	// Colons are replaced with hyphens to remain friendly on case-insensitive
	// filesystems.
	backupTimeLayout = "2006-01-02T15-04-05Z"
)

// ConfigViolation records a single validation failure.
type ConfigViolation struct {
	Field string `json:"field"`
	Rule  string `json:"rule"`
	Got   any    `json:"got,omitempty"`
}

// ConfigDiffEntry describes a single field that changed between two configs.
type ConfigDiffEntry struct {
	Field  string `json:"field"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// BackupEntry describes a single rotating backup file on disk.
type BackupEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Timestamp string `json:"timestamp"`
	Size      int64  `json:"size"`
}

// WriteConfigOptions controls WriteConfigPatch behaviour.
type WriteConfigOptions struct {
	Scope  string // "top" (default) or "v3"
	DryRun bool
}

// WriteConfigResult is the value returned from WriteConfigPatch / exposed by
// `cog_write_config` and `PATCH /v1/config`.
type WriteConfigResult struct {
	Written         bool              `json:"written"`
	RequiresRestart bool              `json:"requires_restart"`
	Violations      []ConfigViolation `json:"violations,omitempty"`
	EffectiveConfig map[string]any    `json:"effective_config,omitempty"`
	BackupPath      string            `json:"backup_path,omitempty"`
	Diff            []ConfigDiffEntry `json:"diff,omitempty"`
	ChangedFields   []string          `json:"changed_fields,omitempty"`
	Path            string            `json:"path"`
	DryRun          bool              `json:"dry_run,omitempty"`
}

// ReadConfigResult is returned from the read-side helpers. It mirrors the MCP
// + HTTP read surface so the wiring layer is a thin projection.
type ReadConfigResult struct {
	EffectiveConfig map[string]any `json:"effective_config"`
	Path            string         `json:"path"`
	Exists          bool           `json:"exists"`
	RawYAML         string         `json:"raw_yaml,omitempty"`
	Defaults        map[string]any `json:"defaults,omitempty"`
}

// reloadSafeFields lists configuration keys that can in principle be hot-
// reloaded in v1.5 without restarting the daemon. Today every mutation still
// returns requires_restart: true, but callers can inspect diff.changed_fields
// to decide whether they need to restart right now or can defer.
var reloadSafeFields = map[string]bool{
	"consolidation_interval":       true,
	"heartbeat_interval":           true,
	"salience_days_window":         true,
	"output_reserve":               true,
	"ollama_embed_endpoint":        true,
	"ollama_embed_model":           true,
	"tool_call_validation_enabled": true,
}

// ── Read ────────────────────────────────────────────────────────────────────

// ReadConfigOnDisk parses kernel.yaml at <root>/.cog/config/kernel.yaml. A
// missing file is not an error: the returned kernelConfig is the zero value,
// exists is false, and rawYAML is empty.
func ReadConfigOnDisk(root string) (kc kernelConfig, rawYAML string, exists bool, err error) {
	path := filepath.Join(root, kernelYAMLRel)
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if os.IsNotExist(rerr) {
			return kernelConfig{}, "", false, nil
		}
		return kernelConfig{}, "", false, fmt.Errorf("read %s: %w", path, rerr)
	}
	if uerr := yaml.Unmarshal(data, &kc); uerr != nil {
		// A malformed file is surfaced to the writer so it can refuse to
		// persist on top of garbage. LoadConfig itself still tolerates this
		// (by design), so we return the raw bytes too.
		return kernelConfig{}, string(data), true, fmt.Errorf("parse %s: %w", path, uerr)
	}
	return kc, string(data), true, nil
}

// ResolveFromKernelConfig projects a kernelConfig into the effective *Config
// the daemon would load, applying LoadConfig's default / override semantics.
// Used for `effective_config` surfaces without needing to touch disk.
func ResolveFromKernelConfig(root string, kc kernelConfig) *Config {
	cfg := &Config{
		WorkspaceRoot:             root,
		CogDir:                    filepath.Join(root, ".cog"),
		Port:                      6931,
		ConsolidationInterval:     3600,
		HeartbeatInterval:         60,
		SalienceDaysWindow:        90,
		OutputReserve:             4096,
		ToolCallValidationEnabled: true,
		LocalModel:                defaultOllamaModel,
		DigestPaths:               map[string]string{},
	}
	applyKernelSection(cfg, kc.kernelConfigSection)
	applyKernelSection(cfg, kc.V3)
	return cfg
}

// DefaultKernelYAML returns a kernelConfig with no overrides (pure defaults).
// Used for the `include_defaults` read surface.
func DefaultKernelYAML(root string) *Config {
	return ResolveFromKernelConfig(root, kernelConfig{})
}

// ConfigToMap projects an effective *Config into a stable JSON-friendly map.
// Field names match the YAML keys to keep read/write schemas symmetrical.
func ConfigToMap(cfg *Config) map[string]any {
	if cfg == nil {
		return map[string]any{}
	}
	digest := map[string]string{}
	for k, v := range cfg.DigestPaths {
		digest[k] = v
	}
	return map[string]any{
		"port":                         cfg.Port,
		"consolidation_interval":       cfg.ConsolidationInterval,
		"heartbeat_interval":           cfg.HeartbeatInterval,
		"salience_days_window":         cfg.SalienceDaysWindow,
		"output_reserve":               cfg.OutputReserve,
		"trm_weights_path":             cfg.TRMWeightsPath,
		"trm_embeddings_path":          cfg.TRMEmbeddingsPath,
		"trm_chunks_path":              cfg.TRMChunksPath,
		"ollama_embed_endpoint":        cfg.OllamaEmbedEndpoint,
		"ollama_embed_model":           cfg.OllamaEmbedModel,
		"tool_call_validation_enabled": cfg.ToolCallValidationEnabled,
		"local_model":                  cfg.LocalModel,
		"digest_paths":                 digest,
	}
}

// ReadConfigSnapshot bundles the read view used by `cog_read_config` and
// `GET /v1/config`.
func ReadConfigSnapshot(root string, includeRaw, includeDefaults bool) (ReadConfigResult, error) {
	path := filepath.Join(root, kernelYAMLRel)
	kc, raw, exists, rerr := ReadConfigOnDisk(root)
	// Even on parse errors we still return a usable snapshot (empty kc) so
	// operators can see what they're working with — matches LoadConfig's
	// "surprising-but-intentional" resilience.
	result := ReadConfigResult{
		EffectiveConfig: ConfigToMap(ResolveFromKernelConfig(root, kc)),
		Path:            path,
		Exists:          exists,
	}
	if includeRaw {
		result.RawYAML = raw
	}
	if includeDefaults {
		result.Defaults = ConfigToMap(DefaultKernelYAML(root))
	}
	if rerr != nil {
		// Surface the parse error as an empty-map + error so callers can
		// decide how to present it. We do not block the read.
		return result, rerr
	}
	return result, nil
}

// ── Merge (RFC 7396) ────────────────────────────────────────────────────────

// patchField enumerates the fields `cog_write_config` accepts. A separate
// (duplicated-but-tiny) mapping keeps us decoupled from kernelConfigSection's
// yaml tags and makes validation errors stable.
var patchFields = []string{
	"port",
	"consolidation_interval",
	"heartbeat_interval",
	"salience_days_window",
	"output_reserve",
	"trm_weights_path",
	"trm_embeddings_path",
	"trm_chunks_path",
	"ollama_embed_endpoint",
	"ollama_embed_model",
	"tool_call_validation_enabled",
	"local_model",
	"digest_paths",
}

var patchFieldSet = func() map[string]bool {
	m := make(map[string]bool, len(patchFields))
	for _, f := range patchFields {
		m[f] = true
	}
	return m
}()

// applyMergePatch merges a raw JSON-shaped patch into the target section using
// RFC 7396 semantics. Returns changed-field names (sorted) and a slice of
// violations accumulated during coercion.
//
// Scalar coercion is forgiving (JSON numbers are float64 by default, so we
// narrow to int where the schema expects int). Unknown keys produce violations.
func applyMergePatch(section *kernelConfigSection, patch map[string]any) (changed []string, violations []ConfigViolation) {
	changedSet := map[string]bool{}

	add := func(field string) {
		if !changedSet[field] {
			changedSet[field] = true
			changed = append(changed, field)
		}
	}

	for key, val := range patch {
		if !patchFieldSet[key] {
			violations = append(violations, ConfigViolation{
				Field: key,
				Rule:  "unknown_field",
				Got:   val,
			})
			continue
		}
		// null → unset (zero the field; LoadConfig re-applies its default
		// because applyKernelSection treats zero as "keep default").
		if val == nil {
			zeroField(section, key)
			add(key)
			continue
		}
		if verr := setField(section, key, val); verr != nil {
			violations = append(violations, *verr)
			continue
		}
		add(key)
	}
	sort.Strings(changed)
	return changed, violations
}

// zeroField sets a kernelConfigSection field to its zero value (RFC 7396 null).
func zeroField(s *kernelConfigSection, key string) {
	switch key {
	case "port":
		s.Port = 0
	case "consolidation_interval":
		s.ConsolidationInterval = 0
	case "heartbeat_interval":
		s.HeartbeatInterval = 0
	case "salience_days_window":
		s.SalienceDaysWindow = 0
	case "output_reserve":
		s.OutputReserve = 0
	case "trm_weights_path":
		s.TRMWeightsPath = ""
	case "trm_embeddings_path":
		s.TRMEmbeddingsPath = ""
	case "trm_chunks_path":
		s.TRMChunksPath = ""
	case "ollama_embed_endpoint":
		s.OllamaEmbedEndpoint = ""
	case "ollama_embed_model":
		s.OllamaEmbedModel = ""
	case "tool_call_validation_enabled":
		s.ToolCallValidation = nil
	case "local_model":
		s.LocalModel = ""
	case "digest_paths":
		s.DigestPaths = nil
	}
}

// setField coerces val and writes it into section[key]. Returns a violation
// on type mismatch (nil on success).
func setField(s *kernelConfigSection, key string, val any) *ConfigViolation {
	intField := func(target *int) *ConfigViolation {
		n, ok := coerceInt(val)
		if !ok {
			return &ConfigViolation{Field: key, Rule: "expected_int", Got: val}
		}
		*target = n
		return nil
	}
	strField := func(target *string) *ConfigViolation {
		s2, ok := val.(string)
		if !ok {
			return &ConfigViolation{Field: key, Rule: "expected_string", Got: val}
		}
		*target = s2
		return nil
	}
	switch key {
	case "port":
		return intField(&s.Port)
	case "consolidation_interval":
		return intField(&s.ConsolidationInterval)
	case "heartbeat_interval":
		return intField(&s.HeartbeatInterval)
	case "salience_days_window":
		return intField(&s.SalienceDaysWindow)
	case "output_reserve":
		return intField(&s.OutputReserve)
	case "trm_weights_path":
		return strField(&s.TRMWeightsPath)
	case "trm_embeddings_path":
		return strField(&s.TRMEmbeddingsPath)
	case "trm_chunks_path":
		return strField(&s.TRMChunksPath)
	case "ollama_embed_endpoint":
		return strField(&s.OllamaEmbedEndpoint)
	case "ollama_embed_model":
		return strField(&s.OllamaEmbedModel)
	case "local_model":
		return strField(&s.LocalModel)
	case "tool_call_validation_enabled":
		b, ok := val.(bool)
		if !ok {
			return &ConfigViolation{Field: key, Rule: "expected_bool", Got: val}
		}
		s.ToolCallValidation = &b
		return nil
	case "digest_paths":
		raw, ok := val.(map[string]any)
		if !ok {
			return &ConfigViolation{Field: key, Rule: "expected_object", Got: val}
		}
		out := make(map[string]string, len(raw))
		for k, v := range raw {
			if k == "" {
				return &ConfigViolation{Field: key, Rule: "empty_map_key", Got: raw}
			}
			vs, ok := v.(string)
			if !ok {
				return &ConfigViolation{Field: key, Rule: "expected_string_values", Got: raw}
			}
			out[k] = vs
		}
		s.DigestPaths = out
		return nil
	}
	return &ConfigViolation{Field: key, Rule: "unknown_field", Got: val}
}

// coerceInt accepts int, int64, float64 (JSON default) — narrows to int. Rejects
// non-integer floats.
func coerceInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	}
	return 0, false
}

// ── Validation ──────────────────────────────────────────────────────────────

// validateSection enforces the static rules from Agent O §4. Validates the
// post-merge state, not the raw patch, so incremental patches that leave the
// merged config invalid are caught.
func validateSection(scope string, s kernelConfigSection) []ConfigViolation {
	var v []ConfigViolation
	prefix := ""
	if scope == "v3" {
		prefix = "v3."
	}
	if s.Port != 0 && (s.Port < 1 || s.Port > 65535) {
		v = append(v, ConfigViolation{Field: prefix + "port", Rule: "1<=port<=65535", Got: s.Port})
	}
	// Negative / zero intervals are rejected only when the field is explicitly
	// set (zero means "use default" in LoadConfig's contract). Negative is a
	// bug in either case.
	if s.ConsolidationInterval < 0 {
		v = append(v, ConfigViolation{Field: prefix + "consolidation_interval", Rule: ">=0", Got: s.ConsolidationInterval})
	}
	if s.HeartbeatInterval < 0 {
		v = append(v, ConfigViolation{Field: prefix + "heartbeat_interval", Rule: ">=0", Got: s.HeartbeatInterval})
	}
	if s.SalienceDaysWindow < 0 {
		v = append(v, ConfigViolation{Field: prefix + "salience_days_window", Rule: ">=0", Got: s.SalienceDaysWindow})
	}
	if s.OutputReserve < 0 {
		v = append(v, ConfigViolation{Field: prefix + "output_reserve", Rule: ">=0", Got: s.OutputReserve})
	}
	if s.OllamaEmbedEndpoint != "" {
		if _, err := url.Parse(s.OllamaEmbedEndpoint); err != nil {
			v = append(v, ConfigViolation{Field: prefix + "ollama_embed_endpoint", Rule: "valid_url", Got: s.OllamaEmbedEndpoint})
		} else if !strings.HasPrefix(s.OllamaEmbedEndpoint, "http://") && !strings.HasPrefix(s.OllamaEmbedEndpoint, "https://") {
			v = append(v, ConfigViolation{Field: prefix + "ollama_embed_endpoint", Rule: "http(s)_scheme_required", Got: s.OllamaEmbedEndpoint})
		}
	}
	for k := range s.DigestPaths {
		if strings.TrimSpace(k) == "" {
			v = append(v, ConfigViolation{Field: prefix + "digest_paths", Rule: "empty_map_key"})
			break
		}
	}
	return v
}

// validateKernelConfig runs validation across the merged top-level + v3 section.
func validateKernelConfig(kc kernelConfig) []ConfigViolation {
	var v []ConfigViolation
	v = append(v, validateSection("top", kc.kernelConfigSection)...)
	v = append(v, validateSection("v3", kc.V3)...)
	return v
}

// ── Diff ────────────────────────────────────────────────────────────────────

// diffConfigMaps returns a sorted list of changed entries between two
// effective-config maps. Used both for the response payload and for deciding
// requires_restart.
func diffConfigMaps(before, after map[string]any) []ConfigDiffEntry {
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	var out []ConfigDiffEntry
	for _, k := range sorted {
		b, a := before[k], after[k]
		if !configValueEqual(b, a) {
			out = append(out, ConfigDiffEntry{Field: k, Before: b, After: a})
		}
	}
	return out
}

func configValueEqual(a, b any) bool {
	// Fast path: primitive equality works for the scalar fields. Maps compare
	// by length + key equality — our digest_paths maps are small.
	am, aOK := a.(map[string]string)
	bm, bOK := b.(map[string]string)
	if aOK && bOK {
		if len(am) != len(bm) {
			return false
		}
		for k, v := range am {
			if bm[k] != v {
				return false
			}
		}
		return true
	}
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// ── Write + Backup ──────────────────────────────────────────────────────────

// WriteConfigPatch merges the supplied JSON patch into kernel.yaml, validates
// the result, and — on success — writes atomically after rotating backups.
// The top-level mutex serializes concurrent callers.
func WriteConfigPatch(root string, patch map[string]any, opts WriteConfigOptions) (WriteConfigResult, error) {
	writeConfigMu.Lock()
	defer writeConfigMu.Unlock()
	return writeConfigPatchLocked(root, patch, opts)
}

func writeConfigPatchLocked(root string, patch map[string]any, opts WriteConfigOptions) (WriteConfigResult, error) {
	if opts.Scope == "" {
		opts.Scope = "top"
	}
	if opts.Scope != "top" && opts.Scope != "v3" {
		return WriteConfigResult{Written: false, Path: filepath.Join(root, kernelYAMLRel), Violations: []ConfigViolation{{
			Field: "scope", Rule: "must_be_top_or_v3", Got: opts.Scope,
		}}}, nil
	}
	if patch == nil {
		patch = map[string]any{}
	}

	path := filepath.Join(root, kernelYAMLRel)

	// 1. Read current (tolerate missing; refuse to overwrite corrupt files).
	current, _, exists, rerr := ReadConfigOnDisk(root)
	if rerr != nil {
		return WriteConfigResult{
			Written: false,
			Path:    path,
			Violations: []ConfigViolation{{
				Field: "kernel.yaml",
				Rule:  "existing_file_unparseable",
				Got:   rerr.Error(),
			}},
		}, nil
	}

	// 2. Merge (RFC 7396). Merge onto a copy; do not mutate `current`.
	merged := current
	var (
		changed    []string
		violations []ConfigViolation
	)
	switch opts.Scope {
	case "v3":
		changed, violations = applyMergePatch(&merged.V3, patch)
	default:
		changed, violations = applyMergePatch(&merged.kernelConfigSection, patch)
	}
	if len(violations) > 0 {
		return WriteConfigResult{
			Written:    false,
			Path:       path,
			Violations: violations,
		}, nil
	}

	// 3. Validate the merged state.
	if vs := validateKernelConfig(merged); len(vs) > 0 {
		return WriteConfigResult{
			Written:    false,
			Path:       path,
			Violations: vs,
		}, nil
	}

	// 4. Compute effective-config maps & diff.
	beforeEff := ConfigToMap(ResolveFromKernelConfig(root, current))
	afterEff := ConfigToMap(ResolveFromKernelConfig(root, merged))
	diff := diffConfigMaps(beforeEff, afterEff)

	// requires_restart: v1 contract is blanket-true on actual writes because
	// we don't hot-reload. We still return the reload-safety hint via the
	// changed field list so callers can reason about it.
	requiresRestart := len(diff) > 0

	// 5. Dry run exits here.
	if opts.DryRun {
		return WriteConfigResult{
			Written:         false,
			RequiresRestart: requiresRestart,
			EffectiveConfig: afterEff,
			Diff:            diff,
			ChangedFields:   changed,
			Path:            path,
			DryRun:          true,
		}, nil
	}

	// 6. Backup existing file (if any) before we overwrite.
	backupPath := ""
	if exists {
		bp, berr := backupKernelYAML(path)
		if berr != nil {
			return WriteConfigResult{}, fmt.Errorf("backup: %w", berr)
		}
		backupPath = bp
		if rerr := rotateBackups(filepath.Dir(path), backupKeep); rerr != nil {
			// Non-fatal: log-worthy but we proceed with the write.
			// Caller can still see the new backup_path in the response.
			_ = rerr
		}
	}

	// 7. Serialize + atomic write.
	//
	// Instead of marshalling the full merged struct (which destroys comments,
	// zeroes absent fields, and emits a spurious duplicate v3: block), we
	// patch only the keys the caller actually set, leaving everything else
	// untouched in the YAML node tree.
	var out []byte
	if exists {
		// Re-read raw bytes (same file we backed up; read again to avoid
		// holding the bytes across the backup path).
		rawBytes, rerr2 := os.ReadFile(path)
		if rerr2 != nil {
			return WriteConfigResult{}, fmt.Errorf("re-read %s for node patch: %w", path, rerr2)
		}
		patched, perr := patchYAMLNodes(rawBytes, patch, opts.Scope)
		if perr != nil {
			return WriteConfigResult{}, fmt.Errorf("node-patch kernel.yaml: %w", perr)
		}
		out = patched
	} else {
		// New file: write only the patched keys under the appropriate scope.
		minimal, merr := buildMinimalYAML(patch, opts.Scope)
		if merr != nil {
			return WriteConfigResult{}, fmt.Errorf("build minimal kernel.yaml: %w", merr)
		}
		out = minimal
	}
	if werr := atomicWriteConfigFile(path, out); werr != nil {
		return WriteConfigResult{}, fmt.Errorf("write %s: %w", path, werr)
	}

	return WriteConfigResult{
		Written:         true,
		RequiresRestart: requiresRestart,
		EffectiveConfig: afterEff,
		BackupPath:      backupPath,
		Diff:            diff,
		ChangedFields:   changed,
		Path:            path,
	}, nil
}

// backupKernelYAML copies kernel.yaml → kernel.yaml.bak-<timestamp>.
// Returns the backup filename (absolute path).
func backupKernelYAML(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(path)
	name := backupPrefix + time.Now().UTC().Format(backupTimeLayout)
	dst := filepath.Join(dir, name)
	// Guard against timestamp collisions (test loops can outrun 1s resolution).
	for i := 1; ; i++ {
		if _, err := os.Stat(dst); os.IsNotExist(err) {
			break
		}
		dst = filepath.Join(dir, fmt.Sprintf("%s-%d", name, i))
	}
	if err := atomicWriteConfigFile(dst, data); err != nil {
		return "", err
	}
	return dst, nil
}

// rotateBackups keeps the most-recent `keep` backup files, deleting older ones.
// Sorts lexicographically — because our timestamp layout is ISO-8601 ordered,
// lexicographic sort == chronological sort.
func rotateBackups(dir string, keep int) error {
	entries, err := ListBackups(dir)
	if err != nil {
		return err
	}
	if len(entries) <= keep {
		return nil
	}
	// ListBackups returns newest-first; drop the tail.
	for _, e := range entries[keep:] {
		_ = os.Remove(e.Path)
	}
	return nil
}

// ListBackups returns the .bak- files in dir, newest-first.
func ListBackups(dir string) ([]BackupEntry, error) {
	fs, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []BackupEntry
	for _, f := range fs {
		if f.IsDir() {
			continue
		}
		name := f.Name()
		if !strings.HasPrefix(name, backupPrefix) {
			continue
		}
		info, ierr := f.Info()
		if ierr != nil {
			continue
		}
		out = append(out, BackupEntry{
			Name:      name,
			Path:      filepath.Join(dir, name),
			Timestamp: strings.TrimPrefix(name, backupPrefix),
			Size:      info.Size(),
		})
	}
	// newest first by name (ISO-8601 lexicographically sorts chronologically)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name > out[j].Name
	})
	return out, nil
}

// ── Rollback ───────────────────────────────────────────────────────────────

// RollbackResult is the response shape for `cog_rollback_config` /
// `POST /v1/config/rollback`.
type RollbackResult struct {
	Restored        bool          `json:"restored"`
	RestoredFrom    string        `json:"restored_from,omitempty"`
	RequiresRestart bool          `json:"requires_restart"`
	Backups         []BackupEntry `json:"backups"`
	Path            string        `json:"path"`
	Error           string        `json:"error,omitempty"`
}

// RollbackOptions controls rollback behaviour.
type RollbackOptions struct {
	Backup   string // bare filename, e.g. "kernel.yaml.bak-2026-04-21T16-30-00Z". Empty → most recent.
	ListOnly bool
}

// RollbackConfig restores kernel.yaml from a prior backup, optionally listing
// backups without mutating anything.
func RollbackConfig(root string, opts RollbackOptions) (RollbackResult, error) {
	writeConfigMu.Lock()
	defer writeConfigMu.Unlock()

	path := filepath.Join(root, kernelYAMLRel)
	dir := filepath.Dir(path)
	backups, err := ListBackups(dir)
	if err != nil {
		return RollbackResult{Path: path}, err
	}
	if opts.ListOnly {
		return RollbackResult{Path: path, Backups: backups}, nil
	}
	if len(backups) == 0 {
		return RollbackResult{Path: path, Backups: backups, Error: "no backups available"}, nil
	}

	target := backups[0].Path // default: most recent
	if opts.Backup != "" {
		name := filepath.Base(opts.Backup) // defend against path traversal
		if !strings.HasPrefix(name, backupPrefix) {
			return RollbackResult{Path: path, Backups: backups, Error: fmt.Sprintf("backup %q does not match expected prefix", name)}, nil
		}
		target = filepath.Join(dir, name)
		if _, err := os.Stat(target); err != nil {
			return RollbackResult{Path: path, Backups: backups, Error: fmt.Sprintf("backup not found: %s", name)}, nil
		}
	}

	data, err := os.ReadFile(target)
	if err != nil {
		return RollbackResult{Path: path, Backups: backups}, err
	}
	if err := atomicWriteConfigFile(path, data); err != nil {
		return RollbackResult{Path: path, Backups: backups}, err
	}

	// Re-list so the response reflects post-rollback state.
	backupsAfter, _ := ListBackups(dir)
	return RollbackResult{
		Restored:        true,
		RestoredFrom:    filepath.Base(target),
		RequiresRestart: true,
		Backups:         backupsAfter,
		Path:            path,
	}, nil
}

// ── YAML node-tree patching ─────────────────────────────────────────────────

// patchYAMLNodes parses existing YAML bytes as a yaml.Node document, then
// sets/updates only the keys present in patch, leaving comments, key order,
// and all untouched keys intact. For scope "v3" the patch keys land under
// the v3: mapping (created only if at least one v3 key is patched). The
// updated document is marshalled back to bytes and returned.
func patchYAMLNodes(existing []byte, patch map[string]any, scope string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(existing, &doc); err != nil {
		return nil, fmt.Errorf("parse existing yaml: %w", err)
	}

	// yaml.Unmarshal with a *yaml.Node yields a DocumentNode wrapping the root.
	// Ensure we have a usable document + root mapping.
	if doc.Kind == 0 {
		// Empty file — build a fresh document node.
		doc = yaml.Node{
			Kind: yaml.DocumentNode,
			Content: []*yaml.Node{
				{Kind: yaml.MappingNode, Tag: "!!map"},
			},
		}
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("unexpected yaml node kind %v", doc.Kind)
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("root yaml node is not a mapping (kind=%v)", root.Kind)
	}

	switch scope {
	case "v3":
		// Find or create the v3: key inside root.
		v3Map := findMappingValue(root, "v3")
		if v3Map == nil {
			// Create a new v3: key with an empty mapping.
			keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "v3"}
			valNode := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			root.Content = append(root.Content, keyNode, valNode)
			v3Map = valNode
		}
		for k, v := range patch {
			setYAMLKey(v3Map, k, v)
		}
	default: // "top"
		for k, v := range patch {
			setYAMLKey(root, k, v)
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("re-encode yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("close yaml encoder: %w", err)
	}
	return buf.Bytes(), nil
}

// findMappingValue looks for a key in a yaml.MappingNode and returns its
// value node, or nil if not found.
func findMappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setYAMLKey sets key=val in a yaml.MappingNode. If the key already exists
// its value node is updated in place; otherwise a new key+value pair is
// appended. val==nil removes the key (RFC 7396 null → delete).
func setYAMLKey(m *yaml.Node, key string, val any) {
	// nil → remove the key from the mapping.
	if val == nil {
		for i := 0; i+1 < len(m.Content); i += 2 {
			if m.Content[i].Value == key {
				m.Content = append(m.Content[:i], m.Content[i+2:]...)
				return
			}
		}
		return
	}

	valNode := anyToYAMLNode(val)

	// Update existing.
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			// Preserve the existing key node (it may carry a comment).
			m.Content[i+1] = valNode
			return
		}
	}

	// Append new key+value.
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	m.Content = append(m.Content, keyNode, valNode)
}

// anyToYAMLNode converts a Go value to a yaml.Node suitable for embedding in a
// node tree. Supports scalars (string, bool, int, float64), map[string]any, and
// map[string]string (digest_paths).
func anyToYAMLNode(v any) *yaml.Node {
	switch val := v.(type) {
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: val}
	case bool:
		s := "false"
		if val {
			s = "true"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: s}
	case int:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprintf("%d", val)}
	case float64:
		// Prefer integer representation when there is no fractional part.
		if val == float64(int64(val)) {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprintf("%d", int64(val))}
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: fmt.Sprintf("%g", val)}
	case map[string]any:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		// Sort keys for stable output.
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			n.Content = append(n.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k},
				anyToYAMLNode(val[k]),
			)
		}
		return n
	case map[string]string:
		n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			n.Content = append(n.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: val[k]},
			)
		}
		return n
	case []string:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, s := range val {
			n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s})
		}
		return n
	case []any:
		n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, s := range val {
			n.Content = append(n.Content, anyToYAMLNode(s))
		}
		return n
	default:
		// Fallback: marshal via yaml then parse back to a node.
		raw, err := yaml.Marshal(v)
		if err != nil {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: fmt.Sprintf("%v", v)}
		}
		var node yaml.Node
		if err := yaml.Unmarshal(raw, &node); err != nil {
			return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: fmt.Sprintf("%v", v)}
		}
		if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
			return node.Content[0]
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: fmt.Sprintf("%v", v)}
	}
}

// buildMinimalYAML produces a minimal YAML document from the patch map alone,
// writing keys only at the relevant scope. No zero-value fields, no empty v3:
// block unless the scope is "v3".
func buildMinimalYAML(patch map[string]any, scope string) ([]byte, error) {
	root := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}

	switch scope {
	case "v3":
		if len(patch) > 0 {
			v3Val := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			for k, v := range patch {
				if v != nil {
					setYAMLKey(v3Val, k, v)
				}
			}
			if len(v3Val.Content) > 0 {
				root.Content = append(root.Content,
					&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "v3"},
					v3Val,
				)
			}
		}
	default:
		for k, v := range patch {
			if v != nil {
				setYAMLKey(root, k, v)
			}
		}
	}

	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ── Atomic Writer ───────────────────────────────────────────────────────────

// atomicWriteConfigFile writes data to path via temp-file + rename. Mirrors
// the pattern in the root-package `atomicWriteFile` (hook_working_memory.go)
// which lives in package main; we duplicate rather than cross-package-import
// because engine is an internal package with no access to main. Same semantics.
func atomicWriteConfigFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".kernel-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, werr := tmp.Write(data); werr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp: %w", werr)
	}
	if cerr := tmp.Close(); cerr != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp: %w", cerr)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename temp: %w", err)
	}
	return nil
}

// ── JSON helpers (decode raw patches while preserving null-vs-missing) ─────

// DecodePatchBody decodes a JSON body into a map[string]any, preserving the
// distinction between an absent key and an explicit null. Used by the HTTP
// handler; the MCP SDK already hands us a map.
func DecodePatchBody(data []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber() // makes later coerceInt / coerceFloat more predictable
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	// json.Number → float64 to match the MCP SDK's default decoding, so
	// downstream coerceInt handles both paths identically.
	return normalizeNumbers(m), nil
}

func normalizeNumbers(v any) map[string]any {
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	for k, val := range m {
		switch n := val.(type) {
		case json.Number:
			if i, err := n.Int64(); err == nil {
				m[k] = float64(i)
			} else if f, err := n.Float64(); err == nil {
				m[k] = f
			}
		case map[string]any:
			m[k] = normalizeNumbers(n)
		}
	}
	return m
}
