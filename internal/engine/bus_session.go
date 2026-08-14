// bus_session.go — per-bus session manager + hash-chained event log.
//
// Track 5 Phase 3: this is where the `/v1/bus/*` HTTP surface lives in
// engine. Storage layout:
//
//	{workspace}/.cog/.state/buses/
//	  registry.json                     — bus metadata catalogue
//	  {bus_id}/events.jsonl             — append-only hash chain (one CogBlock/line)
//
// Bus events use pkg/cogfield.Block as the wire type; the hash chain is
// per-bus (distinct from the ledger chain in ledger.go — do NOT merge).
//
// Byte-compat matters here: the canonical form used for hash computation and
// the event JSON shape must stay identical across releases, since the bridge
// at cog-sandbox-mcp/src/cog_sandbox_mcp/tools/cogos_bridge.py reads:
//
//	{v: 2, bus_id, seq, ts, from, type, payload, prev_hash?, prev?, hash}
package engine

import (
	"bufio"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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

// maxReadCacheBuses bounds how many buses' busReadCache entries
// ReadEventsSince keeps resident at once. Without a cap, readCache grows
// with the number of distinct buses ever read — one fully-parsed event
// history plus a seen map[int]bool each — and never shrinks; on a node with
// hundreds of buses that is an unbounded-memory trade for the unbounded-CPU
// bug this cache exists to fix (#561). Least-recently-*touched* bus is
// evicted once the count would exceed this. Exposed as a var so tests can
// shrink it without needing hundreds of buses to exercise eviction.
var maxReadCacheBuses = 256

// maxReadCacheEventsPerBus bounds how many parsed events a single bus's
// busReadCache entry may retain. A bus is normally bounded by
// eventsFileMaxBytes (rotation clears its cache), but a very high volume of
// small events can still accumulate a large in-memory parsed slice before
// rotation triggers on byte size. Once a bus's cache would exceed this,
// the entry is dropped after answering the current call — see
// ReadEventsSince. Exposed as a var for the same reason as
// maxReadCacheBuses.
var maxReadCacheEventsPerBus = 50_000

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
// pkg/cogfield.Block so the byte-compat JSON shape is guaranteed.
type BusBlock = cogfield.Block

// BusRegistryEntry matches registry.json shape — aliased for the same reason.
type BusRegistryEntry = cogfield.BusRegistryEntry

// busEventHandler is a named handler for bus events.
type busEventHandler struct {
	name    string
	handler func(busID string, block *BusBlock)
}

// BusSessionManager manages CogBus operations: bus creation, event appending,
// and reading event history.
type BusSessionManager struct {
	mu            sync.Mutex
	workspaceRoot string
	eventHandlers []busEventHandler
	// lastSeq and lastHash cache the most-recently written seq and hash per
	// busID so that getLastEvent can return without scanning events.jsonl on
	// every AppendEvent call.  Both maps are populated on the first successful
	// AppendEvent for a bus and on every subsequent write; they are reset to
	// zero-values on rotation (seq semantics are per-file — AppendEvent's
	// rotation branch below resets LastEventSeq/EventCount to 0 after the
	// rename).  AppendEvent's rotation path mirrors this reset into the
	// registry via resetRegistrySeq, so the in-memory cache and the on-disk
	// registry entry reset in lockstep — see resetRegistrySeq's doc comment
	// for why a plain updateRegistrySeqIfNewer call is not sufficient here.
	// Protected by m.mu.
	lastSeq  map[string]int64
	lastHash map[string]string

	// generation counts size-based rotations of busID's events.jsonl, and
	// fences registry seq writes against reordering across a rotation
	// boundary — see currentGenerationLocked and the doc comments on
	// updateRegistrySeqIfNewer / resetRegistrySeq for the race this closes
	// (a non-rotating append's registry advance landing AFTER a later
	// rotating append's registry reset, reinstating the pre-rotation seq and
	// silently freezing the registry again).
	//
	// Primed from the persisted registry entry's Generation field on first
	// touch per busID this process (see currentGenerationLocked) so a
	// process restart doesn't reset the counter to 0 while the persisted
	// value is already ahead — that would make every subsequent write's
	// generation permanently mismatch the persisted one and never apply.
	// Incremented only on a successful rotation, under m.mu, so the
	// generation captured by any two AppendEvent calls reflects their true
	// m.mu-serialized order — the ordering question this fixes is decided
	// here, at the one point in AppendEvent where ordering is already
	// serialized, not later where the two write paths run concurrently and
	// unordered. Protected by m.mu.
	generation map[string]int64

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

	// readCacheMetaMu guards readCache, readCacheLocks, readCacheLRU, and
	// readCacheLRUIdx (the maps/list themselves, not the *busReadCache
	// contents) — see ReadEventsSince. Held only for map/list bookkeeping,
	// never across file I/O.
	readCacheMetaMu sync.Mutex
	// readCache holds, per bus, the incrementally-extended parse state
	// ReadEventsSince uses to avoid re-scanning a bus's entire history on
	// every call. See busReadCache and ReadEventsSince's doc comment (#561).
	// Bounded to maxReadCacheBuses entries via readCacheLRU (LRU eviction)
	// — see ReadEventsSince.
	readCache map[string]*busReadCache
	// readCacheLocks holds one *sync.Mutex per bus, serializing concurrent
	// ReadEventsSince/ReadEvents calls against the SAME bus's cache (so two
	// pollers hitting the same busy bus at once don't double-scan the same
	// delta) without serializing reads across DIFFERENT buses. Entries are
	// evicted in lockstep with readCache — see ReadEventsSince.
	readCacheLocks map[string]*sync.Mutex
	// readCacheLRU tracks bus-cache recency, most-recently-touched at the
	// front, for the eviction maxReadCacheBuses enforces. readCacheLRUIdx is
	// the busID -> element index into it. Both are readCache/readCacheLocks
	// bookkeeping, guarded by readCacheMetaMu exactly like those maps.
	readCacheLRU    *list.List
	readCacheLRUIdx map[string]*list.Element
}

// busReadCache is one bus's incremental ReadEventsSince state: everything
// parsed from events.jsonl so far, plus the byte offset that content ends
// at, so the next call can Seek there and parse only what's new instead of
// re-reading the file from byte 0.
//
// events is de-duped by Seq and kept in the order encountered (== Seq
// order for a well-formed file, since AppendEvent hands out strictly
// increasing per-file Seq values) — the exact contract ReadEvents has
// always had, just built up across calls instead of rebuilt on every one.
type busReadCache struct {
	offset int64 // bytes of events.jsonl already folded into events; always a line boundary
	events []BusBlock
	seen   map[int]bool // seq -> true, same dedup semantics ReadEvents documents

	// fi is the os.FileInfo captured from the last events.jsonl this cache
	// was built against — the file-identity check ReadEventsSince uses to
	// detect rotation (see os.SameFile there). nil means "no known prior
	// identity" (a fresh cache, or the file didn't exist last time), which
	// os.SameFile safely treats as "not the same file" without panicking.
	fi os.FileInfo
}

// NewBusSessionManager constructs a manager rooted at workspaceRoot.
// Events and registry live under {workspaceRoot}/.cog/.state/buses/.
func NewBusSessionManager(workspaceRoot string) *BusSessionManager {
	return &BusSessionManager{
		workspaceRoot:    workspaceRoot,
		lastSeq:          make(map[string]int64),
		lastHash:         make(map[string]string),
		generation:       make(map[string]int64),
		registeredActive: make(map[string]bool),
		readCache:        make(map[string]*busReadCache),
		readCacheLocks:   make(map[string]*sync.Mutex),
		readCacheLRU:     list.New(),
		readCacheLRUIdx:  make(map[string]*list.Element),
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
// field set and order MUST stay identical release-to-release — the hash is
// byte-compat observable by cog-sandbox-mcp's bridge (see the file header).
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
// exist, so downstream callers (e.g. the chat pipeline) don't have to
// pre-create them before their first append.
func (m *BusSessionManager) AppendEvent(busID, eventType, from string, payload map[string]interface{}) (*BusBlock, error) {
	// EnsureBus is idempotent and takes its own lock-free path; do it
	// before acquiring m.mu to keep the critical section small.
	if err := m.EnsureBus(busID); err != nil {
		return nil, fmt.Errorf("ensure bus: %w", err)
	}

	m.mu.Lock()

	lastSeq, lastHash := m.getLastEvent(busID)
	newSeq := lastSeq + 1

	// Capture the generation this append belongs to HERE, under m.mu, at the
	// same point newSeq is decided — see the generation field's doc comment.
	// writeGen is what actually gets passed to the registry write below;
	// it only changes (to gen+1) if THIS append is the one that rotates.
	gen := m.currentGenerationLocked(busID)
	writeGen := gen

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
	// from a known-empty file — seq semantics are per-file, so LastEventSeq
	// and EventCount reset to 0 for the fresh file (mirrored into the
	// registry below via resetRegistrySeq).
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
				m.generation[busID] = gen + 1
				writeGen = gen + 1
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

	// Test-only synchronization seam — always nil in production. See its
	// doc comment for what it's for.
	if appendEventPostUnlockHook != nil {
		appendEventPostUnlockHook(busID, rotated, writeGen)
	}

	// Archive retention runs OUTSIDE m.mu: it lists a directory and unlinks
	// files, and holding the bus lock across that would stall every unrelated
	// bus operation.  Only runs on the append that actually rotated, so the
	// cost is amortised across a whole events file.
	//
	// Both registry writes below run outside m.mu for the same reason
	// (documented in detail on updateRegistrySeqIfNewer): putting the
	// load-modify-save-plus-cross-process-flock cycle under the per-bus hot
	// mutex would serialize every unrelated bus operation in this process
	// behind disk I/O and flock(2) retries — exactly the #505 thread-storm
	// this package already had to fix once. Running outside m.mu instead
	// means these two writes can race each other, which is what
	// writeGen/gen (captured above, under m.mu, where their relative order
	// IS decided) exist to fence.
	if rotated {
		m.pruneBusArchives(busID, archiveRetentionFor(busID))

		// Reset the registry entry in lockstep with the in-memory cache
		// reset above (m.lastSeq[busID] = 0). The event just appended
		// (newSeq) now lives in the archived file, not the fresh one, so
		// the registry should reflect the fresh file's true state — zero
		// events — rather than record the archived file's terminal seq via
		// the normal advance-only path. See resetRegistrySeq's doc comment
		// for why this must be an unconditional-on-seq (but generation-
		// fenced) reset, not updateRegistrySeqIfNewer(busID, newSeq, ...):
		// the latter would write newSeq (the old file's terminal value)
		// into the registry, after which every following append's small
		// per-file seq (1, 2, 3, …) would trip the monotonic guard and be
		// dropped forever — exactly the staleness bug this method fixes.
		m.resetRegistrySeq(busID, evt.Ts, writeGen)
	} else {
		// Registry seq update runs OUTSIDE m.mu: it blocks (briefly) on the
		// cross-process filelock, and must never stall unrelated bus operations.
		m.updateRegistrySeqIfNewer(busID, newSeq, writeGen, evt.Ts)
	}

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

// currentGenerationLocked returns busID's in-memory rotation generation,
// priming it from the persisted registry entry the first time this process
// touches busID (same one-time-cost-then-cached shape as getLastEvent's
// cache-miss file scan above — including doing that one-time file I/O while
// holding m.mu, which is the existing precedent for this file, not new
// practice this change introduces). After priming, the value lives purely
// in-memory and is only ever advanced by this process's own rotations
// (see the generation field's doc comment), so every subsequent call is a
// map lookup.
//
// The registry read here is intentionally lock-free (no registryFileMu, no
// cross-process filelock) — it's a best-effort seed, not a correctness-
// critical read: if it races a concurrent writer and sees a slightly stale
// value, the persisted Generation this process seeds from was itself either
// written by this same process in a prior run, or (in the narrower,
// pre-existing cross-process-same-bus-writer case that this fix does not
// newly create — see resetRegistrySeq's doc comment) by a peer process,
// and worst case this process's first generation-fenced write for busID is
// skipped as a mismatch and self-heals on the next append, same as any
// other best-effort skip on this path.
//
// Caller must hold m.mu.
func (m *BusSessionManager) currentGenerationLocked(busID string) int64 {
	if g, ok := m.generation[busID]; ok {
		return g
	}
	var g int64
	for _, entry := range m.loadRegistry() {
		if entry.BusID == busID {
			g = entry.Generation
			break
		}
	}
	m.generation[busID] = g
	return g
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
//
// gen fences this write against a rotation that has ALREADY completed for
// this bus by the time this call reaches the lock: this call and
// resetRegistrySeq both run outside m.mu with no other ordering between
// them, so a call like this one — computed for the generation that was
// current when its seq was assigned — can be delayed (GC pause, scheduler
// preemption, or simply losing a race against a rotating append's much
// heavier resetRegistrySeq path) long enough for a LATER rotation's reset to
// land first. Without the gen check, the `entry.LastEventSeq >= seq` guard
// alone doesn't catch this: a fresh reset sets LastEventSeq to 0, so any
// positive seq from the stale pre-rotation generation now looks newer and
// overwrites it — silently reinstating the pre-rotation terminal seq and
// freezing every post-rotation append's small seq (1, 2, 3, …) behind that
// guard forever, exactly the bug resetRegistrySeq exists to fix, just
// reached by a race instead of a deterministic gap. Requiring gen to match
// the persisted entry.Generation exactly makes that reordering impossible to
// apply, not merely unlikely: a write for a generation the registry has
// already moved past (or hasn't reached yet, which "≠" also catches, though
// that direction shouldn't arise given gen is only ever captured from a
// monotonically-advancing in-process counter) is rejected outright.
func (m *BusSessionManager) updateRegistrySeqIfNewer(busID string, seq int, gen int64, ts string) {
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
			if entry.Generation != gen {
				// This write belongs to a generation the registry has since
				// moved past (a rotation's reset landed first) or has not
				// yet caught up to — either way, applying LastEventSeq/seq
				// here would relate two different files' seq spaces. Skip;
				// best-effort, self-heals once a write for the registry's
				// actual current generation arrives.
				return
			}
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

// resetRegistrySeq resets busID's registry entry to reflect the fresh
// events.jsonl a size-based rotation just created: LastEventSeq and
// EventCount return to 0, Generation advances to gen, LastEventAt is
// stamped with ts. This completes the symmetry AppendEvent's rotation path
// starts by resetting the in-memory cache (m.lastSeq[busID] = 0,
// m.lastHash[busID] = "").
//
// Before this method existed only the in-memory half of that reset ever
// happened on the automatic size-rotation path: the registry kept the old
// file's terminal seq (e.g. 109619) while the in-memory cursor restarted at
// 0, so every following append's seq (1, 2, 3, …) tripped
// updateRegistrySeqIfNewer's `entry.LastEventSeq >= seq` monotonic guard and
// was silently dropped — forever, since the cache never returns to a value
// large enough to pass the guard again. The registry entry's LastEventAt
// froze at the moment of the bus's first rotation even though the bus kept
// being written to.
//
// Unlike updateRegistrySeqIfNewer, this BLOCKS on the registry lock (like
// RegisterBus) instead of skipping under contention — deliberately, not by
// oversight. Rotation is rare (once per eventsFileMaxBytes of writes, not
// once per event), so blocking here is cheap and does not reintroduce the
// #505 thread-storm updateRegistrySeqIfNewer's TryLock exists to avoid. It
// also matters more here than on the per-event path: a skipped *advance*
// self-heals on the very next append (later, larger seq values still pass
// the guard), but a skipped *reset* would leave the registry pinned to the
// old file's terminal seq while the in-memory cursor has already restarted
// at 0 — reproducing this exact bug for every append until the NEXT
// rotation attempts another reset.
//
// gen must be strictly greater than the persisted entry.Generation to apply
// — NOT unconditional, unlike the pre-generation-fencing version of this
// method. Two rotations of the same bus can themselves race (this method
// always blocks rather than skipping, so nothing here prevents a second,
// later-captured rotation's reset from reaching the lock before an earlier
// one that got descheduled); accepting an out-of-order reset would let a
// stale gen clobber Generation backward, which would then make every
// legitimate advance captured under the true (already-higher) in-memory
// generation permanently mismatch the regressed persisted value — the same
// frozen-registry failure mode, reached through the reset path instead of
// the advance path. Requiring strict-greater makes a stale reset a no-op
// instead of a regression, symmetric with updateRegistrySeqIfNewer's exact-
// match guard.
func (m *BusSessionManager) resetRegistrySeq(busID, ts string, gen int64) {
	m.registryFileMu.Lock()
	defer m.registryFileMu.Unlock()

	if err := os.MkdirAll(m.BusesDir(), 0755); err != nil {
		slog.Warn("bus: registry seq reset mkdir failed", "err", err, "bus_id", busID)
		return
	}
	lock, err := filelock.Acquire(m.RegistryPath()+".lock", registryLockTimeout)
	if err != nil {
		slog.Warn("bus: registry seq reset could not acquire lock", "err", err, "bus_id", busID)
		return
	}
	defer lock.Release()

	registry := m.loadRegistry()
	for i, entry := range registry {
		if entry.BusID == busID {
			if gen <= entry.Generation {
				// A newer generation's reset already landed (or somehow
				// this bus's persisted generation is already ahead) —
				// applying this one would regress Generation and freeze
				// the registry against the actual current generation's
				// future advances. Skip.
				return
			}
			registry[i].Generation = gen
			registry[i].LastEventSeq = 0
			registry[i].LastEventAt = ts
			registry[i].EventCount = 0
			break
		}
	}
	if err := m.saveRegistry(registry); err != nil {
		slog.Warn("bus: failed to reset registry seq after rotation", "err", err, "bus_id", busID)
	}
}

// appendEventPostUnlockHook, when non-nil, is invoked by AppendEvent
// immediately after releasing m.mu and before the registry seq write
// (resetRegistrySeq / updateRegistrySeqIfNewer), with the generation that
// write is about to use. Always nil in production — this is a
// synchronization seam for deterministically reproducing the ordering race
// generation fencing closes: tests use it to pause a non-rotating append
// right after it releases m.mu (mirroring "goroutine A" in the reviewer
// finding this fixes) until a separately-driven rotating append's reset has
// already landed (mirroring "goroutine B"), then let the paused append's
// stale-generation write proceed and assert it was rejected. Same spirit as
// eventsFileMaxBytes above being a var instead of a const for test control.
var appendEventPostUnlockHook func(busID string, rotated bool, gen int64)

// recordRegistrySeqSkip increments the skip counter and logs at a rate
// limit — see registrySeqSkipLogEvery.
func (m *BusSessionManager) recordRegistrySeqSkip(busID, reason string) {
	n := m.registrySeqSkipCount.Add(1)
	if n == 1 || n%registrySeqSkipLogEvery == 0 {
		slog.Warn("bus: skipping registry seq update (lock contended)",
			"bus_id", busID, "reason", reason, "skip_count", n)
	}
}

// readCacheLockFor returns the per-bus mutex that serializes
// ReadEventsSince calls against busID's cache, creating it on first use.
// Held only briefly (a map lookup) — the returned lock, not this one, is
// what callers hold across the actual file I/O.
func (m *BusSessionManager) readCacheLockFor(busID string) *sync.Mutex {
	m.readCacheMetaMu.Lock()
	defer m.readCacheMetaMu.Unlock()
	lock, ok := m.readCacheLocks[busID]
	if !ok {
		lock = &sync.Mutex{}
		m.readCacheLocks[busID] = lock
	}
	return lock
}

// touchReadCacheLRU marks busID most-recently-used and evicts the
// least-recently-used bus cache(s) if that pushes the tracked set past
// maxReadCacheBuses. Must be called with readCacheMetaMu held.
//
// Bounds readCache (#561 follow-up): without this, readCache retains one
// fully-parsed event history plus a seen map[int]bool per bus FOREVER —
// across hundreds of buses that never stop being polled even after they go
// quiet, that is an unbounded-memory trade for the unbounded-CPU bug this
// cache exists to fix. An LRU cap on bus COUNT is the simplest mechanism
// that actually bounds the failure mode observed in practice (many buses,
// each individually cache-bounded already by rotation — see
// maxReadCacheEventsPerBus below for the case rotation doesn't cover): it
// needs no TTL clock, no background sweep, and eviction is driven by the
// same call path that would otherwise grow the map, so it can't fall behind.
func (m *BusSessionManager) touchReadCacheLRU(busID string) {
	if el, ok := m.readCacheLRUIdx[busID]; ok {
		m.readCacheLRU.MoveToFront(el)
		return
	}
	m.readCacheLRUIdx[busID] = m.readCacheLRU.PushFront(busID)

	for m.readCacheLRU.Len() > maxReadCacheBuses {
		back := m.readCacheLRU.Back()
		if back == nil {
			break
		}
		oldest := back.Value.(string)
		if oldest == busID {
			// maxReadCacheBuses < 1 (misconfigured); never evict the entry
			// we were just asked to touch.
			break
		}
		m.evictReadCacheLocked(oldest)
	}
}

// evictReadCacheLocked drops busID's cache entry, per-bus lock, and LRU
// bookkeeping. Must be called with readCacheMetaMu held.
//
// Safe at any moment: busReadCache is purely a derived memoization of
// events.jsonl, never its own source of truth. Dropping an entry costs the
// next reader of busID a full re-parse from offset 0 — same cost the
// pre-#561 implementation always paid — but produces byte-identical results,
// because ReadEventsSince rebuilds a nil/zero-value cache exactly the way it
// rebuilds one after a detected rotation. If another goroutine is
// concurrently mid-read for busID (holding the lock this deletes from
// readCacheLocks via its own already-fetched pointer), eviction here doesn't
// disturb it: it finishes against its own private lock and cache objects,
// and any future caller for busID simply gets a freshly created lock+cache
// pair. No shared mutable state crosses that split, so this cannot race.
func (m *BusSessionManager) evictReadCacheLocked(busID string) {
	delete(m.readCache, busID)
	delete(m.readCacheLocks, busID)
	if el, ok := m.readCacheLRUIdx[busID]; ok {
		m.readCacheLRU.Remove(el)
		delete(m.readCacheLRUIdx, busID)
	}
}

// ReadEventsSince returns busID's events with Seq > afterSeq, de-duped by
// Seq, in Seq-ascending order. afterSeq <= 0 returns the full history —
// the same contract ReadEvents(busID) has always had (it is now a thin
// wrapper: ReadEventsSince(busID, 0)). A cursor at or beyond the bus's
// current tip returns an empty, non-nil-error slice rather than erroring.
//
// Fixes #561: the naive implementation this replaced re-opened
// events.jsonl and re-Unmarshal'd every line on every call — O(entire bus
// history) per poll, measured at ~50% of kernel CPU against a 1.3 GB bus
// store with ~30 live pollers (see the issue's pprof evidence). This
// method instead maintains a per-bus busReadCache: the byte offset through
// which the file has already been parsed, plus the de-duped events parsed
// so far. Each call Seeks to that offset and only reads/Unmarshal's the
// bytes appended since the LAST call against this manager for this bus —
// so a steady-state poller pays for the handful of new events since its
// last poll, not the whole file, regardless of which caller (or which
// cursor value) triggers the read: ReadEvents(busID) and every
// ReadEventsSince(busID, N) for this bus all share the same cache, so the
// full-history callers benefit too, not just the cursor-aware ones.
//
// Concurrency: a per-bus lock (readCacheLockFor) serializes cache access
// for the SAME bus without serializing reads across different buses, and
// is held across the incremental file read — safe because it's a strict
// subset of a single bus's read path, never nested with m.mu (the append
// lock), so it cannot introduce a new deadlock ordering with AppendEvent.
//
// Rotation: detected by FILE IDENTITY (os.SameFile, i.e. device+inode on
// Darwin/Linux), not by size. An earlier version of this cache compared
// fi.Size() < cache.offset, which is a false negative whenever the fresh
// post-rotation file has already grown back to or past the stale cached
// offset before the next read for that bus — the code would then Seek to a
// meaningless byte offset in the brand-new file and silently discard every
// event before it, with cache.offset re-syncing afterward so the loss was
// never surfaced (#561 review). Identity survives growth: AppendEvent's
// rotation path renames the old file away and os.Create's a genuinely new
// one, so the inode always changes at rotation regardless of how much the
// fresh file has grown by the time this next reads it. os.SameFile returns
// false (never panics) whenever either side's identity can't be determined
// — including cache.fi's zero value on a bus's very first read — so an
// unknown identity is conservatively treated as "assume rotation, do a full
// re-read" rather than trusting a stale offset. The old size-shrink check
// is kept as a cheap secondary signal, not the primary one.
//
// Bounding: readCache is capped at maxReadCacheBuses distinct buses (LRU
// eviction, touchReadCacheLRU) and at maxReadCacheEventsPerBus parsed
// events per bus (checked below) — see maxReadCacheBuses's doc comment for
// why an unbounded cache is the same class of bug #561 fixes, just in
// memory instead of CPU. Evicting a cache entry is always safe: see
// evictReadCacheLocked.
//
// Aliasing (deliberately NOT deep-copied, see #561 review's unverified
// note): the returned []BusBlock is a fresh slice (copy() below), but each
// element's Payload (a map) and Prev (a slice) are shallow-copied struct
// fields, so every caller's returned events alias the SAME Payload map /
// Prev backing array held in cache.events — and thus each other, across
// every past and future caller of this bus. A real deep copy here would
// mean re-walking and re-allocating every returned event's Payload map on
// every call, which for a full-history caller (ReadEvents == afterSeq 0)
// is exactly the O(entire bus history)-per-call cost class #561 exists to
// eliminate — it would erode the fix for the callers most likely to invoke
// it repeatedly (sessions.go, peer_awareness_query.go, session_fork.go all
// call ReadEvents to recompute full state). Deferred rather than fixed:
// audited as of this PR, no caller mutates a returned event's Payload or
// Prev in place. THE INVARIANT ANY FUTURE CALLER MUST HOLD: treat returned
// BusBlock.Payload/.Prev as read-only. Mutating either in place corrupts
// the shared cache for every other past/future reader of this bus without
// holding readCacheLockFor's lock — i.e. a data race plus cross-caller
// state leakage. A caller that needs to mutate must copy Payload/Prev
// itself first.
func (m *BusSessionManager) ReadEventsSince(busID string, afterSeq int) ([]BusBlock, error) {
	if !validPathComponent(busID) {
		return nil, fmt.Errorf("invalid bus_id %q", busID)
	}
	eventsFile := m.EventsPath(busID)

	lock := m.readCacheLockFor(busID)
	lock.Lock()
	defer lock.Unlock()

	m.readCacheMetaMu.Lock()
	cache := m.readCache[busID]
	if cache == nil {
		cache = &busReadCache{seen: make(map[int]bool)}
		m.readCache[busID] = cache
	}
	m.touchReadCacheLRU(busID)
	m.readCacheMetaMu.Unlock()

	f, err := os.Open(eventsFile)
	if err != nil {
		if os.IsNotExist(err) {
			// No file yet — nothing to cache either (a stale cache here
			// would mean the bus's events.jsonl was removed out from under
			// us, which EnsureBus/AppendEvent never do apart from the
			// rotation rename+recreate handled below).
			cache.offset = 0
			cache.events = nil
			cache.seen = make(map[int]bool)
			cache.fi = nil
			return nil, nil
		}
		return nil, fmt.Errorf("open events file: %w", err)
	}
	defer f.Close()

	fi, statErr := f.Stat()
	if statErr != nil {
		return nil, fmt.Errorf("stat events file: %w", statErr)
	}

	// File-identity check is primary: a rotation always swaps in a new
	// inode (see the doc comment above), and unlike a size comparison this
	// isn't fooled by the fresh file growing past the stale offset before
	// we get here. os.SameFile safely returns false — it never panics —
	// when cache.fi is nil (no prior identity recorded yet), which is
	// exactly the conservative "unknown means treat as rotated" behavior
	// wanted; on a genuinely fresh cache this is a costless no-op since
	// cache.offset is already 0.
	rotated := !os.SameFile(cache.fi, fi)

	// Cheap secondary signal, kept as a backstop: a file that has SHRUNK
	// relative to what we've already parsed cannot be a pure continuation
	// of the cache even in the (implausible) case identity matched.
	if !rotated && fi.Size() < cache.offset {
		rotated = true
	}

	if rotated {
		cache.offset = 0
		cache.events = nil
		cache.seen = make(map[int]bool)
	}
	cache.fi = fi

	if fi.Size() > cache.offset {
		if _, err := f.Seek(cache.offset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek events file: %w", err)
		}
		reader := bufio.NewReaderSize(f, 256*1024)
		var consumed int64
		for {
			lineBytes, readErr := reader.ReadBytes('\n')
			if n := len(lineBytes); n > 0 && lineBytes[n-1] == '\n' {
				consumed += int64(n)
				if line := lineBytes[:n-1]; len(line) > 0 {
					var block BusBlock
					if err := json.Unmarshal(line, &block); err == nil {
						if !cache.seen[block.Seq] {
							cache.seen[block.Seq] = true
							cache.events = append(cache.events, block)
						}
					}
					// Malformed lines are silently skipped — same as the
					// scanner-based implementation this replaced.
				}
			}
			if readErr != nil {
				// EOF (almost always io.EOF) with no trailing newline on
				// the final chunk means a concurrent AppendEvent's write is
				// either in flight or was torn. Deliberately do NOT count
				// those trailing bytes as consumed: the next call re-reads
				// from the same offset and picks up the completed line
				// once the writer finishes, exactly as a fresh full scan
				// starting from byte 0 would have.
				break
			}
		}
		cache.offset += consumed
	}

	// result's elements alias cache.events' Payload maps / Prev slices —
	// read-only for every caller; see the aliasing paragraph in this
	// method's doc comment.
	var result []BusBlock
	if afterSeq <= 0 {
		result = make([]BusBlock, len(cache.events))
		copy(result, cache.events)
	} else {
		// cache.events is Seq-ascending, so the wanted suffix starts at the
		// first entry past afterSeq. Linear scan, not binary search: the
		// steady-state caller (a poller) supplies a cursor near the tail, so
		// this is typically a handful of comparisons, not a real search cost.
		start := len(cache.events)
		for i, e := range cache.events {
			if e.Seq > afterSeq {
				start = i
				break
			}
		}
		result = make([]BusBlock, len(cache.events)-start)
		copy(result, cache.events[start:])
	}

	// Per-bus event-count bound (#561 follow-up, maxReadCacheEventsPerBus):
	// a bus normally stays bounded by eventsFileMaxBytes (rotation clears
	// its cache — see the identity check above), but a very high volume of
	// small events can accumulate a large parsed slice before that
	// byte-size threshold is ever crossed. Once this bus's cache would
	// exceed the cap, drop it so the NEXT read starts clean and re-parses
	// from offset 0. Safe to do after result is already computed: this
	// call's answer was built from the cache as it stood a moment ago, so
	// resetting it now cannot change what's returned here (see
	// evictReadCacheLocked's doc comment for the same argument in the LRU
	// case — this is the identical invariant, applied per-bus instead of
	// across buses).
	if len(cache.events) > maxReadCacheEventsPerBus {
		cache.offset = 0
		cache.events = nil
		cache.seen = make(map[int]bool)
	}

	return result, nil
}

// ReadEvents reads all events from a bus. De-dups by seq (file may have
// duplicates from crash recovery). Equivalent to ReadEventsSince(busID, 0)
// — kept as its own name because most callers want "everything", and that
// reads better at those call sites than a literal 0 cursor would. See
// ReadEventsSince for the caching behaviour this now shares.
func (m *BusSessionManager) ReadEvents(busID string) ([]BusBlock, error) {
	return m.ReadEventsSince(busID, 0)
}
