// Package cogfield is the legacy import path for what is now the canonical
// package at github.com/myrgic/cogos/pkg/substrate/cogfield.
//
// Per ADR-100, the substrate layer is the architectural source of truth.
// As of the canonical-vs-shim inversion (2026-05-23), the source-of-truth
// implementation lives at pkg/substrate/cogfield/. This package retains the
// legacy import path as a thin re-export shim so external consumers that
// still import the legacy path continue to compile without source changes.
//
// All symbols here are Go type aliases or variable aliases of their
// counterparts in github.com/myrgic/cogos/pkg/substrate/cogfield. Consumers
// of either import path get identical types at the language level.
//
// This shim adds no logic. New code should import the substrate path directly.
package cogfield

import cogfield "github.com/myrgic/cogos/pkg/substrate/cogfield"

// --- Graph types ---

type Node = cogfield.Node
type Edge = cogfield.Edge
type Stats = cogfield.Stats
type Graph = cogfield.Graph

// --- Block types ---

type Block = cogfield.Block
type GraphBlock = cogfield.GraphBlock

// --- Adapter types ---

type BlockAdapter = cogfield.BlockAdapter
type AdapterNodeConfig = cogfield.AdapterNodeConfig
type BlockTypeConfig = cogfield.BlockTypeConfig
type ExpandNodeResponse = cogfield.ExpandNodeResponse

// --- Bus types ---

type BusDetail = cogfield.BusDetail
type BusRegistryEntry = cogfield.BusRegistryEntry

// --- Condition types ---

type FieldCondition = cogfield.FieldCondition
type TriggeredCondition = cogfield.TriggeredCondition
type FieldConditionState = cogfield.FieldConditionState

// --- Document types ---

type DocRef = cogfield.DocRef
type DocumentDetail = cogfield.DocumentDetail

// --- Event types ---

type SessionJSONLEvent = cogfield.SessionJSONLEvent

// --- Session types ---

type SessionMessage = cogfield.SessionMessage
type SessionDetail = cogfield.SessionDetail

// --- Signal types ---

type SignalFieldState = cogfield.SignalFieldState
type PersistedSignal = cogfield.PersistedSignal

// --- Graph functions ---

var NormalizeEntityType = cogfield.NormalizeEntityType
var InferSector = cogfield.InferSector
var StrengthFromMetrics = cogfield.StrengthFromMetrics
var ParseCSVSet = cogfield.ParseCSVSet
var FilterNodes = cogfield.FilterNodes
var BFSSubgraph = cogfield.BFSSubgraph
var ComputeStats = cogfield.ComputeStats
var FilterByMeta = cogfield.FilterByMeta

// --- Adapter functions ---

var GraphBlockToNode = cogfield.GraphBlockToNode

// --- Condition functions ---

var ParseConditionQueryString = cogfield.ParseConditionQueryString
var EvaluateFieldConditions = cogfield.EvaluateFieldConditions

// --- Event functions ---

var ExtractTimestamp = cogfield.ExtractTimestamp

// --- Signal functions ---

var ComputeRelevance = cogfield.ComputeRelevance
var SignalIsActive = cogfield.SignalIsActive
