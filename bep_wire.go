// bep_wire.go — root package shim.
// BEP framing (Wire type and size constants) have been extracted to
// pkg/substrate/bep (wire.go). This file re-exports them under the
// BEP-prefixed names used throughout root package main and its tests.

package main

import (
	bep "github.com/myrgic/cogos/pkg/substrate/bep"
)

const (
	bepMaxMessageSize = bep.MaxMessageSize
	bepMaxHelloSize   = bep.MaxHelloSize
)

// BEPWire is an alias for the canonical bep.Wire type.
type BEPWire = bep.Wire

// NewBEPWire wraps a connection with BEP framing.
var NewBEPWire = bep.NewWire
