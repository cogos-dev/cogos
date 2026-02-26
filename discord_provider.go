// discord_provider.go
// DiscordProvider implements the Reconcilable interface for Discord server
// infrastructure reconciliation. This is a thin adapter layer that delegates
// to the existing functions in discord_reconcile.go.

package main

import (
	"context"
	"fmt"
	"time"
)

// DiscordLiveState bundles the live Discord data fetched from the API.
type DiscordLiveState struct {
	Channels []DiscordChannel
	Roles    []DiscordRole
}

// DiscordProvider implements Reconcilable for Discord server management.
type DiscordProvider struct {
	// Token is the Discord bot token. Set before calling FetchLive/ApplyPlan.
	Token string
}

// Type returns "discord".
func (d *DiscordProvider) Type() string { return "discord" }

// SetToken implements Tokenable for generic CLI dispatch.
func (d *DiscordProvider) SetToken(token string) { d.Token = token }

// LoadConfig loads the Discord server config (HCL-first, YAML fallback).
// Returns *DiscordServerConfig as any.
func (d *DiscordProvider) LoadConfig(root string) (any, error) {
	cfg, _, err := loadDiscordServerConfig(root)
	return cfg, err
}

// FetchLive retrieves the current Discord server state from the API.
// Config must be *DiscordServerConfig. Returns *DiscordLiveState as any.
func (d *DiscordProvider) FetchLive(ctx context.Context, config any) (any, error) {
	cfg, ok := config.(*DiscordServerConfig)
	if !ok {
		return nil, fmt.Errorf("discord: expected *DiscordServerConfig, got %T", config)
	}

	if d.Token == "" {
		return nil, fmt.Errorf("discord: bot token not set")
	}

	client := newDiscordClient(d.Token, cfg.Reconciler.MaxAPICalls)

	channels, err := client.fetchChannels(cfg.Guild.ID)
	if err != nil {
		return nil, fmt.Errorf("discord: fetching channels: %w", err)
	}

	roles, err := client.fetchRoles(cfg.Guild.ID)
	if err != nil {
		return nil, fmt.Errorf("discord: fetching roles: %w", err)
	}

	return &DiscordLiveState{Channels: channels, Roles: roles}, nil
}

// ComputePlan compares declared config against live state to produce a plan.
// Config must be *DiscordServerConfig, live must be *DiscordLiveState.
// Returns *ReconcilePlan (wrapping the Discord-specific Plan).
func (d *DiscordProvider) ComputePlan(config any, live any, state *ReconcileState) (*ReconcilePlan, error) {
	cfg, ok := config.(*DiscordServerConfig)
	if !ok {
		return nil, fmt.Errorf("discord: expected *DiscordServerConfig, got %T", config)
	}
	liveState, ok := live.(*DiscordLiveState)
	if !ok {
		return nil, fmt.Errorf("discord: expected *DiscordLiveState, got %T", live)
	}

	// Convert generic state to Discord state for the existing function
	var discordState *DiscordState
	if state != nil {
		discordState = reconcileStateToDiscordState(state)
	}

	plan := computePlanWithState(cfg, liveState.Channels, liveState.Roles, discordState)

	// Convert Discord Plan to generic ReconcilePlan
	return discordPlanToReconcilePlan(plan), nil
}

