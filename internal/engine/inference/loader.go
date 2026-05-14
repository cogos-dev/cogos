package inference

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// coreInferenceConfigFile is the canonical path relative to workspaceRoot.
const coreInferenceConfigFile = ".cog/config/core-inference.yaml"

// coreInferenceYAML is the on-disk YAML shape. It wraps CoreInferenceConfig
// directly, since the file contains the contract fields at the top level.
type coreInferenceYAML struct {
	Tiers       []TierProfile `yaml:"tiers"`
	DefaultRole string        `yaml:"default_role,omitempty"`
}

// LoadCoreInferenceConfig reads the core inference contract from
// workspaceRoot/.cog/config/core-inference.yaml.
//
// If the file is absent, DefaultCoreInferenceConfig() is returned with no error.
// If the file is present but Tiers is empty after parsing, DefaultCoreInferenceConfig()
// is returned (guards against an empty/stub config file).
// Any other read or parse error is returned as-is.
func LoadCoreInferenceConfig(workspaceRoot string) (CoreInferenceConfig, error) {
	path := filepath.Join(workspaceRoot, coreInferenceConfigFile)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return DefaultCoreInferenceConfig(), nil
		}
		return CoreInferenceConfig{}, fmt.Errorf("load core-inference.yaml: %w", err)
	}

	var raw coreInferenceYAML
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return CoreInferenceConfig{}, fmt.Errorf("parse core-inference.yaml: %w", err)
	}

	if len(raw.Tiers) == 0 {
		// File exists but contains no tiers — treat as absent.
		return DefaultCoreInferenceConfig(), nil
	}

	return CoreInferenceConfig{
		Tiers:       raw.Tiers,
		DefaultRole: raw.DefaultRole,
	}, nil
}
