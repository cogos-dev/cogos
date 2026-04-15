// Package bep provides types, interfaces, and protocol primitives for the
// Block Exchange Protocol (BEP) transport layer used by CogOS for cross-node
// agent CRD synchronization.
//
// This package contains the wire-compatible protocol types (matching Syncthing's
// bep.proto field numbers), TLS certificate management, DeviceID derivation,
// version vector logic, index types, event types, and the BEP framing layer.
//
// The BEP engine implementation (connection management, peer lifecycle, sync
// model) remains in the kernel and imports these types. This separation allows
// other packages to depend on BEP types without pulling in the full kernel.
//
// Extracted from apps/cogos/bep_*.go. The following files remain in the kernel:
//   - bep_engine.go: BEPEngine (coupled to kernel bus, provider, config)
//   - bep_model.go: AgentSyncModel (coupled to BEPEngine, BEPProvider)
//   - bep_provider.go: BEPProvider (coupled to fsnotify, AgentCRD, yaml)
//   - bep_receiver.go: receiver operations (coupled to AgentCRD validation)
package bep
