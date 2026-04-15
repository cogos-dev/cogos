// Package modality provides the bus and pipeline abstractions for multi-modal
// communication in CogOS.
//
// A modality bus routes typed messages (text, voice, structured data) between
// producers and consumers through a Gate/Decoder/Encoder pipeline. Each sensory
// modality (text, voice, vision, spatial) implements the Module interface.
//
// The package is organized in layers:
//
//   - types.go: Core interfaces (Module, Gate, Decoder, Encoder) and data types
//   - bus.go: Bus orchestration and module lifecycle
//   - wire.go: D2 wire protocol for subprocess communication (JSON-lines)
//   - channel.go: Channel capability declarations and session binding
//   - events.go: Event type constants and data structures
//   - salience.go: Attentional field scoring with exponential decay
//   - supervisor.go: Process supervision with health monitoring and restart
//   - text.go: Reference Module implementation (identity passthrough)
package modality
