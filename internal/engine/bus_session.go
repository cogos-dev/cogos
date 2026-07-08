// bus_session.go — per-bus session manager + hash-chained event log.
//
// Track 5 Phase 3: ported verbatim from the root package's bus_session.go so
// that the `/v1/bus/*` HTTP surface lives in engine.  The storage layout is
// identical to root:
//
//	{workspace}/.cog/.state/buses/
//	  registry.json                     — bus metadata catalogue
//	  {bus_id}/events.jsonl             — append-only hash chain (one CogBlock/line)
//
// Bus events use pkg/cogfield.Block as the wire type; the hash chain is
// per-bus (distinct from the ledger chain in ledger.go — do NOT merge).
//
// Byte-compat with root: the canonical form used for hash computation and the
// event JSON shape must stay identical.  The bridge at
// cog-sandbox-mcp/src/cog_sandbox_mcp/tools/cogos_bridge.py reads:
//
//	{v: 2, bus_id, seq, ts, from, type, payload, prev_hash?, prev?, hash}
package engine

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/myrgic/cogos/pkg/filelock"
	"github.com/myrgic/cogos/pkg/substrate/cogfield"
)

// eventsFileMaxBytes is the size threshold that triggers a size-based rotation
// of events.jsonl.  When an append causes the file to reach or exceed this
// limit the file is renamed to events.<timestamp>.jsonl and a fresh
// events.jsonl is opened.  Exposed as a var (not const) so tests can override
// it without build-tag gymnastics; reset to the default in t.Cleanup.
var eventsFileMaxBytes int64 = 64 * 1024 * 1024 // 64 MB

// BusBlock is the wire format for bus events. Alias to the canonical
// pkg/cogfield.Block so the byte-compat JSON shape is guaranteed — the
// root package uses the same type.
type BusBlock = cogfield.Block

// BusRegistryEntry matches registry.json shape — aliased for the same reason.
type BusRegistryEntry = cogfield.BusRegistryEntry

// busEventHandler is a named handler for bus events.
type busEventHandler struct {
	name    string
	handler func(busID string, block *BusBlock)
}

// BusSessionManager manages CogBus operations: bus creation, event appending,
// and reading event history. Direct verbatim port of root's busSessionManager
// to preserve byte-compat.
type BusSessionManager struct {
	mu            sync.Mutex
	workspaceRoot string
	eventHandlers []busEventHandler
	// lastSeq and lastHash cache the most-recently written seq and hash per
	// busID so that getLastEvent can return without scanning events.jsonl on
	// every AppendEvent call.  Both maps are populated on the first successful
	// AppendEvent for a bus and on every subsequent write; they are reset to
	// zero-values on rotation (seq semantics are per-file — see archiveBus in
	// root's bus_session.go which resets LastEventSeq/EventCount to 0 after
	// rename).  Protected by m.mu.
	lastSeq  map[string]int64
	lastHash map[string]string
}

// NewBusSessionManager constructs a manager rooted at workspaceRoot.
// Events and registry live under {workspaceRoot}/.cog/.state/buses/.
func NewBusSessionManager(workspaceRoot string) *BusSessionManager {
	return &BusSessionManager{
		workspaceRoot: workspaceRoot,
		lastSeq:       make(map[string]int64),
		lastHash:      make(map[string]string),
	}
}

// WorkspaceRoot returns the workspace path the manager is bound to.
func (m *BusSessionManager) WorkspaceRoot() string {
	return m.workspaceRoot
}

// AddEventHandler registers a named handler for bus events.
// Handlers are called in registration order when a bus event is appended.
func (m *BusSessionManager) AddEventHandler(name string, fn func(busID string, block *BusBlock)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.eventHandlers = append(m.eventHandlers, busEventHandler{name: name, handler: fn})
}

// RemoveEventHandler removes a named handler by name.
func (m *BusSessionManager) RemoveEventHandler(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, h := range m.eventHandlers {
		if h.name == name {
			m.eventHandlers = append(m.eventHandlers[:i], m.eventHandlers[i+1:]...)
			return
		}
	}
}

// BusesDir returns the path to the buses state directory.
func (m *BusSessionManager) BusesDir() string {
	return filepath.Join(m.workspaceRoot, ".cog", ".state", "buses")
}

// RegistryPath returns the path to the bus registry file.
func (m *BusSessionManager) RegistryPath() string {
	return filepath.Join(m.BusesDir(), "registry.json")
}

// EventsPath returns the path to a bus's events JSONL file.
func (m *BusSessionManager) EventsPath(busID string) string {
	return filepath.Join(m.BusesDir(), busID, "events.jsonl")
}

