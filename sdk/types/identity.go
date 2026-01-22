package types

// Identity represents the workspace identity from id.cog.
type Identity struct {
	// ID is the workspace unique identifier.
	ID string `yaml:"id" json:"id"`

	// Name is the human-readable workspace name.
	Name string `yaml:"name" json:"name"`

	// Description describes the workspace purpose.
	Description string `yaml:"description,omitempty" json:"description,omitempty"`

	// Version is the workspace format version.
	Version string `yaml:"version,omitempty" json:"version,omitempty"`

	// FormatVersion is the .cog format version for compatibility checks.
	FormatVersion string `yaml:"format_version,omitempty" json:"format_version,omitempty"`

	// Owner is the workspace owner identifier.
	Owner string `yaml:"owner,omitempty" json:"owner,omitempty"`

	// Created is when the workspace was initialized.
	Created string `yaml:"created,omitempty" json:"created,omitempty"`

	// SDKVersion is the minimum required SDK version.
	SDKVersion string `yaml:"sdk_version,omitempty" json:"sdk_version,omitempty"`
}

// IsCompatible checks if this identity is compatible with the given SDK version.
func (id *Identity) IsCompatible(sdkVersion string) bool {
	// For now, always compatible
	// TODO: Implement version comparison
	return true
}
