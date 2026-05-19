// Package reconcile re-exports the public surface of pkg/reconcile as
// pkg/substrate/reconcile, per ADR-100 Step 2a.
//
// All symbols here are Go type aliases or variable/function aliases of their
// counterparts in github.com/myrgic/cogos/pkg/reconcile. Consumers of either
// import path get identical types at the language level — no conversion needed.
//
// This package adds no logic. It is a pure re-export layer so downstream code
// can migrate incrementally to the substrate import path without changing
// call sites or type assertions.
package reconcile

import reconcile "github.com/myrgic/cogos/pkg/reconcile"

// --- Status enums ---

type SyncStatus = reconcile.SyncStatus

const (
	SyncStatusSynced    = reconcile.SyncStatusSynced
	SyncStatusOutOfSync = reconcile.SyncStatusOutOfSync
	SyncStatusUnknown   = reconcile.SyncStatusUnknown
)

type HealthStatus = reconcile.HealthStatus

const (
	HealthHealthy     = reconcile.HealthHealthy
	HealthDegraded    = reconcile.HealthDegraded
	HealthProgressing = reconcile.HealthProgressing
	HealthMissing     = reconcile.HealthMissing
	HealthSuspended   = reconcile.HealthSuspended
)

type OperationPhase = reconcile.OperationPhase

const (
	OperationIdle    = reconcile.OperationIdle
	OperationSyncing = reconcile.OperationSyncing
	OperationWaiting = reconcile.OperationWaiting
)

// --- Composite status type ---

type ResourceStatus = reconcile.ResourceStatus

// --- Action / Resource enums ---

type ActionType = reconcile.ActionType

const (
	ActionCreate = reconcile.ActionCreate
	ActionUpdate = reconcile.ActionUpdate
	ActionDelete = reconcile.ActionDelete
	ActionSkip   = reconcile.ActionSkip
)

type ResourceMode = reconcile.ResourceMode

const (
	ModeManaged   = reconcile.ModeManaged
	ModeUnmanaged = reconcile.ModeUnmanaged
	ModeData      = reconcile.ModeData
)

type ApplyStatus = reconcile.ApplyStatus

const (
	ApplySucceeded = reconcile.ApplySucceeded
	ApplyFailed    = reconcile.ApplyFailed
	ApplySkipped   = reconcile.ApplySkipped
)

// --- Plan / Action / Summary / Result ---

type Plan    = reconcile.Plan
type Action  = reconcile.Action
type Summary = reconcile.Summary
type Result  = reconcile.Result

// --- State / Resource ---

type State    = reconcile.State
type Resource = reconcile.Resource

// --- Provider interfaces ---

type Reconcilable   = reconcile.Reconcilable
type Tokenable      = reconcile.Tokenable
type ConfigExporter = reconcile.ConfigExporter

// --- types.go helpers ---

var NewResourceStatus     = reconcile.NewResourceStatus
var ResourceIndex         = reconcile.ResourceIndex
var ResourceByExternalID  = reconcile.ResourceByExternalID

// --- state.go ---

var StatePath        = reconcile.StatePath
var LoadState        = reconcile.LoadState
var WriteState       = reconcile.WriteState
var NewState         = reconcile.NewState
var GenerateLineage  = reconcile.GenerateLineage

// --- registry.go ---

var RegisterProvider = reconcile.RegisterProvider
var GetProvider      = reconcile.GetProvider
var ListProviders    = reconcile.ListProviders
var HasProvider      = reconcile.HasProvider
var UpsertProvider   = reconcile.UpsertProvider
var ResetProviders   = reconcile.ResetProviders

// --- events.go constants ---

const (
	EventPlanStart    = reconcile.EventPlanStart
	EventPlanComplete = reconcile.EventPlanComplete
	EventApplyStart   = reconcile.EventApplyStart
	EventApplyAction  = reconcile.EventApplyAction
	EventApplyComplete = reconcile.EventApplyComplete
	EventDrift        = reconcile.EventDrift
	EventError        = reconcile.EventError
)

// --- events.go types / functions ---

type Event = reconcile.Event

var EmitEvent          = reconcile.EmitEvent
var EmitPlanStart      = reconcile.EmitPlanStart
var EmitPlanComplete   = reconcile.EmitPlanComplete
var EmitApplyStart     = reconcile.EmitApplyStart
var EmitApplyAction    = reconcile.EmitApplyAction
var EmitApplyComplete  = reconcile.EmitApplyComplete
var EmitDriftDetected  = reconcile.EmitDriftDetected
var EmitError          = reconcile.EmitError

// --- meta.go types ---

type MetaResource = reconcile.MetaResource
type MetaConfig   = reconcile.MetaConfig
type MetaResult   = reconcile.MetaResult
type MetaOpts     = reconcile.MetaOpts

// --- meta.go functions ---

var ResolveOrder          = reconcile.ResolveOrder
var AutoDiscoverResources = reconcile.AutoDiscoverResources
var RunMeta               = reconcile.RunMeta
var ConfigureProvider     = reconcile.ConfigureProvider
var ResolveToken          = reconcile.ResolveToken

// --- cogdoc_review_types.go types ---

type CogdocReviewClass       = reconcile.CogdocReviewClass
type CogdocProposal          = reconcile.CogdocProposal
type SimilarityCandidate     = reconcile.SimilarityCandidate
type AcknowledgmentDecision  = reconcile.AcknowledgmentDecision
type CandidateAcknowledgment = reconcile.CandidateAcknowledgment
type ReviewActionTaken       = reconcile.ReviewActionTaken
type ProvenanceRecord        = reconcile.ProvenanceRecord
type ReviewTRMTuple          = reconcile.ReviewTRMTuple

// --- cogdoc_review_types.go constants ---

const (
	AckReadDistinct    = reconcile.AckReadDistinct
	AckAmendInstead    = reconcile.AckAmendInstead
	ReviewActionAuthored  = reconcile.ReviewActionAuthored
	ReviewActionAmended   = reconcile.ReviewActionAmended
	ReviewActionAbandoned = reconcile.ReviewActionAbandoned
)

// --- cogdoc_review_types.go functions ---

var DefaultCogdocReviewClass = reconcile.DefaultCogdocReviewClass
