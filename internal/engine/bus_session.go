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
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/myrgic/cogos/pkg/filelock"
	"github.com/myrgic/cogos/pkg/pathsafe"
	"github.com/myrgic/cogos/pkg/substrate/cogfield"
)

// eventsFileMaxBytes is the size threshold that triggers a size-based rotation
// of events.jsonl.  When an append causes the file to reach or exceed this
// limit the file is renamed to events.<timestamp>.jsonl and a fresh
// events.jsonl is opened.  Exposed as a var (not const) so tests can override
// it without build-tag gymnastics; reset to the default in t.Cleanup.
var eventsFileMaxBytes int64 = 64 * 1024 * 1024 // 64 MB

// busArchiveRetention declares, per bus, how many rotated events.<ts>.jsonl
// archives to keep.  Rotation without retention grows without bound: on one
// long-running node the `traces` bus reached 11.2 GB across 173 archives with
// nothing ever reclaiming them, and the accumulated bus directory was the
// largest writer on the machine.
//
// Retention is deliberately declared PER BUS rather than as a global default,
// because different buses stand in different relationships to the substrate.
// Trace and telemetry buses are disposable instrumentation — the current window
// is the only part anyone reads.  Chat and MCP buses carry conversation
// content, which is ground truth and must never be reclaimed on a timer.  A
// global default would have to pick one of those and be wrong for the other.
//
// A bus with no entry here keeps every archive forever, which is the historical
// behaviour: this map is opt-in, so adding retention can never silently delete
// history for a bus nobody considered.
//
// Exposed as a var (not const) so tests can override it without build-tag
// gymnastics; reset to the default in t.Cleanup.
var busArchiveRetention = map[string]int{
	"traces":         8,
	"kernel_proprio": 8,
	"peer_awareness": 8,
}

// busArchiveKeepAll is the sentinel returned for buses with no declared
// retention: keep every archive.
const busArchiveKeepAll = -1

// archiveRetentionFor reports how many rotated archives to keep for busID, or
// busArchiveKeepAll when the bus has declared no retention.
//
// Bus IDs reaching this function are prefixed by producer ("bus_traces",
// "bus_chat_<uuid>"), so the lookup matches on the prefix-stripped name and
// then on the leading segment, letting one entry cover a whole family
// ("chat" would cover every per-conversation chat bus) without enumerating
// instance IDs.
func archiveRetentionFor(busID string) int {
	name := strings.TrimPrefix(busID, "bus_")
	if n, ok := busArchiveRetention[name]; ok {
		return n
	}
	if idx := strings.IndexByte(name, '_'); idx > 0 {
		if n, ok := busArchiveRetention[name[:idx]]; ok {
			return n
		}
	}
	return busArchiveKeepAll
}

// pruneBusArchives removes the oldest rotated archives for busID, keeping the
// newest keep of them.  Archives are named events.<RFC3339-ish-UTC>.jsonl, so
// lexical order over the filename is chronological order and no stat calls are
// needed to sort.  The live events.jsonl is never a candidate.
//
// Callers must NOT hold m.mu: this does directory I/O and unlinks, and holding
// the bus lock across it would stall every unrelated bus operation.  Errors are
// logged and swallowed — failing to reclaim disk must never fail an append that
// has already been durably written.
func (m *BusSessionManager) pruneBusArchives(busID string, keep int) {
	if keep < 0 {
		return
	}
	busDir := filepath.Join(m.BusesDir(), pathsafe.SanitizeComponent(busID))
	entries, err := os.ReadDir(busDir)
	if err != nil {
		slog.Warn("bus: archive prune could not read bus dir", "err", err, "bus_id", busID)
		return
	}
	var archives []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || n == "events.jsonl" {
			continue
		}
		if strings.HasPrefix(n, "events.") && strings.HasSuffix(n, ".jsonl") {
			archives = append(archives, n)
		}
	}
	if len(archives) <= keep {
		return
	}
	sort.Strings(archives) // lexical == chronological for the timestamp format
	for _, n := range archives[:len(archives)-keep] {
		if err := os.Remove(filepath.Join(busDir, n)); err != nil {
			slog.Warn("bus: archive prune failed", "err", err, "bus_id", busID, "file", n)
			continue
		}
		slog.Info("bus: pruned rotated archive", "bus_id", busID, "file", n, "keep", keep)
	}
}

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

	// registryFileMu single-flights this PROCESS's attempts at the
	// cross-process registry filelock (RegisterBus's correctness path and
	// updateRegistrySeqIfNewer's best-effort path). Without it, N concurrent
	// AppendEvent/RegisterBus callers in the same process each independently
	// call filelock.Acquire — a real flock(2) syscall, retried on a 50ms
	// poll — and Go's runtime pins one OS thread per goroutine currently
	// inside that syscall. Under request-storm concurrency (#505: every HTTP
	// handler span-emission re-registers and re-appends to bus_traces, see
	// handler_span.go) that turned into hundreds of concurrently live retry
	// loops and a monotonically growing thread count — Go never returns
	// created OS threads to the kernel, so the count only ratchets upward.
	// Serializing in-process access through this mutex means at most one
	// goroutine per process is ever inside the real flock(2) retry loop at a
	// time; every other contender blocks on this mutex instead, which parks
	// on the Go scheduler and costs no OS thread. Cross-process behavior
	// (a daemon and a `cogos mcp serve` subprocess sharing a workspace) is
	// unaffected — the underlying filelock.Acquire calls and their timeouts
	// are unchanged, just no longer raced against each other from within a
	// single process.
	registryFileMu sync.Mutex

	// registeredActive caches busIDs this process has already registered as
	// "active" in registry.json. RegisterBus is idempotent by contract, but
	// before this cache every call — regardless of whether state actually
	// changed — paid the full cross-process lock + load + save cycle.
	// Callers like handler_span.go call RegisterBus on every single HTTP
	// request; with the cache, only the first call per busID per process
	// pays that cost, and every repeat is a m.mu-guarded map lookup. No code
	// path in this package ever demotes a bus out of "active", so the cache
	// cannot go stale for the lifetime of this manager. Guarded by m.mu.
	registeredActive map[string]bool

	// registrySeqSkipCount counts every best-effort seq update skipped —
	// either because another goroutine already holds registryFileMu, or
	// because the cross-process filelock itself timed out. Logged at a rate
	// limit (registrySeqSkipLogEvery) instead of once per skip, so the skip
	// path itself cannot become a log-volume amplifier under sustained
	// contention (the original #505 symptom: a continuous storm of
	// "skipping registry seq update" lines).
	registrySeqSkipCount atomic.Int64
}

