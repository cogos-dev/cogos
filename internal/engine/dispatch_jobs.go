// dispatch_jobs.go — async job handle registry for cog_dispatch_to_harness.
//
// Problem: cog_dispatch_to_harness is fully synchronous — the MCP request
// blocks until every fan-out slot completes, errors, or times out (up to
// dispatch_timeout_cap_seconds, default 600s). Some MCP clients (notably the
// window an interactive Claude Code turn gets before the harness gives up
// and the request context is torn down) close well before that. The caller
// never sees the result even though the dispatch completed successfully.
//
// This file adds an optional async path: when the caller sets Async=true on
// the dispatchToHarnessInput, toolDispatchToHarness registers a job, spawns
// the real dispatch in a goroutine running under a DETACHED context (see
// dispatch_jobs.go's use of context.WithoutCancel in mcp_server.go), and
// returns a job handle immediately. The caller polls the job by id — via the
// HTTP surface (GET /v1/dispatch-jobs/{id}, the primary poll surface per the
// design) or the cog_poll_dispatch MCP convenience tool — until it reaches a
// terminal state (done/failed).
//
// Detach-from-request-context rationale (#432, e4e6aca): the MCP request's
// context is canceled the instant toolDispatchToHarness returns the receipt.
// If the spawned goroutine inherited that context unmodified, the dispatch
// would be canceled out from under itself the moment the receipt was
// returned — reintroducing exactly the zombie-generation / cancel-races-
// ahead-of-server-completion failure mode #432 fixed for the synchronous
// path. context.WithoutCancel severs that link; a fresh WithTimeout derived
// from the request's TimeoutSeconds (falling back to the dispatch default)
// bounds the goroutine's lifetime independently.
package engine

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DispatchJobState is the lifecycle state of one async dispatch job.
type DispatchJobState string

const (
	DispatchJobPending DispatchJobState = "pending"
	DispatchJobRunning DispatchJobState = "running"
	DispatchJobDone    DispatchJobState = "done"
	DispatchJobFailed  DispatchJobState = "failed"
)

// dispatchJobTTL is how long a terminal (done/failed) job record is kept
// before lazy GC reclaims it. 30 minutes gives a caller ample time to poll
// after the dispatch finishes without the registry growing unbounded across
// a long-running kernel process. Not caller-configurable today — this is
// kernel-internal bookkeeping, not a dispatch parameter.
const dispatchJobTTL = 30 * time.Minute

