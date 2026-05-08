// trace_emit.go — Bus-publish helper for cycle-trace events (ADR-083).
//
// This file is the bridge between the schema-only `trace` subpackage and the
// kernel's bus API (`busSessionManager.appendBusEvent`). Emission is
// best-effort and non-blocking: errors are logged, never propagated. Losing
// a trace event is always preferable to stalling the metabolic cycle.
//
// Wiring:
//   - At serve startup, call `InstallTraceEmitter(busManager, root)` once.
//     That sets:
//       * trace bus ensured on disk (`bus_cycle_trace`)
//       * `engine.TraceEmitter` hook wired to `emitCycleEvent`
//       * `engine.TraceIdentity` wired to the active identity name
//     After that, engine/process.go and the agent harness can call
//     `emitCycleEvent` (or the engine hook) without further setup.
//
// ADR-084 dataref migration policy (Phase 1: schema-additive) — this file
// is the G3 pilot consumer of the by-reference emit path. Two paths coexist
// inside emitCycleEvent:
//
//   - Inline (legacy / default): JSON-decoded payload map is stamped onto
//     CogBlock.Payload via appendBusEvent.
//   - By-reference (Phase 2 pilot): when COGOS_DATAREF_EMIT contains the
//     "cycle_trace" token AND a BlobStore is wired, the raw bytes are stored
//     content-addressed and the envelope carries only Digest + MediaType +
//     Size via appendBusEventRef; Payload is nil. BlobStore errors fall
//     through to the inline path so events are never dropped.
//
// Consumers of bus_cycle_trace MUST tolerate both shapes during Phase 1 and
// resolve byref-first (Digest -> GET /v1/blobs/:digest, else inline Payload).
// The media type is the single `traceMediaType` constant below, so decoders
// can switch on it regardless of envelope shape.
//
// See: .cog/adr/084-bus-payloads-as-cogblocks.cog.md
package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/myrgic/cogos/internal/engine"
	"github.com/myrgic/cogos/trace"
)

// traceBusID is the dedicated bus that carries cycle-trace events (B5 /
// ADR-083). External consumers (e.g. Mod³ dashboard) subscribe via the
// existing /v1/events/stream SSE endpoint filtered on this bus.
const traceBusID = "bus_cycle_trace"

// traceMediaType is the OCI media type advertised on dataref emissions for
// cycle-trace events (ADR-084). Consumers resolve the digest via
// GET /v1/blobs/:digest and decode the bytes as trace.CycleEvent JSON.
const traceMediaType = "application/vnd.cogos.trace.cycle.v1+json"

// traceDatarefEnvVar is the comma-separated allowlist of emit sites that
// should write payloads by reference (to BlobStore) instead of inline. The
// G3 pilot recognises the token "cycle_trace"; other tokens are ignored.
// When unset, emitCycleEvent falls back to the legacy inline-payload path so
// existing consumers continue to work unchanged.
const traceDatarefEnvVar = "COGOS_DATAREF_EMIT"

// traceBusMgr holds the bus manager used by emitCycleEvent. Set once by
// InstallTraceEmitter. Accessed without a lock — a single writer at startup.
var traceBusMgr atomic.Pointer[busSessionManager]

// traceBlobStore holds the shared content-addressed blob store used by the
// dataref emit path (ADR-084 Phase 2). Set by InstallTraceEmitter when
// provided; otherwise emitCycleEvent skips the dataref path and emits inline.
var traceBlobStore atomic.Pointer[engine.BlobStore]

// traceIdentityName is the cached active-identity name used as the `source`
// field on emitted events. Falls back to "cog" until wired.
var traceIdentityName atomic.Value // string

func init() {
	traceIdentityName.Store("cog")
}

// datarefEmitEnabled reports whether the given emit-site token is present in
// the COGOS_DATAREF_EMIT env var. The lookup is intentionally cheap (per
// emit) so the flag can be toggled without restart in smoke tests.
func datarefEmitEnabled(site string) bool {
	raw := os.Getenv(traceDatarefEnvVar)
	if raw == "" {
		return false
	}
	for _, tok := range strings.Split(raw, ",") {
		if strings.TrimSpace(tok) == site {
			return true
		}
	}
	return false
}