// NewBusSessionManager constructs a manager rooted at workspaceRoot.
// Events and registry live under {workspaceRoot}/.cog/.state/buses/.
func NewBusSessionManager(workspaceRoot string) *BusSessionManager {
	return &BusSessionManager{
		workspaceRoot:    workspaceRoot,
		lastSeq:          make(map[string]int64),
		lastHash:         make(map[string]string),
		registeredActive: make(map[string]bool),
	}
}

// RegistrySeqSkipCount reports how many best-effort registry seq updates have
// been skipped (in-process or cross-process contention) since this manager
// was constructed. Exposed for tests and diagnostics — see #505.
func (m *BusSessionManager) RegistrySeqSkipCount() int64 {
	return m.registrySeqSkipCount.Load()
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

// EventsPath returns the path to a bus's events JSONL file. busID is
// sanitized via pathsafe.SanitizeComponent before joining: bus_id is a
// caller-supplied free-form string (POST /v1/bus/open, POST /v1/bus/send)
// that is structurally identical to a session key — the same whole-class
// defect this file's package fixes for session keys (myrgic/cogos#489)
// applies here, since validPathComponent alone permits colons and other
// NTFS-illegal characters through. All internal readers/writers of the
// events file route through this method, so sanitizing here is the single
// seam that makes EnsureBus, AppendEvent, getLastEvent, and ReadEvents safe.
func (m *BusSessionManager) EventsPath(busID string) string {
	return filepath.Join(m.BusesDir(), pathsafe.SanitizeComponent(busID), "events.jsonl")
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
//
// validPathComponent rejects the degenerate cases (empty, ".", "..",
// embedded separators/NUL) with a clear "invalid bus_id" error up front —
// those are inputs with no sane on-disk representation, not ones we want to
// silently escape. Everything validPathComponent accepts still needs
// NTFS-illegal-character escaping (colons, etc.), which EventsPath now
// applies via pathsafe.SanitizeComponent — see that method's doc comment.
func (m *BusSessionManager) EnsureBus(busID string) error {
	if !validPathComponent(busID) {
		return fmt.Errorf("invalid bus_id %q", busID)
	}
	eventsFile := m.EventsPath(busID)
	busDir := filepath.Dir(eventsFile)
	if err := os.MkdirAll(busDir, 0755); err != nil {
		return fmt.Errorf("create bus dir: %w", err)
	}
	if _, err := os.Stat(eventsFile); os.IsNotExist(err) {
		f, err := os.Create(eventsFile)
		if err != nil {
			return fmt.Errorf("create events file: %w", err)
		}
		f.Close()
	}
	return nil
}

// RegisterBus adds or updates a bus entry in the registry. Idempotent: a
// repeat call for a busID already known-active IN THIS PROCESS is a cache
// hit (see registeredActive) and never touches the cross-process lock at
// all — the fast path for callers like handler_span.go that call this on
// every request.
//
// Lock ordering: registryFileMu (in-process single-flight) BEFORE the
// registry filelock (cross-process) BEFORE m.mu, never the reverse —
// acquiring an outer lock while holding an inner one would let one
// contended peer process/goroutine stall every unrelated bus operation in
// this process.
func (m *BusSessionManager) RegisterBus(busID, sessionID, origin string) error {
	m.mu.Lock()
	alreadyActive := m.registeredActive[busID]
	m.mu.Unlock()
	if alreadyActive {
		return nil
	}

	// Single-flight this process's attempts at the cross-process lock (see
	// registryFileMu doc comment). This path is correctness-critical, so it
	// blocks for its turn rather than skipping.
	m.registryFileMu.Lock()
	defer m.registryFileMu.Unlock()

	lock, err := m.acquireRegistryLock()
	if err != nil {
		return fmt.Errorf("acquire registry lock: %w", err)
	}
	defer lock.Release()

	m.mu.Lock()
	err = m.registerBusLocked(busID, sessionID, origin)
	if err == nil {
		m.registeredActive[busID] = true
	}
	m.mu.Unlock()
	return err
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
// load-modify-save cycle on registry.json. Any other process pointed at the
// same workspace root — notably a `cogos mcp serve` subprocess, which
// constructs its own independent BusSessionManager (see cli_mcp.go) — takes
// the SAME lock, so a running daemon and concurrent CLI/MCP invocations
// serialize instead of last-writer-wins silently dropping each other's
// entries. Lock ordering: registryFileMu, then m.mu, are always taken
// outside this filelock by this manager's own callers; a peer process has
// no equivalent to registryFileMu and simply contends at the OS level.
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
		// Sanitized to match the actual on-disk directory name (see
		// EventsPath) — an unsanitized Endpoint here would record metadata
		// that doesn't match where EnsureBus actually wrote the bus.
		Endpoint:     filepath.Join(".cog", ".state", "buses", pathsafe.SanitizeComponent(busID)),
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
	rotated := false
	if fi, statErr := os.Stat(eventsFile); statErr == nil && fi.Size() >= eventsFileMaxBytes {
		ts := time.Now().UTC().Format("2006-01-02T150405Z")
		archivePath := filepath.Join(m.BusesDir(), pathsafe.SanitizeComponent(busID), "events."+ts+".jsonl")
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
				rotated = true
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

	// Archive retention runs OUTSIDE m.mu: it lists a directory and unlinks
	// files, and holding the bus lock across that would stall every unrelated
	// bus operation.  Only runs on the append that actually rotated, so the
	// cost is amortised across a whole events file.
	if rotated {
		m.pruneBusArchives(busID, archiveRetentionFor(busID))
	}

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

// registrySeqSkipLogEvery rate-limits the skip warning below so sustained
// contention logs a sample instead of one line per AppendEvent — the
// original #505 symptom was a continuous storm of these exact log lines.
const registrySeqSkipLogEvery = 100

// updateRegistrySeqIfNewer updates the last event seq/timestamp in the
// registry. Caller must NOT hold m.mu — this method may briefly touch the
// cross-process registry filelock, and holding the in-process mutex across
// that wait would let one contended peer stall every unrelated bus operation
// in this process (the exact hazard this PR fixes elsewhere).
//
// Best-effort + monotonic: the seq update is derivable metadata that
// self-heals on the next append, so on ANY contention — in-process
// (registryFileMu already held by another goroutine, see its doc comment)
// or cross-process (the filelock itself times out) — this skips immediately
// rather than joining a retry loop. Because callers run without m.mu their
// updates can arrive out of order — the IfNewer guard makes a stale update a
// harmless no-op instead of a seq regression.
//
// registryFileMu uses TryLock (not Lock) here deliberately: this is the
// AppendEvent hot path, potentially called at high frequency (every HTTP
// handler span, every bus message). Blocking for a turn would still be
// correct, but skipping outright when another goroutine is already inside
// the cross-process lock attempt keeps this path from ever queuing, which is
// what "best-effort" means for metadata that self-heals on the next append.
func (m *BusSessionManager) updateRegistrySeqIfNewer(busID string, seq int, ts string) {
	if !m.registryFileMu.TryLock() {
		m.recordRegistrySeqSkip(busID, "in-process contention")
		return
	}
	defer m.registryFileMu.Unlock()

	if err := os.MkdirAll(m.BusesDir(), 0755); err != nil {
		m.recordRegistrySeqSkip(busID, "mkdir: "+err.Error())
		return
	}
	lock, err := filelock.Acquire(m.RegistryPath()+".lock", registrySeqLockTimeout)
	if err != nil {
		m.recordRegistrySeqSkip(busID, "cross-process lock contended")
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

// recordRegistrySeqSkip increments the skip counter and logs at a rate
// limit — see registrySeqSkipLogEvery.
func (m *BusSessionManager) recordRegistrySeqSkip(busID, reason string) {
	n := m.registrySeqSkipCount.Add(1)
	if n == 1 || n%registrySeqSkipLogEvery == 0 {
		slog.Warn("bus: skipping registry seq update (lock contended)",
			"bus_id", busID, "reason", reason, "skip_count", n)
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
