// bep_receiver.go — root package shim.
// BEPProvider receiver methods (ReceiveAgentCRD, RemoveAgentCRD, History) and
// the ReceivedEvent type have been extracted to internal/engine as part of
// Phase 2 S1 (issue #330).
//
// ReceivedEvent is re-exported here as a type alias for root package consumers.
// The BEPProvider methods are defined on the type in internal/engine and are
// accessible via the BEPProvider type alias in bep_provider.go.

package main

import (
	bep "github.com/myrgic/cogos/pkg/substrate/bep"
	engine "github.com/myrgic/cogos/internal/engine"
)

// ReceivedEvent is an alias for the canonical type in pkg/substrate/bep.
type ReceivedEvent = bep.ReceivedEvent

// receiverMaxHistory is re-exported from internal/engine for root-package tests.
const receiverMaxHistory = engine.ReceiverMaxHistory
