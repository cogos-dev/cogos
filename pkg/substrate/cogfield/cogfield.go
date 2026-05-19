// Package cogfield re-exports the public surface of pkg/cogfield as
// pkg/substrate/cogfield, per ADR-100 Step 2b.
//
// All symbols here are Go type aliases or variable/function aliases of their
// counterparts in github.com/myrgic/cogos/pkg/cogfield. Consumers of either
// import path get identical types at the language level — no conversion needed.
//
// This package adds no logic. It is a pure re-export layer so downstream code
// can migrate incrementally to the substrate import path without changing
// call sites or type assertions.
package cogfield

import cogfield "github.com/myrgic/cogos/pkg/cogfield"

// --- Graph types ---

type Node  = cogfield.Node
type Edge  = cogfield.Edge
type Stats = cogfield.Stats
type Graph = cogfield.Graph

// --- Block types ---

type Block      = cogfield.Block
type GraphBlock = cogfield.GraphBlock

// --- Adapter types ---

type BlockAdapter       = cogfield.BlockAdapter
type AdapterNodeConfig  = cogfield.AdapterNodeConfig
type BlockTypeConfig    = cogfield.BlockTypeConfig
type ExpandNodeResponse = cogfield.ExpandNodeResponse

// --- Bus types ---

type BusDetail         = cogfield.BusDetail
type BusRegistryEntry  = cogfield.BusRegistryEntry

// --- Condition types ---

type FieldCondition       = cogfield.FieldCondition
type TriggeredCondition   = cogfield.TriggeredCondition
type FieldConditionState  = cogfield.FieldConditionState

// --- Document types ---

type DocRef         = cogfield.DocRef
type DocumentDetail = cogfield.DocumentDetail

// --- Event types ---

type SessionJSONLEvent = cogfield.SessionJSONLEvent

// --- Session types ---

type SessionMessage = cogfield.SessionMessage
type SessionDetail  = cogfield.SessionDetail

// --- Signal types ---

type SignalFieldState  = cogfield.SignalFieldState
type PersistedSignal   = cogfield.PersistedSignal

// --- Graph functions ---

var NormalizeEntityType = cogfield.NormalizeEntityType
var InferSector         = cogfield.InferSector
var StrengthFromMetrics = cogfield.StrengthFromMetrics
var ParseCSVSet         = cogfield.ParseCSVSet
var FilterNodes         = cogfield.FilterNodes
var BFSSubgraph         = cogfield.BFSSubgraph
var ComputeStats        = cogfield.ComputeStats
var FilterByMeta        = cogfield.FilterByMeta

// --- Adapter functions ---

var GraphBlockToNode = cogfield.GraphBlockToNode

// --- Condition functions ---

var ParseConditionQueryString  = cogfield.ParseConditionQueryString
var EvaluateFieldConditions    = cogfield.EvaluateFieldConditions

// --- Event functions ---

var ExtractTimestamp = cogfield.ExtractTimestamp

// --- Signal functions ---

var ComputeRelevance = cogfield.ComputeRelevance
var SignalIsActive   = cogfield.SignalIsActive
