// Package bep is the legacy import path for what is now the canonical
// package at github.com/myrgic/cogos/pkg/substrate/bep.
//
// Per ADR-100, the substrate layer is the architectural source of truth.
// As of the canonical-vs-shim inversion (2026-05-23), the source-of-truth
// implementation lives at pkg/substrate/bep/. This package retains the
// legacy import path as a thin re-export shim so external consumers that
// still import the legacy path continue to compile without source changes.
//
// All symbols here are Go type aliases or variable aliases of their
// counterparts in github.com/myrgic/cogos/pkg/substrate/bep. Consumers of
// either import path get identical types at the language level.
//
// This shim adds no logic. New code should import the substrate path directly.
package bep

import bep "github.com/myrgic/cogos/pkg/substrate/bep"

// --- Version vector ordering ---

type Ordering = bep.Ordering

const (
	OrderEqual      = bep.OrderEqual
	OrderGreater    = bep.OrderGreater
	OrderLesser     = bep.OrderLesser
	OrderConcurrent = bep.OrderConcurrent
)

// --- Proto message type enums ---

type MessageType = bep.MessageType
type MessageCompression = bep.MessageCompression
type ErrorCode = bep.ErrorCode

const (
	MessageTypeClusterConfig MessageType = bep.MessageTypeClusterConfig
	MessageTypeIndex         MessageType = bep.MessageTypeIndex
	MessageTypeIndexUpdate   MessageType = bep.MessageTypeIndexUpdate
	MessageTypeRequest       MessageType = bep.MessageTypeRequest
	MessageTypeResponse      MessageType = bep.MessageTypeResponse
	MessageTypePing          MessageType = bep.MessageTypePing
	MessageTypeClose         MessageType = bep.MessageTypeClose
)

const (
	CompressionNone MessageCompression = bep.CompressionNone
)

const (
	ErrorCodeNoError     ErrorCode = bep.ErrorCodeNoError
	ErrorCodeGeneric     ErrorCode = bep.ErrorCodeGeneric
	ErrorCodeNoSuchFile  ErrorCode = bep.ErrorCodeNoSuchFile
	ErrorCodeInvalidFile ErrorCode = bep.ErrorCodeInvalidFile
)

// BEPMagic is the BEP Hello magic number.
const BEPMagic uint32 = bep.BEPMagic

// --- Wire size constants ---

const (
	MaxMessageSize = bep.MaxMessageSize
	MaxHelloSize   = bep.MaxHelloSize
)

// --- DeviceID ---

type DeviceID = bep.DeviceID

// --- Config types ---

type Peer = bep.Peer
type Config = bep.Config
type SyncStatus = bep.SyncStatus
type EngineStatus = bep.EngineStatus
type PeerStatusSummary = bep.PeerStatusSummary
type ReceivedEvent = bep.ReceivedEvent

// --- Index types ---

type VersionVector = bep.VersionVector
type IndexEntry = bep.IndexEntry
type DiffResult = bep.DiffResult
type PersistedIndex = bep.PersistedIndex

// --- Proto types ---

type Hello = bep.Hello
type Header = bep.Header
type Device = bep.Device
type Folder = bep.Folder
type ClusterConfig = bep.ClusterConfig
type Counter = bep.Counter
type Vector = bep.Vector
type BlockInfo = bep.BlockInfo
type FileInfo = bep.FileInfo
type Index = bep.Index
type IndexUpdate = bep.IndexUpdate
type Request = bep.Request
type Response = bep.Response
type Ping = bep.Ping
type Close = bep.Close

// --- Wire type ---

type Wire = bep.Wire

// --- Event types ---

type SyncEvent = bep.SyncEvent

// --- Event constants ---

const (
	SyncEventPeerConnected    = bep.SyncEventPeerConnected
	SyncEventPeerDisconnected = bep.SyncEventPeerDisconnected
	SyncEventFileReceived     = bep.SyncEventFileReceived
	SyncEventFileSent         = bep.SyncEventFileSent
	SyncEventConflict         = bep.SyncEventConflict
	SyncEventIndexComplete    = bep.SyncEventIndexComplete
	SyncEventEngineStarted    = bep.SyncEventEngineStarted
	SyncEventEngineStopped    = bep.SyncEventEngineStopped
)

// --- Interface types ---

type Engine = bep.Engine
type SyncProvider = bep.SyncProvider

// --- Version vector functions ---

var NewVersionVector = bep.NewVersionVector
var VersionVectorFromBEP = bep.VersionVectorFromBEP

// --- Index functions ---

var IndexEntryFromBEP = bep.IndexEntryFromBEP
var IsAgentCRDFile = bep.IsAgentCRDFile
var ScanLocalIndex = bep.ScanLocalIndex
var DiffIndex = bep.DiffIndex
var PersistIndex = bep.PersistIndex
var LoadPersistedIndex = bep.LoadPersistedIndex
var ShortIDFromDeviceID = bep.ShortIDFromDeviceID

// --- TLS / DeviceID functions ---

var GenerateBEPCert = bep.GenerateBEPCert
var LoadBEPCert = bep.LoadBEPCert
var DeviceIDFromCert = bep.DeviceIDFromCert
var DeviceIDFromTLSCert = bep.DeviceIDFromTLSCert
var FormatDeviceID = bep.FormatDeviceID
var ParseDeviceID = bep.ParseDeviceID
var TLSConfig = bep.TLSConfig
var CertDir = bep.CertDir
var ExpandCertDir = bep.ExpandCertDir

// --- Proto functions ---

var PBDecode = bep.PBDecode
var DecodeVarint = bep.DecodeVarint

// --- Wire functions ---

var NewWire = bep.NewWire

// --- Event functions ---

var EmitSyncEvent = bep.EmitSyncEvent
var EmitPeerConnected = bep.EmitPeerConnected
var EmitPeerDisconnected = bep.EmitPeerDisconnected
var EmitFileReceived = bep.EmitFileReceived
var EmitFileSent = bep.EmitFileSent
var EmitSyncConflict = bep.EmitSyncConflict
var EmitIndexComplete = bep.EmitIndexComplete
var EmitEngineStarted = bep.EmitEngineStarted
var EmitEngineStopped = bep.EmitEngineStopped