// ApplyPlan executes the planned changes against Discord.
func (d *DiscordProvider) ApplyPlan(ctx context.Context, plan *ReconcilePlan) ([]ReconcileResult, error) {
	if d.Token == "" {
		return nil, fmt.Errorf("discord: bot token not set")
	}

	// Convert generic plan back to Discord plan
	discordPlan := reconcilePlanToDiscordPlan(plan)

	guildID, _ := plan.Metadata["guild_id"].(string)
	if guildID == "" {
		return nil, fmt.Errorf("discord: guild_id not found in plan metadata")
	}

	maxCalls := 60
	if mc, ok := plan.Metadata["max_api_calls"].(float64); ok {
		maxCalls = int(mc)
	}

	client := newDiscordClient(d.Token, maxCalls)

	// Need roles and channels for apply (name→ID resolution)
	roles, err := client.fetchRoles(guildID)
	if err != nil {
		return nil, fmt.Errorf("discord: fetching roles for apply: %w", err)
	}
	channels, err := client.fetchChannels(guildID)
	if err != nil {
		return nil, fmt.Errorf("discord: fetching channels for apply: %w", err)
	}

	results, err := applyPlan(client, discordPlan, guildID, roles, channels)
	if err != nil {
		return nil, err
	}

	// Convert to generic results
	genericResults := make([]ReconcileResult, len(results))
	for i, r := range results {
		genericResults[i] = ReconcileResult{
			Phase:     r.Phase,
			Action:    r.Action,
			Name:      r.Name,
			Status:    ApplyStatus(r.Status),
			Error:     r.Error,
			CreatedID: r.CreatedID,
		}
	}
	return genericResults, nil
}

// BuildState constructs state from live data for snapshot/import.
func (d *DiscordProvider) BuildState(config any, live any, existing *ReconcileState) (*ReconcileState, error) {
	cfg, ok := config.(*DiscordServerConfig)
	if !ok {
		return nil, fmt.Errorf("discord: expected *DiscordServerConfig, got %T", config)
	}
	liveState, ok := live.(*DiscordLiveState)
	if !ok {
		return nil, fmt.Errorf("discord: expected *DiscordLiveState, got %T", live)
	}

	var existingDiscord *DiscordState
	if existing != nil {
		existingDiscord = reconcileStateToDiscordState(existing)
	}

	ds := buildStateFromLive(cfg.Guild.ID, cfg, liveState.Channels, liveState.Roles, existingDiscord)
	return discordStateToReconcileState(ds), nil
}

// Health returns the current three-axis status for the Discord provider.
func (d *DiscordProvider) Health() ResourceStatus {
	if d.Token == "" {
		return ResourceStatus{
			Sync:      SyncStatusUnknown,
			Health:    HealthMissing,
			Operation: OperationIdle,
			Message:   "no bot token configured",
		}
	}
	return NewResourceStatus(SyncStatusUnknown, HealthHealthy)
}

// --- Conversion helpers between Discord-specific and generic types ---

func discordPlanToReconcilePlan(plan *Plan) *ReconcilePlan {
	actions := make([]ReconcileAction, len(plan.Actions))
	for i, a := range plan.Actions {
		actions[i] = ReconcileAction{
			Action:       ActionType(a.Action),
			ResourceType: a.ResourceType,
			Name:         a.Name,
			Details:      a.Details,
		}
	}
	return &ReconcilePlan{
		ResourceType: "discord",
		GeneratedAt:  plan.GeneratedAt,
		ConfigPath:   plan.ConfigPath,
		Actions:      actions,
		Summary: ReconcileSummary{
			Creates: plan.Summary.Creates,
			Updates: plan.Summary.Updates,
			Deletes: plan.Summary.Deletes,
			Skipped: plan.Summary.Skipped,
		},
		Warnings: plan.Warnings,
		Metadata: map[string]any{
			"guild_id":   plan.GuildID,
			"guild_name": plan.GuildName,
		},
	}
}

func reconcilePlanToDiscordPlan(plan *ReconcilePlan) *Plan {
	actions := make([]PlanAction, len(plan.Actions))
	for i, a := range plan.Actions {
		actions[i] = PlanAction{
			Action:       string(a.Action),
			ResourceType: a.ResourceType,
			Name:         a.Name,
			Details:      a.Details,
		}
	}
	guildID, _ := plan.Metadata["guild_id"].(string)
	guildName, _ := plan.Metadata["guild_name"].(string)
	return &Plan{
		GuildID:     guildID,
		GuildName:   guildName,
		GeneratedAt: plan.GeneratedAt,
		ConfigPath:  plan.ConfigPath,
		Actions:     actions,
		Summary: PlanSummary{
			Creates: plan.Summary.Creates,
			Updates: plan.Summary.Updates,
			Deletes: plan.Summary.Deletes,
			Skipped: plan.Summary.Skipped,
		},
		Warnings: plan.Warnings,
	}
}