// InstallTraceEmitter wires the engine's TraceEmitter / TraceIdentity hooks
// to the kernel bus and the active identity. Safe to call once at daemon
// start; subsequent calls overwrite the bus manager.
//
// root is the workspace root (used to resolve the identity name).
//
// bs is the shared BlobStore used by the ADR-084 dataref emit path. When nil,
// emitCycleEvent always emits inline regardless of the COGOS_DATAREF_EMIT env
// var — the dataref path degrades gracefully if blob storage is not wired.
func InstallTraceEmitter(mgr *busSessionManager, root string, bs *engine.BlobStore) {
	if mgr == nil {
		return
	}
	traceBusMgr.Store(mgr)
	if bs != nil {
		traceBlobStore.Store(bs)
	}
	ensureTraceBus(mgr)

	if name, err := GetIdentityName(root); err == nil && name != "" {
		traceIdentityName.Store(name)
	}
	// TODO: wire to active identity (re-fetch if identity switches at runtime).

	engine.TraceEmitter = emitCycleEvent
	engine.TraceIdentity = func() string {
		if v := traceIdentityName.Load(); v != nil {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
		return "cog"
	}
	log.Printf("[trace] cycle-trace emitter installed (bus=%s identity=%s)",
		traceBusID, engine.TraceIdentity())
}

// ensureTraceBus creates the trace bus directory, events file, and registry
// entry if they don't already exist.
func ensureTraceBus(mgr *busSessionManager) {
	busDir := filepath.Join(mgr.busesDir(), traceBusID)
	if err := os.MkdirAll(busDir, 0o755); err != nil {
		log.Printf("[trace] create bus dir: %v", err)
		return
	}
	eventsFile := filepath.Join(busDir, "events.jsonl")
	if _, err := os.Stat(eventsFile); os.IsNotExist(err) {
		if f, err := os.Create(eventsFile); err == nil {
			f.Close()
		}
	}
	if err := mgr.registerBus(traceBusID, "kernel:trace", "kernel:trace"); err != nil {
		log.Printf("[trace] register bus: %v", err)
	}
}

// emitCycleEvent publishes a CycleEvent onto the trace bus. Best-effort:
// marshal or append errors are logged, never returned. The publish runs on a
// goroutine so the caller (state machine, harness loop) is never blocked by
// bus I/O or handler dispatch.
//
// ADR-084 Phase 2 (G3 pilot): when COGOS_DATAREF_EMIT contains "cycle_trace"
// and a BlobStore is wired, the payload bytes are written to the blob store
// and the envelope carries only a digest reference (media type
// `application/vnd.cogos.trace.cycle.v1+json`). Otherwise the legacy inline
// payload path is used. Consumers MUST tolerate both shapes during Phase 1.
func emitCycleEvent(ev trace.CycleEvent) {
	mgr := traceBusMgr.Load()
	if mgr == nil {
		return
	}
	raw, err := json.Marshal(ev)
	if err != nil {
		log.Printf("[trace] marshal: %v", err)
		return
	}
	eventType := "cycle." + string(ev.Kind)
	source := engine.TraceIdentity()

	// ADR-084 dataref path: digest-by-reference via BlobStore. Only taken
	// when both the emit-site flag is set and a BlobStore is wired, so the
	// absence of either cleanly falls back to the legacy inline path.
	if bs := traceBlobStore.Load(); bs != nil && datarefEmitEnabled("cycle_trace") {
		hexHash, err := bs.Store(raw, traceMediaType)
		if err != nil {
			log.Printf("[trace] blob store: %v", err)
			// Fall through to inline emit so the event is not lost.
		} else {
			digest := "sha256:" + hexHash
			size := len(raw)
			go func() {
				if _, err := mgr.appendBusEventRef(traceBusID, eventType, source, digest, traceMediaType, size); err != nil {
					log.Printf("[trace] append (dataref): %v", err)
				}
			}()
			return
		}
	}

	// Legacy inline path: decode the raw bytes into a map and stamp them on
	// the envelope's Payload field. Consumers that pre-date ADR-084 see this
	// shape unchanged.
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		log.Printf("[trace] unmarshal: %v", err)
		return
	}
	go func() {
		if _, err := mgr.appendBusEvent(traceBusID, eventType, source, payload); err != nil {
			log.Printf("[trace] append: %v", err)
		}
	}()
}

// --- cycleID context plumbing for the agent harness ---------------------------
//
// The harness's Assess/Execute signatures are public; threading a cycleID
// parameter through them would be a breaking change. Instead we stash the ID
// on the context so tool-dispatch sites and per-turn emission can pull it
// out without plumbing. A missing value yields "" and emission falls back to
// a one-shot ID. See agent_harness.go / agent_serve.go for the real helpers.

type cycleIDKey struct{}
