// types.go
// Core reconciliation types, interfaces, and enums for the CogOS provider model.
// Providers (Discord, Agent, Workspace, etc.) implement Reconcilable to participate
// in the unified plan/apply/state lifecycle.

package reconcile

import "context"

// --- Status Enums (Argo-inspired three-axis model) ---

// SyncStatus indicates whether declared config matches live state.
type SyncStatus string

const (
	SyncStatusSynced    SyncStatus = "Synced"
	SyncStatusOutOfSync SyncStatus = "OutOfSync"
	SyncStatusUnknown   SyncStatus = "Unknown"
)

// HealthStatus indicates the health of the managed resource.
type HealthStatus string

const (
	HealthHealthy     HealthStatus = "Healthy"
	HealthDegraded    HealthStatus = "Degraded"
	HealthProgressing HealthStatus = "Progressing"
	HealthMissing     HealthStatus = "Missing"
	HealthSuspended   HealthStatus = "Suspended"
)

// OperationPhase indicates the current reconciliation operation.
type OperationPhase string

const (
	OperationIdle    OperationPhase = "Idle"
	OperationSyncing OperationPhase = "Syncing"
	OperationWaiting OperationPhase = "Waiting"
)

// ResourceStatus combines all three status axes for a single resource provider.
type ResourceStatus struct {
	Sync      SyncStatus     `json:"sync"`
	Health    HealthStatus   `json:"health"`
	Operation OperationPhase `json:"operation"`
	Message   string         `json:"message,omitempty"`
}

// --- Action and Resource Enums ---

// ActionType identifies what a plan action does.
type ActionType string

const (
	ActionCreate ActionType = "create"
	ActionUpdate ActionType = "update"
	ActionDelete ActionType = "delete"
	ActionSkip   ActionType = "skip"
)

// ResourceMode indicates how a resource is managed.
type ResourceMode string

const (
	ModeManaged   ResourceMode = "managed"
	ModeUnmanaged ResourceMode = "unmanaged"
	ModeData      ResourceMode = "data"
)

// ApplyStatus indicates the result of applying a single action.
type ApplyStatus string

const (
	ApplySucceeded ApplyStatus = "succeeded"
	ApplyFailed    ApplyStatus = "failed"
	ApplySkipped   ApplyStatus = "skipped"
)

// --- Generalized Plan Types ---

// Plan describes the set of changes needed to bring live state
// into alignment with declared config. Provider-agnostic.
type Plan struct {
	ResourceType string            `json:"resource_type"`
	GeneratedAt  string            `json:"generated_at"`
	ConfigPath   string            `json:"config_path"`
	Actions      []Action          `json:"actions"`
	Summary      Summary           `json:"summary"`
	Warnings     []string          `json:"warnings"`
	Metadata     map[string]any    `json:"metadata,omitempty"`
}

// Action describes a single create/update/delete/skip operation.
type Action struct {
	Action       ActionType     `json:"action"`
	ResourceType string         `json:"resource_type"`
	Name         string         `json:"name"`
	Details      map[string]any `json:"details"`
}

// Summary counts actions by type.
type Summary struct {
	Creates int `json:"creates"`
	Updates int `json:"updates"`
	Deletes int `json:"deletes"`
	Skipped int `json:"skipped"`
}

// Total returns the total number of actions.
func (s Summary) Total() int {
	return s.Creates + s.Updates + s.Deletes + s.Skipped
}

// HasChanges returns true if there are any non-skip actions.
func (s Summary) HasChanges() bool {
	return s.Creates > 0 || s.Updates > 0 || s.Deletes > 0
}

// --- Generalized Apply Types ---

// Result records the outcome of executing a single plan action.
type Result struct {
	Phase     string      `json:"phase"`
	Action    string      `json:"action"`
	Name      string      `json:"name"`
	Status    ApplyStatus `json:"status"`
	Error     string      `json:"error,omitempty"`
	CreatedID string      `json:"created_id,omitempty"`
}

// --- Generalized State Types ---

// State tracks the last-known state of managed resources.
// Modeled after Terraform state: version, lineage, serial, resources.
type State struct {
	Version      int               `json:"version"`
	Lineage      string            `json:"lineage"`
	Serial       int               `json:"serial"`
	ResourceType string            `json:"resource_type"`
	GeneratedAt  string            `json:"generated_at"`
	Resources    []Resource        `json:"resources"`
	Metadata     map[string]any    `json:"metadata,omitempty"`
}

// Resource describes a single tracked resource within state.
type Resource struct {
	Address         string         `json:"address"`
	Type            string         `json:"type"`
	Mode            ResourceMode   `json:"mode"`
	ExternalID      string         `json:"external_id"`
	Name            string         `json:"name"`
	ParentAddress   string         `json:"parent_address,omitempty"`
	ParentID        string         `json:"parent_id,omitempty"`
	Attributes      map[string]any `json:"attributes,omitempty"`
	UnmanagedReason string         `json:"unmanaged_reason,omitempty"`
	LastRefreshed   string         `json:"last_refreshed"`
}

// --- Provider Interface ---

// Reconcilable is the contract all resource providers implement.
// Each provider manages one resource type (Discord, Agent, Workspace, etc.)
// through the standard plan/apply/state lifecycle.
type Reconcilable interface {
	// Type returns the resource type identifier (e.g., "discord", "agent", "workspace").
	Type() string

	// LoadConfig loads the declared configuration from the workspace.
	LoadConfig(root string) (any, error)

	// FetchLive retrieves the current live state from the external system.
	FetchLive(ctx context.Context, config any) (any, error)

	// ComputePlan compares declared config against live state to produce a plan.
	ComputePlan(config any, live any, state *State) (*Plan, error)

	// ApplyPlan executes the planned changes against the external system.
	ApplyPlan(ctx context.Context, plan *Plan) ([]Result, error)

	// BuildState constructs state from live data (for snapshot/import).
	BuildState(config any, live any, existing *State) (*State, error)

	// Health returns the current three-axis status.
	Health() ResourceStatus
}

// Tokenable is an optional interface for providers that need auth tokens.
// Providers that implement this can receive tokens from --token flags or
// environment variables ({TYPE}_TOKEN, e.g. DISCORD_BOT_TOKEN).
type Tokenable interface {
	SetToken(token string)
}

// ConfigExporter is an optional interface for providers that can generate
// a declared config file (e.g., config.yaml) from live state.
type ConfigExporter interface {
	ExportConfig(root string) error
}

// --- Helpers ---

// NewResourceStatus creates a ResourceStatus with defaults.
func NewResourceStatus(sync SyncStatus, health HealthStatus) ResourceStatus {
	return ResourceStatus{
		Sync:      sync,
		Health:    health,
		Operation: OperationIdle,
	}
}

// ResourceIndex returns a map from address to resource for fast lookup.
func ResourceIndex(state *State) map[string]*Resource {
	if state == nil {
		return nil
	}
	idx := make(map[string]*Resource, len(state.Resources))
	for i := range state.Resources {
		idx[state.Resources[i].Address] = &state.Resources[i]
	}
	return idx
}

// ResourceByExternalID returns a map from external ID to resource.
func ResourceByExternalID(state *State) map[string]*Resource {
	if state == nil {
		return nil
	}
	idx := make(map[string]*Resource, len(state.Resources))
	for i := range state.Resources {
		if state.Resources[i].ExternalID != "" {
			idx[state.Resources[i].ExternalID] = &state.Resources[i]
		}
	}
	return idx
}