// DispatchJobRecord is one entry in the DispatchJobRegistry. Result and Err
// are populated only once State reaches a terminal value (done/failed).
type DispatchJobRecord struct {
	JobID     string
	CycleID   string // correlates to the harness.dispatch.* ledger events (local_agent_harness.go)
	State     DispatchJobState
	Result    *DispatchBatchResult
	Err       string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// snapshot returns a defensive copy safe to hand to a caller outside the
// registry's lock.
func (r *DispatchJobRecord) snapshot() *DispatchJobRecord {
	if r == nil {
		return nil
	}
	cp := *r
	return &cp
}

// DispatchJobRegistry is an in-memory, mutex-guarded store of async dispatch
// jobs keyed by a freshly-minted UUID (NOT the per-slot content-keyed
// RequestID computed inside dispatchSlot — that key collides on identical
// dispatch content by design, which makes it unfit as a job-registry primary
// key; see local_agent_harness.go's dispatchSlot contentKey and the KV-cache-
// branching rationale documented there).
//
// Lazy GC: expired terminal records are swept opportunistically on every
// Create call rather than on a background ticker — this keeps the registry
// dependency-free (no goroutine to start/stop) at the cost of only reclaiming
// memory when new jobs are created. Acceptable for the expected usage shape
// (jobs created steadily during normal operation); a kernel that dispatches
// once and never again also never accumulates more than one stale record.
type DispatchJobRegistry struct {
	mu   sync.Mutex
	jobs map[string]*DispatchJobRecord
	ttl  time.Duration
	now  func() time.Time // overridable for tests
}

// NewDispatchJobRegistry constructs an empty registry with the default TTL.
func NewDispatchJobRegistry() *DispatchJobRegistry {
	return &DispatchJobRegistry{
		jobs: make(map[string]*DispatchJobRecord),
		ttl:  dispatchJobTTL,
		now:  time.Now,
	}
}

// Create allocates a new job in the pending state and returns its id. Sweeps
// expired terminal records first (lazy GC).
func (reg *DispatchJobRegistry) Create(cycleID string) string {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.gcLocked()

	jobID := uuid.NewString()
	now := reg.now()
	reg.jobs[jobID] = &DispatchJobRecord{
		JobID:     jobID,
		CycleID:   cycleID,
		State:     DispatchJobPending,
		CreatedAt: now,
		UpdatedAt: now,
	}
	return jobID
}

// MarkRunning transitions a job to running. No-op if the job is unknown or
// already terminal (defensive — should not happen in normal operation since
// exactly one goroutine owns each job's transitions).
func (reg *DispatchJobRegistry) MarkRunning(jobID string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	rec, ok := reg.jobs[jobID]
	if !ok || isTerminal(rec.State) {
		return
	}
	rec.State = DispatchJobRunning
	rec.UpdatedAt = reg.now()
}

// Complete transitions a job to done with the given result.
func (reg *DispatchJobRegistry) Complete(jobID string, result *DispatchBatchResult) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	rec, ok := reg.jobs[jobID]
	if !ok {
		return
	}
	rec.State = DispatchJobDone
	rec.Result = result
	rec.UpdatedAt = reg.now()
}

// Fail transitions a job to failed with the given error message.
func (reg *DispatchJobRegistry) Fail(jobID string, errMsg string) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	rec, ok := reg.jobs[jobID]
	if !ok {
		return
	}
	rec.State = DispatchJobFailed
	rec.Err = errMsg
	rec.UpdatedAt = reg.now()
}

// Get returns a defensive copy of the job record, or (nil, false) when the
// id is unknown (never registered, or already GC'd past its TTL).
func (reg *DispatchJobRegistry) Get(jobID string) (*DispatchJobRecord, bool) {
	reg.mu.Lock()
	defer reg.mu.Unlock()
	rec, ok := reg.jobs[jobID]
	if !ok {
		return nil, false
	}
	return rec.snapshot(), true
}

// gcLocked removes terminal records older than ttl. Caller must hold reg.mu.
func (reg *DispatchJobRegistry) gcLocked() {
	cutoff := reg.now().Add(-reg.ttl)
	for id, rec := range reg.jobs {
		if isTerminal(rec.State) && rec.UpdatedAt.Before(cutoff) {
			delete(reg.jobs, id)
		}
	}
}

func isTerminal(s DispatchJobState) bool {
	return s == DispatchJobDone || s == DispatchJobFailed
}

// dispatchAsyncDefaultTimeoutSeconds bounds the detached goroutine's context
// when the request carried no explicit TimeoutSeconds. Mirrors
// dispatchTimeoutDefault (agent_dispatch_query.go) so the async path's
// no-timeout-specified behavior matches the synchronous path's.
const dispatchAsyncDefaultTimeoutSeconds = dispatchTimeoutDefault

// detachedDispatchContext returns a context that has severed cancellation
// from parent (see package doc above / #432 e4e6aca) but still carries a
// bounded deadline of its own. seconds<=0 falls back to
// dispatchAsyncDefaultTimeoutSeconds.
func detachedDispatchContext(parent context.Context, seconds int) (context.Context, context.CancelFunc) {
	if seconds <= 0 {
		seconds = dispatchAsyncDefaultTimeoutSeconds
	}
	detached := context.WithoutCancel(parent)
	return context.WithTimeout(detached, time.Duration(seconds)*time.Second)
}