func discordStateToReconcileState(ds *DiscordState) *ReconcileState {
	resources := make([]ReconcileResource, len(ds.Resources))
	for i, r := range ds.Resources {
		resources[i] = ReconcileResource{
			Address:         r.Address,
			Type:            r.Type,
			Mode:            ResourceMode(r.Mode),
			ExternalID:      r.DiscordID,
			Name:            r.Name,
			ParentAddress:   r.ParentAddress,
			ParentID:        r.ParentID,
			Attributes:      r.Attributes,
			UnmanagedReason: r.UnmanagedReason,
			LastRefreshed:   r.LastRefreshed,
		}
	}
	return &ReconcileState{
		Version:      ds.Version,
		Lineage:      ds.Lineage,
		Serial:       ds.Serial,
		ResourceType: "discord",
		GeneratedAt:  ds.GeneratedAt,
		Resources:    resources,
		Metadata:     map[string]any{"guild_id": ds.GuildID},
	}
}

func reconcileStateToDiscordState(state *ReconcileState) *DiscordState {
	resources := make([]StateResource, len(state.Resources))
	for i, r := range state.Resources {
		resources[i] = StateResource{
			Address:         r.Address,
			Type:            r.Type,
			Mode:            string(r.Mode),
			DiscordID:       r.ExternalID,
			Name:            r.Name,
			ParentAddress:   r.ParentAddress,
			ParentID:        r.ParentID,
			Attributes:      r.Attributes,
			UnmanagedReason: r.UnmanagedReason,
			LastRefreshed:   r.LastRefreshed,
		}
	}
	guildID, _ := state.Metadata["guild_id"].(string)
	return &DiscordState{
		Version:     state.Version,
		Lineage:     state.Lineage,
		Serial:      state.Serial,
		GuildID:     guildID,
		GeneratedAt: state.GeneratedAt,
		Resources:   resources,
	}
}

// init registers the Discord provider with the global registry.
func init() {
	RegisterProvider("discord", &DiscordProvider{})
}

// ResolveDiscordToken resolves the bot token from flag or environment.
// Exported for use by verb dispatchers.
func ResolveDiscordToken(root, flagToken string) (string, error) {
	return resolveToken(root, flagToken)
}

// SetupDiscordProvider creates a DiscordProvider with a resolved token.
func SetupDiscordProvider(root, flagToken string) (*DiscordProvider, error) {
	token, err := resolveToken(root, flagToken)
	if err != nil {
		return nil, err
	}
	return &DiscordProvider{Token: token}, nil
}

// DiscordProviderHealthCheck performs a live health check by pinging the Discord API.
func DiscordProviderHealthCheck(token string) ResourceStatus {
	if token == "" {
		return ResourceStatus{
			Sync:      SyncStatusUnknown,
			Health:    HealthMissing,
			Operation: OperationIdle,
			Message:   "no bot token",
		}
	}

	client := newDiscordClient(token, 1)
	_, err := client.get("/users/@me")
	if err != nil {
		return ResourceStatus{
			Sync:      SyncStatusUnknown,
			Health:    HealthDegraded,
			Operation: OperationIdle,
			Message:   fmt.Sprintf("API unreachable: %v", err),
		}
	}
	return ResourceStatus{
		Sync:      SyncStatusUnknown,
		Health:    HealthHealthy,
		Operation: OperationIdle,
		Message:   fmt.Sprintf("API reachable (checked %s)", time.Now().UTC().Format(time.RFC3339)),
	}
}