// computeBusBlockHash computes the V2 content-addressed hash for a bus block.
// Hashes the full canonical envelope (all fields except hash and sig). The
// field set and order MUST stay identical to root's computeBlockHash — the
// hash is byte-compat observable.
func computeBusBlockHash(block *BusBlock) string {
	canonical := struct {
		V       int                    `json:"v"`
		BusID   string                 `json:"bus_id,omitempty"`
		Seq     int                    `json:"seq,omitempty"`
		Ts      string                 `json:"ts"`
		From    string                 `json:"from"`
		To      string                 `json:"to,omitempty"`
		Type    string                 `json:"type"`
		Payload map[string]interface{} `json:"payload"`
		Prev    []string               `json:"prev,omitempty"`
		Merkle  string                 `json:"merkle,omitempty"`
		Size    int                    `json:"size,omitempty"`
	}{
		V: block.V, BusID: block.BusID, Seq: block.Seq,
		Ts: block.Ts, From: block.From, To: block.To,
		Type: block.Type, Payload: block.Payload,
		Prev: block.Prev, Merkle: block.Merkle, Size: block.Size,
	}
	data, _ := json.Marshal(canonical)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// EnsureBus creates the bus directory + events.jsonl if they don't exist.
// Safe to call multiple times.
func (m *BusSessionManager) EnsureBus(busID string) error {
	if !validPathComponent(busID) {
		return fmt.Errorf("invalid bus_id %q", busID)
	}
	busDir := filepath.Join(m.BusesDir(), busID)
	if err := os.MkdirAll(busDir, 0755); err != nil {
		return fmt.Errorf("create bus dir: %w", err)
	}
	eventsFile := filepath.Join(busDir, "events.jsonl")
	if _, err := os.Stat(eventsFile); os.IsNotExist(err) {
		f, err := os.Create(eventsFile)
		if err != nil {
			return fmt.Errorf("create events file: %w", err)
		}
		f.Close()
	}
	return nil
}

// RegisterBus adds or updates a bus entry in the registry.
//
// Lock ordering: registry filelock BEFORE m.mu, never the reverse — acquiring
// the cross-process lock while holding the in-process mutex would let one
// contended peer process stall every unrelated bus operation in this process.
func (m *BusSessionManager) RegisterBus(busID, sessionID, origin string) error {
	lock, err := m.acquireRegistryLock()
	if err != nil {
		return fmt.Errorf("acquire registry lock: %w", err)
	}
	defer lock.Release()

	m.mu.Lock()
	defer m.mu.Unlock()
	return m.registerBusLocked(busID, sessionID, origin)
}

// registryLockTimeout bounds how long a registry writer waits for the
// cross-process advisory lock before failing the operation (registration:
// correctness-critical, cold path).
const registryLockTimeout = 5 * time.Second

// registrySeqLockTimeout bounds the best-effort seq-metadata update on the
// append hot path — short, because a skipped update self-heals on the next
// append and a long wait would delay the AppendEvent caller.
const registrySeqLockTimeout = 500 * time.Millisecond

// acquireRegistryLock takes the cross-process advisory lock guarding the
// load-modify-save cycle on registry.json. The root-package CLI writer
// (busSessionManager, reachable from `cog bus send`/`cog infer`) takes the
// SAME lock, so a running daemon and concurrent CLI invocations serialize
// instead of last-writer-wins silently dropping each other's entries.
// Lock ordering: m.mu is always taken before this filelock.
func (m *BusSessionManager) acquireRegistryLock() (*filelock.FileLock, error) {
	if err := os.MkdirAll(m.BusesDir(), 0755); err != nil {
		return nil, err
	}
	return filelock.Acquire(m.RegistryPath()+".lock", registryLockTimeout)
}

// registerBusLocked is the locked-variant helper. Caller must hold BOTH the
// registry filelock (see acquireRegistryLock) and m.mu, acquired in that
// order.
func (m *BusSessionManager) registerBusLocked(busID, sessionID, origin string) error {
	registry := m.loadRegistry()

	for i, entry := range registry {
		if entry.BusID == busID {
			registry[i].State = "active"
			return m.saveRegistry(registry)
		}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	entry := BusRegistryEntry{
		BusID:        busID,
		State:        "active",
		Participants: []string{fmt.Sprintf("%s:session:%s", origin, sessionID), "kernel:cogos"},
		Transport:    "file",
		Endpoint:     filepath.Join(".cog", ".state", "buses", busID),
		CreatedAt:    now,
		LastEventSeq: 0,
		LastEventAt:  now,
		EventCount:   0,
	}
	registry = append(registry, entry)
	return m.saveRegistry(registry)
}

// loadRegistry reads the bus registry from disk. Returns empty slice on error.
// Caller must hold m.mu.
func (m *BusSessionManager) loadRegistry() []BusRegistryEntry {
	data, err := os.ReadFile(m.RegistryPath())
	if err != nil {
		return []BusRegistryEntry{}
	}
	var entries []BusRegistryEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return []BusRegistryEntry{}
	}
	return entries
}

// LoadRegistry is the public, lock-acquiring variant. Returns a copy of the
// current registry snapshot.
func (m *BusSessionManager) LoadRegistry() []BusRegistryEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	entries := m.loadRegistry()
	out := make([]BusRegistryEntry, len(entries))
	copy(out, entries)
	return out
}

