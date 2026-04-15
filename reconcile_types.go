// reconcile_types.go
// Thin re-export layer: all reconciliation types and interfaces are defined
// in pkg/reconcile and aliased here for backward compatibility within the kernel.

package main

import "github.com/cogos-dev/cogos/pkg/reconcile"

// --- Type aliases for backward compatibility ---

type SyncStatus = reconcile.SyncStatus
type HealthStatus = reconcile.HealthStatus
type OperationPhase = reconcile.OperationPhase
type ResourceStatus = reconcile.ResourceStatus
type ActionType = reconcile.ActionType
type ResourceMode = reconcile.ResourceMode
type ApplyStatus = reconcile.ApplyStatus
type ReconcilePlan = reconcile.Plan
type ReconcileAction = reconcile.Action
type ReconcileSummary = reconcile.Summary
type ReconcileResult = reconcile.Result
type ReconcileState = reconcile.State
type ReconcileResource = reconcile.Resource
type Reconcilable = reconcile.Reconcilable
type Tokenable = reconcile.Tokenable
type ConfigExporter = reconcile.ConfigExporter

// --- Re-exported constants ---

const (
	SyncStatusSynced    = reconcile.SyncStatusSynced
	SyncStatusOutOfSync = reconcile.SyncStatusOutOfSync
	SyncStatusUnknown   = reconcile.SyncStatusUnknown

	HealthHealthy     = reconcile.HealthHealthy
	HealthDegraded    = reconcile.HealthDegraded
	HealthProgressing = reconcile.HealthProgressing
	HealthMissing     = reconcile.HealthMissing
	HealthSuspended   = reconcile.HealthSuspended

	OperationIdle    = reconcile.OperationIdle
	OperationSyncing = reconcile.OperationSyncing
	OperationWaiting = reconcile.OperationWaiting

	ActionCreate = reconcile.ActionCreate
	ActionUpdate = reconcile.ActionUpdate
	ActionDelete = reconcile.ActionDelete
	ActionSkip   = reconcile.ActionSkip

	ModeManaged   = reconcile.ModeManaged
	ModeUnmanaged = reconcile.ModeUnmanaged
	ModeData      = reconcile.ModeData

	ApplySucceeded = reconcile.ApplySucceeded
	ApplyFailed    = reconcile.ApplyFailed
	ApplySkipped   = reconcile.ApplySkipped
)

// --- Re-exported functions ---

var (
	NewResourceStatus         = reconcile.NewResourceStatus
	ReconcileResourceIndex    = reconcile.ResourceIndex
	ReconcileResourceByExternalID = reconcile.ResourceByExternalID
)
