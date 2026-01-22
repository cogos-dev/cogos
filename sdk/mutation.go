package sdk

import "encoding/json"

// MutationOp defines the type of mutation to perform.
type MutationOp string

const (
	// MutationSet replaces the entire content.
	MutationSet MutationOp = "set"

	// MutationPatch merges with existing content (for JSON/YAML).
	MutationPatch MutationOp = "patch"

	// MutationAppend adds to a collection.
	MutationAppend MutationOp = "append"

	// MutationDelete removes the resource.
	MutationDelete MutationOp = "delete"
)

// Mutation describes a change to be made to a resource.
//
// Mutations are the write-side counterpart to Resources.
// They are validated by the projector before being applied.
type Mutation struct {
	// Op is the type of mutation.
	Op MutationOp `json:"op"`

	// Content is the new content (for Set, Patch, Append).
	Content []byte `json:"content,omitempty"`

	// Metadata contains additional mutation parameters.
	// For example, {"validate": true} to enforce cogdoc validation.
	Metadata map[string]any `json:"metadata,omitempty"`
}

// NewSetMutation creates a mutation that replaces content.
func NewSetMutation(content []byte) *Mutation {
	return &Mutation{
		Op:      MutationSet,
		Content: content,
	}
}

// NewSetJSONMutation creates a mutation that replaces content with JSON.
func NewSetJSONMutation(data any) (*Mutation, error) {
	content, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	return &Mutation{
		Op:      MutationSet,
		Content: content,
	}, nil
}

// NewPatchMutation creates a mutation that merges with existing content.
func NewPatchMutation(patch []byte) *Mutation {
	return &Mutation{
		Op:      MutationPatch,
		Content: patch,
	}
}

// NewAppendMutation creates a mutation that adds to a collection.
func NewAppendMutation(item []byte) *Mutation {
	return &Mutation{
		Op:      MutationAppend,
		Content: item,
	}
}

// NewDeleteMutation creates a mutation that removes a resource.
func NewDeleteMutation() *Mutation {
	return &Mutation{
		Op: MutationDelete,
	}
}

// WithMetadata adds metadata to the mutation.
func (m *Mutation) WithMetadata(key string, value any) *Mutation {
	if m.Metadata == nil {
		m.Metadata = make(map[string]any)
	}
	m.Metadata[key] = value
	return m
}

// IsSet returns true if this is a Set mutation.
func (m *Mutation) IsSet() bool { return m.Op == MutationSet }

// IsPatch returns true if this is a Patch mutation.
func (m *Mutation) IsPatch() bool { return m.Op == MutationPatch }

// IsAppend returns true if this is an Append mutation.
func (m *Mutation) IsAppend() bool { return m.Op == MutationAppend }

// IsDelete returns true if this is a Delete mutation.
func (m *Mutation) IsDelete() bool { return m.Op == MutationDelete }