// saveRegistry writes the bus registry to disk. Caller must hold m.mu.
//
// The write is atomic (tmp + rename): a plain truncate-before-write left
// registry.json empty if the process was killed mid-write, and loadRegistry
// swallows the resulting parse error and returns an empty registry — dropping
// all bus-to-session metadata until sessions re-register. Same pattern as
// WriteState / saveSignalField.
func (m *BusSessionManager) saveRegistry(entries []BusRegistryEntry) error {
	if err := os.MkdirAll(m.BusesDir(), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.RegistryPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.RegistryPath()); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// AppendEvent appends a new BusBlock to a bus's event chain.
// V2 blocks hash the full canonical envelope (all fields except hash and sig).
// Both prev ([]string) and prev_hash (string) are written for V1 compat.
// Handlers are dispatched synchronously after the lock is released.
//
// The bus directory + events.jsonl are created on demand if they don't yet
// exist — matches root's behaviour where handleBusSend pre-creates them but
// downstream callers (e.g. the chat pipeline) can skip that step.
func (m *BusSessionManager) AppendEvent(busID, eventType, from string, payload map[string]interface{}) (*BusBlock, error) {
	// EnsureBus is idempotent and takes its own lock-free path; do it
	// before acquiring m.mu to keep the critical section small.
	if err := m.EnsureBus(busID); err != nil {
		return nil, fmt.Errorf("ensure bus: %w", err)
	}

	m.mu.Lock()

	lastSeq, lastHash := m.getLastEvent(busID)
	newSeq := lastSeq + 1

	var prev []string
	if lastHash != "" {
		prev = []string{lastHash}
	}

	evt := BusBlock{
		V:        2,
		BusID:    busID,
		Seq:      newSeq,
		Ts:       time.Now().UTC().Format(time.RFC3339Nano),
		From:     from,
		Type:     eventType,
		Payload:  payload,
		Prev:     prev,
		PrevHash: lastHash, // V1 compat — written alongside Prev during transition
	}
	evt.Hash = computeBusBlockHash(&evt)

	line, err := json.Marshal(evt)
	if err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("marshal event: %w", err)
	}

	eventsFile := m.EventsPath(busID)
	f, err := os.OpenFile(eventsFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("open events file: %w", err)
	}
	if _, err := f.WriteString(string(line) + "\n"); err != nil {
		f.Close()
		m.mu.Unlock()
		return nil, fmt.Errorf("write event: %w", err)
	}
	f.Close()

	// Update the in-memory cache so subsequent getLastEvent calls skip the
	// file scan.  Only update after a confirmed successful write.
	m.lastSeq[busID] = int64(newSeq)
	m.lastHash[busID] = evt.Hash

	// Size-based rotation: if events.jsonl has grown past the threshold,
	// rename it to a timestamped archive and start a fresh file.
	// Cache is cleared for this busID so the next getLastEvent call starts
	// from a known-empty file (seq resets to 0; seq semantics are per-file,
	// matching root's archiveBus which resets LastEventSeq/EventCount to 0).
	if fi, statErr := os.Stat(eventsFile); statErr == nil && fi.Size() >= eventsFileMaxBytes {
		ts := time.Now().UTC().Format("2006-01-02T150405Z")
		archivePath := filepath.Join(m.BusesDir(), busID, "events."+ts+".jsonl")
		if renameErr := os.Rename(eventsFile, archivePath); renameErr == nil {
			// Create a fresh empty events.jsonl for subsequent appends. Only
			// reset the seq/hash cache once the new file actually exists — if
			// Create fails (e.g. disk full) the events.jsonl is now absent, and
			// zeroing the cache would make a concurrent ReadEvents see an empty
			// bus until the next append's EnsureBus recreates the file.
			if nf, createErr := os.Create(eventsFile); createErr == nil {
				nf.Close()
				m.lastSeq[busID] = 0
				m.lastHash[busID] = ""
			} else {
				slog.Warn("bus: size-rotation create failed, retaining seq cache", "err", createErr, "bus_id", busID)
			}
		} else {
			slog.Warn("bus: size-rotation rename failed", "err", renameErr, "bus_id", busID)
		}
	}

	// Snapshot handlers while locked, then dispatch OUTSIDE the lock.
	handlers := make([]busEventHandler, len(m.eventHandlers))
	copy(handlers, m.eventHandlers)
	m.mu.Unlock()

	// Registry seq update runs OUTSIDE m.mu: it blocks (briefly) on the
	// cross-process filelock, and must never stall unrelated bus operations.
	m.updateRegistrySeqIfNewer(busID, newSeq, evt.Ts)

	for _, h := range handlers {
		h.handler(busID, &evt)
	}

	return &evt, nil
}

