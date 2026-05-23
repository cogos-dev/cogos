// interfaces.go — Interfaces that the kernel engine must implement.
//
// These interfaces define the contracts between pkg/bep types and the kernel
// engine. The kernel implements these interfaces; consumers can depend on them
// without importing the engine.

package bep

// Engine defines the interface for the BEP transport engine.
// The kernel's BEPEngine implements this interface.
type Engine interface {
	// Start begins listening for connections and dialing peers.
	Start() error

	// Stop shuts down the engine, closing all connections.
	Stop()

	// SendToPeer sends a message to a specific peer.
	SendToPeer(peerID DeviceID, msgType MessageType, payload []byte)

	// BroadcastMessage sends a message to all connected peers.
	BroadcastMessage(msgType MessageType, payload []byte)

	// NotifyLocalChange is called when a local CRD file changes.
	NotifyLocalChange(filename string)

	// Status returns the current engine status.
	Status() EngineStatus
}

// SyncProvider defines the interface for the local file sync provider.
// The kernel's BEPProvider implements this interface.
type SyncProvider interface {
	// Start begins watching for CRD file changes.
	Start() error

	// Stop halts the file watcher.
	Stop()

	// OnFileChange sets the callback for CRD file changes.
	OnFileChange(fn func(filename string))

	// AddChangeHandler registers an additional change handler.
	AddChangeHandler(fn func(filename string))

	// ReceiveAgentCRD handles an agent CRD received from a remote peer.
	ReceiveAgentCRD(peerID string, filename string, data []byte) error

	// RemoveAgentCRD handles deletion of an agent CRD from a remote peer.
	RemoveAgentCRD(peerID string, filename string) error

	// ListPeers returns known peer nodes.
	ListPeers() []Peer

	// Status returns current sync state.
	Status() SyncStatus

	// History returns recent sync events.
	History() []ReceivedEvent
}
