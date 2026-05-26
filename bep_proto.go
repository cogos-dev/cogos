// bep_proto.go — root package shim.
// BEP v1 message types have been extracted to pkg/substrate/bep (proto.go).
// This file re-exports them under the BEP-prefixed names used throughout root
// package main and its tests. All type aliases are identical types at the Go
// language level — methods, struct literals, and assignments are fully
// interchangeable with the canonical pkg/substrate/bep types.

package main

import (
	bep "github.com/myrgic/cogos/pkg/substrate/bep"
)

// ─── Message type enum ──────────────────────────────────────────────────────────

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

// ─── Proto type aliases ─────────────────────────────────────────────────────────

type BEPHello = bep.Hello
type BEPHeader = bep.Header
type BEPDevice = bep.Device
type BEPFolder = bep.Folder
type BEPClusterConfig = bep.ClusterConfig
type BEPCounter = bep.Counter
type BEPVector = bep.Vector
type BEPBlockInfo = bep.BlockInfo
type BEPFileInfo = bep.FileInfo
type BEPIndex = bep.Index
type BEPIndexUpdate = bep.Index // same wire format as Index
type BEPRequest = bep.Request
type BEPResponse = bep.Response
type BEPPing = bep.Ping
type BEPClose = bep.Close

// ─── Helper functions ───────────────────────────────────────────────────────────

// PBDecode and DecodeVarint are forwarded for root package consumers.
var PBDecode = bep.PBDecode
var DecodeVarint = bep.DecodeVarint