// getLastEvent returns the seq and hash of the most-recently appended event.
// Caller must hold m.mu.
//
// Fast path: if the in-memory cache has an entry for busID (populated by every
// successful AppendEvent), return immediately — no file I/O.
//
// Slow path (cache miss): scan events.jsonl to find the last line and populate
// the cache.  This only happens on the very first AppendEvent after process
// start, or after a size-based rotation clears the cache entry.
func (m *BusSessionManager) getLastEvent(busID string) (int, string) {
	// Cache hit — return without touching the filesystem.
	if seq, ok := m.lastSeq[busID]; ok {
		return int(seq), m.lastHash[busID]
	}

	// Cache miss — scan the file and prime the cache.
	eventsFile := m.EventsPath(busID)
	f, err := os.Open(eventsFile)
	if err != nil {
		return 0, ""
	}
	defer f.Close()

	var lastLine string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			lastLine = line
		}
	}

	if lastLine == "" {
		// Prime the cache so subsequent calls skip the file open entirely.
		m.lastSeq[busID] = 0
		m.lastHash[busID] = ""
		return 0, ""
	}

	var block BusBlock
	if err := json.Unmarshal([]byte(lastLine), &block); err != nil {
		return 0, ""
	}
	m.lastSeq[busID] = int64(block.Seq)
	m.lastHash[busID] = block.Hash
	return block.Seq, block.Hash
}

// LatestEventHash returns the seq and content-addressed hash of the latest
// event written to busID, without appending. Returns ("", 0) if no event has
// been written yet. Acquires the bus mutex for a moment, then releases it.
func (m *BusSessionManager) LatestEventHash(busID string) (hash string, seq int64, err error) {
	m.mu.Lock()
	s, h := m.getLastEvent(busID)
	m.mu.Unlock()
	return h, int64(s), nil
}

// updateRegistrySeqIfNewer updates the last event seq/timestamp in the
// registry. Caller must NOT hold m.mu — this method blocks (briefly) on the
// cross-process registry filelock, and holding the in-process mutex across
// that wait would let one contended peer stall every unrelated bus operation
// in this process (the exact hazard this PR fixes elsewhere).
//
// Best-effort + monotonic: the seq update is derivable metadata that
// self-heals on the next append, so on lock contention we skip rather than
// wait long (registrySeqLockTimeout), and because callers run outside m.mu
// their updates can arrive out of order — the IfNewer guard makes a stale
// update a harmless no-op instead of a seq regression.
func (m *BusSessionManager) updateRegistrySeqIfNewer(busID string, seq int, ts string) {
	if err := os.MkdirAll(m.BusesDir(), 0755); err != nil {
		slog.Warn("bus: skipping registry seq update", "err", err, "bus_id", busID)
		return
	}
	lock, err := filelock.Acquire(m.RegistryPath()+".lock", registrySeqLockTimeout)
	if err != nil {
		slog.Warn("bus: skipping registry seq update (lock contended)", "err", err, "bus_id", busID)
		return
	}
	defer lock.Release()

	registry := m.loadRegistry()
	for i, entry := range registry {
		if entry.BusID == busID {
			if entry.LastEventSeq >= seq {
				return // stale out-of-order update; newer one already landed
			}
			registry[i].LastEventSeq = seq
			registry[i].LastEventAt = ts
			registry[i].EventCount = seq
			break
		}
	}
	if err := m.saveRegistry(registry); err != nil {
		slog.Warn("bus: failed to update registry seq", "err", err, "bus_id", busID)
	}
}

// ReadEvents reads all events from a bus. De-dups by seq (file may have
// duplicates from crash recovery).
func (m *BusSessionManager) ReadEvents(busID string) ([]BusBlock, error) {
	if !validPathComponent(busID) {
		return nil, fmt.Errorf("invalid bus_id %q", busID)
	}
	eventsFile := m.EventsPath(busID)
	f, err := os.Open(eventsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open events file: %w", err)
	}
	defer f.Close()

	var events []BusBlock
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	seen := make(map[int]bool)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		var block BusBlock
		if err := json.Unmarshal([]byte(line), &block); err != nil {
			continue
		}
		if seen[block.Seq] {
			continue
		}
		seen[block.Seq] = true
		events = append(events, block)
	}

	return events, nil
}
