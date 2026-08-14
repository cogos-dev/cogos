// provider_queue.go — kernel-owned per-backend FIFO queue for local
// OpenAI-compat backends (issue #556).
//
// Operator directive 2026-08-13: "this is where the kernel can present the
// endpoint for LMS and embed its own queue interstitially." Local LM Studio
// backends now run parallel=1 by ruling (#555) — concurrency queues at the
// source, but LMS's own queue is a black box: no depth endpoint, no wait
// metrics, only TTFT stretch. This file moves that queue INTO the kernel
// where it is a first-class, observable resource: depth + wait exposed on
// the vitals surface (host_vitals.go), via response headers
// (X-Cogos-Queue-Depth / X-Cogos-Queue-Wait-Ms, set in serve.go/
// serve_anthropic.go), and via a live snapshot (GET /v1/queue, serve_queue.go).
//
// Two pieces:
//
//  1. backendQueue — a ticket-based FIFO semaphore. Deliberately NOT a plain
//     buffered-channel semaphore: a channel-based semaphore has no way to
//     excise a specific waiter on ctx cancellation without disturbing FIFO
//     order for everyone behind it. Each waiter gets its own ticket (with its
//     own signal channel) parked on a doubly-linked list; Release always
//     grants the front of the list, and a canceled waiter removes its own
//     ticket from the list before giving up.
//
//  2. queuedProvider — a Provider decorator (mirrors countingProvider in
//     local_agent_harness.go) that wraps ONE local backend's Provider and
//     gates Complete/CompleteCancelSafe/Stream through that backend's
//     backendQueue. Constructed once per backend in router.go's makeProvider
//     — no call site elsewhere in the codebase changes; HTTP chat, dispatch
//     fan-out, tool-loop re-calls, and the autonomic consult all get queued
//     for free because they all go through the same Provider interface.
package engine

import (
	"container/list"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// ── backendQueue ─────────────────────────────────────────────────────────────

// queueTicket is one waiter's position in a backendQueue's wait list. ch is
// closed by Release to hand the slot to this ticket; enqueued is used to
// compute wait time once granted (or, for a live snapshot, wait time so far).
type queueTicket struct {
	ch       chan struct{}
	caller   string
	enqueued time.Time
}

// backendQueue is a FIFO semaphore gating concurrent access to one local
// backend, sized to that backend's declared parallelism. Safe for concurrent
// use.
type backendQueue struct {
	mu          sync.Mutex
	name        string
	key         string // the canonical backendQueues registry key (queueKey) this queue is stored under — see newQueuedProvider and Snapshot's Endpoint field. Empty for queues constructed directly by tests via newBackendQueue rather than through newQueuedProvider.
	concurrency int
	inFlight    int
	waiters     *list.List // of *queueTicket, oldest at Front
}

// newBackendQueue constructs a backendQueue with the given concurrency.
// concurrency < 1 is clamped to 1 — a queue that admits nothing is a
// deadlock, not a safe default.
func newBackendQueue(name string, concurrency int) *backendQueue {
	if concurrency < 1 {
		concurrency = 1
	}
	return &backendQueue{
		name:        name,
		concurrency: concurrency,
		waiters:     list.New(),
	}
}

// Acquire blocks until a slot is available or ctx is done. On success it
// returns a release func the caller MUST call exactly once (defer it
// immediately) to free the slot. position/waitMs describe THIS caller's own
// admission: position=0 and waitMs=0 mean the caller ran immediately;
// otherwise position is the 1-based queue position observed at enqueue time
// and waitMs is the time actually spent waiting before being granted.
//
// callerID is attribution (RequestMetadata.Attribution, or "anonymous") —
// recorded on the ticket for potential future internal/gated observability.
// It plays no role in scheduling, which is strict FIFO regardless of caller
// identity, and — as of the #556 repair round-1 security fix — it is NOT
// surfaced on GET /v1/queue, which is unauthenticated (see
// queueCallerSnapshot's doc comment).
func (q *backendQueue) Acquire(ctx context.Context, callerID string) (release func(), position int, waitMs int64, err error) {
	// Fail fast on an already-canceled/expired ctx before ever touching
	// q.inFlight: without this check, the fast path below grants a slot
	// unconditionally (it never consults ctx at all), so a caller whose
	// context is already done — e.g. an HTTP client that disconnected before
	// dispatch — would still be admitted, still occupy the backend's slot,
	// and still trigger an upstream generation nobody will read. This is a
	// best-effort check, not a guarantee against a race immediately after
	// this point; the ctx.Done() branch in the wait-list path below remains
	// the authoritative cancellation handling for a caller that goes away
	// while actually queued.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, 0, 0, ctxErr
	}

	q.mu.Lock()
	if q.inFlight < q.concurrency {
		q.inFlight++
		q.mu.Unlock()
		return q.makeRelease(), 0, 0, nil
	}

	t := &queueTicket{ch: make(chan struct{}), caller: callerID, enqueued: time.Now()}
	elem := q.waiters.PushBack(t)
	position = q.waiters.Len()
	q.mu.Unlock()

	select {
	case <-t.ch:
		waitMs = time.Since(t.enqueued).Milliseconds()
		return q.makeRelease(), position, waitMs, nil
	case <-ctx.Done():
		q.mu.Lock()
		removed := false
		for e := q.waiters.Front(); e != nil; e = e.Next() {
			if e == elem {
				q.waiters.Remove(e)
				removed = true
				break
			}
		}
		q.mu.Unlock()
		if !removed {
			// Release already popped this ticket concurrently with our own
			// cancellation — it is in the process of (or has already)
			// closing t.ch to hand us the slot. There is no further
			// blocking work on the Release side between the pop and the
			// close, so waiting here is bounded and safe. Drain the
			// handoff and immediately release it (we don't want the slot,
			// ctx is already done) rather than silently leaking it —
			// that's the caller-disconnects-while-waiting case this ticket
			// design exists to close.
			<-t.ch
			q.release()
		}
		return nil, 0, 0, ctx.Err()
	}
}

// makeRelease wraps q.release in a sync.Once so a caller that (incorrectly)
// invokes the returned func more than once cannot double-free the slot.
func (q *backendQueue) makeRelease() func() {
	var once sync.Once
	return func() {
		once.Do(q.release)
	}
}

// release frees one slot: if a waiter is queued AND admitting it would not
// push inFlight over the current concurrency ceiling, the slot transfers
// directly to the oldest waiter (true FIFO — inFlight is unchanged,
// ownership moves from the releasing caller to the waiter); otherwise
// inFlight is decremented and any waiters stay parked.
//
// #556 repair (round 3): this used to transfer to the front waiter
// UNCONDITIONALLY whenever one was queued, never checking inFlight against
// concurrency first. setConcurrency's narrowing path (below) only removes
// capacity lazily — by design, it does not preempt callers already holding
// a slot — and relies on release() to enforce the new, lower ceiling as
// those callers finish. Because release() never consulted concurrency, a
// narrowing (e.g. an operator lowering options.model_state.parallel from 4
// to 1 while a backlog existed) never actually took effect: each release()
// kept handing the freed slot straight to the next waiter, holding inFlight
// at its pre-narrowing level indefinitely. The check here is
// `q.inFlight <= q.concurrency` (not `<`) evaluated BEFORE any decrement,
// which is deliberately the same admission test Acquire's fast path uses —
// it correctly falls through to granting in the steady state (no pending
// narrowing) where inFlight always equals concurrency when a waiter is
// present, and correctly withholds admission while inFlight is still above
// the new ceiling, letting it drain one release() at a time until it is
// not.
func (q *backendQueue) release() {
	q.mu.Lock()
	if e := q.waiters.Front(); e != nil && q.inFlight <= q.concurrency {
		q.waiters.Remove(e)
		t := e.Value.(*queueTicket)
		q.mu.Unlock()
		close(t.ch)
		return
	}
	if q.inFlight > 0 {
		q.inFlight--
	}
	q.mu.Unlock()
}

// setConcurrency reconciles the queue's admitted concurrency to n (clamped
// to a minimum of 1, mirroring newBackendQueue), for the case where a queue
// already exists (newQueuedProvider's LoadOrStore load-hit) but the caller's
// own declared concurrency differs from what the queue currently enforces —
// e.g. providers.yaml's options.model_state.parallel was edited between
// dispatches (#556 repair round 2).
//
// Narrowing (n < current concurrency) takes effect lazily: callers already
// holding a slot are not preempted, and — as of the #556 repair (round 3)
// fix to release() — no additional slot is granted to a waiter until
// inFlight naturally drops back under the new, lower ceiling via a normal
// release(); release() now checks inFlight against concurrency before
// transferring a slot, so this is actually enforced rather than merely
// documented. Widening (n > current concurrency) takes effect immediately:
// capacity newly freed by the increase is handed to waiters at the front of
// the FIFO list right away — the same grant release() performs — rather
// than leaving them parked until some unrelated future release() call that,
// if the backend is otherwise idle, might never come.
func (q *backendQueue) setConcurrency(n int) {
	if n < 1 {
		n = 1
	}
	q.mu.Lock()
	q.concurrency = n
	var granted []*queueTicket
	for q.inFlight < q.concurrency {
		e := q.waiters.Front()
		if e == nil {
			break
		}
		q.waiters.Remove(e)
		q.inFlight++
		granted = append(granted, e.Value.(*queueTicket))
	}
	q.mu.Unlock()
	for _, t := range granted {
		close(t.ch)
	}
}

// queueCallerSnapshot is one waiter's observable state for GET /v1/queue.
//
// Deliberately does NOT include the ticket's caller attribution. GET
// /v1/queue is grant-exempt like every other GET request
// (isGrantExemptRequest, serve_grant_auth.go) — reachable with no auth at
// all — so exposing each waiter's resolved bound-identity subject here would
// leak who is currently calling the kernel to anyone who can reach the port.
// The queue itself still tracks attribution internally (queueTicket.caller)
// for potential future gated/authenticated views; this snapshot just never
// surfaces it on the open read surface.
type queueCallerSnapshot struct {
	Position  int   `json:"position"`
	WaitingMs int64 `json:"waiting_ms"`
}

// queueSnapshot is a point-in-time read of a backendQueue's state.
type queueSnapshot struct {
	Name string `json:"name"`
	// Endpoint is a REDACTED form of the queue's stable registry identity
	// — host:port only, via redactQueueEndpoint — not the raw registry key
	// (backendQueues' map key; see newQueuedProvider). #556 repair (round
	// 3): Name is NOT stable — it is whichever call site's displayName
	// happened to win newQueuedProvider's LoadOrStore race first (e.g.
	// "agent-local" vs "lmstudio-darkstar" vs "lmstudio" for the exact
	// same physical backend, depending on which of
	// buildLocalProvider/makeProvider/autoDiscoverOpenAICompat ran
	// first), so it is not a reliable identifier for a shared queue.
	// Endpoint is, which is why round 3 added it here.
	//
	// #556 repair (round 4): round 3 shipped Endpoint as the RAW registry
	// key (q.key verbatim) — a URL that can carry userinfo credentials
	// (an operator-configured "http://svc:token@host:port/path" openai-
	// compat endpoint) and a path. GET /v1/queue is grant-exempt like
	// every other GET (isGrantExemptRequest, serve_grant_auth.go) —
	// reachable with NO authentication at all — so that raw key was
	// published to anyone who can reach the port, reopening (in the
	// opposite direction) the exact disclosure round 3's
	// queueCallerSnapshot doc comment says this route was hardened
	// against. redactQueueEndpoint strips userinfo and path before this
	// field is ever populated, publishing only host:port. Whether GET
	// /v1/queue should additionally require auth is a separate, still-
	// undecided question left to operator arbitration — this fix only
	// makes the unauthenticated response strictly less disclosive than it
	// was, it does not resolve that question.
	// Omitted for queues constructed directly by tests via newBackendQueue
	// (no queueKey to report).
	Endpoint     string                `json:"endpoint,omitempty"`
	Concurrency  int                   `json:"concurrency"`
	InFlight     int                   `json:"in_flight"`
	Waiting      int                   `json:"waiting"`
	OldestWaitMs int64                 `json:"oldest_wait_ms"`
	Callers      []queueCallerSnapshot `json:"callers,omitempty"`
}

// redactQueueEndpoint returns a redacted identity for a backend queue's
// registry key, safe to publish on the unauthenticated GET /v1/queue
// response: host:port only, with any URL userinfo (credentials) and path
// stripped. #556 repair (round 4) — see queueSnapshot.Endpoint's doc
// comment for the disclosure this closes.
//
// Registry keys are always produced by normalizeLocalLLMEndpoint /
// canonicalizeLoopbackHost (scheme://[user:pass@]host[:port][/path]), so a
// successful url.Parse with a non-empty Host is the expected case;
// u.Host never includes userinfo (that lives in u.User) or path. An
// endpoint that fails to parse, or has no host, redacts to "" rather than
// falling back to the raw input — this function only ever narrows what is
// disclosed, it never risks echoing something unredacted by accident.
func redactQueueEndpoint(key string) string {
	if key == "" {
		return ""
	}
	u, err := url.Parse(key)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Host
}

// maxQueueSnapshotCallers bounds the per-backend caller list in a /v1/queue
// snapshot — a live diagnostic read, not a paginated resource.
const maxQueueSnapshotCallers = 50

// Snapshot returns a point-in-time read of the queue's state. Best-effort,
// no I/O, cheap enough to call from an HTTP handler or a vitals tick.
func (q *backendQueue) Snapshot() queueSnapshot {
	q.mu.Lock()
	defer q.mu.Unlock()

	snap := queueSnapshot{
		Name:        q.name,
		Endpoint:    redactQueueEndpoint(q.key),
		Concurrency: q.concurrency,
		InFlight:    q.inFlight,
		Waiting:     q.waiters.Len(),
	}
	now := time.Now()
	pos := 0
	for e := q.waiters.Front(); e != nil; e = e.Next() {
		pos++
		t := e.Value.(*queueTicket)
		waitMs := now.Sub(t.enqueued).Milliseconds()
		if waitMs > snap.OldestWaitMs {
			snap.OldestWaitMs = waitMs
		}
		if len(snap.Callers) < maxQueueSnapshotCallers {
			snap.Callers = append(snap.Callers, queueCallerSnapshot{
				Position:  pos,
				WaitingMs: waitMs,
			})
		}
	}
	return snap
}

// ── registry ─────────────────────────────────────────────────────────────────

// backendQueues is the process-wide registry of backend name -> *backendQueue,
// populated by newQueuedProvider at router construction time. Mirrors the
// inflightRequests sync.Map idiom in inference_inflight.go: a package-level
// registry makes every backend's queue reachable from both the vitals
// sampler (host_vitals.go) and the GET /v1/queue handler (serve_queue.go)
// without threading queue state through Server or Router.
var backendQueues sync.Map // map[string]*backendQueue

// allBackendQueueSnapshots returns a snapshot of every registered backend
// queue, sorted by name for deterministic output. Always non-nil (an empty
// registry yields an empty, not nil, slice) so GET /v1/queue's JSON encoding
// reports "backends": [] rather than "backends": null.
func allBackendQueueSnapshots() []queueSnapshot {
	out := []queueSnapshot{}
	backendQueues.Range(func(_, v any) bool {
		q, ok := v.(*backendQueue)
		if !ok {
			return true
		}
		out = append(out, q.Snapshot())
		return true
	})
	sortQueueSnapshotsByName(out)
	return out
}

func sortQueueSnapshotsByName(snaps []queueSnapshot) {
	for i := 1; i < len(snaps); i++ {
		for j := i; j > 0 && snaps[j].Name < snaps[j-1].Name; j-- {
			snaps[j], snaps[j-1] = snaps[j-1], snaps[j]
		}
	}
}

// resetBackendQueuesForTest clears the process-wide queue registry. Test-only
// — production code never needs to unregister a backend queue.
func resetBackendQueuesForTest() {
	backendQueues.Range(func(k, _ any) bool {
		backendQueues.Delete(k)
		return true
	})
}

// ── queue observation (response-header side channel) ─────────────────────────

// queueObservation carries one HTTP request's own queue admission stats
// (backend, position, wait) out of the Provider call stack so the HTTP
// handler (serve.go/serve_anthropic.go) can set X-Cogos-Queue-Depth /
// X-Cogos-Queue-Wait-Ms on the response. queuedProvider.Acquire happens deep
// inside Complete/CompleteCancelSafe/Stream, which only receive a
// context.Context — not a response writer — so a context-carried pointer is
// the mechanism, not a change to the Provider interface or CompletionResponse.
//
// Only ever written by the queuedProvider handling THIS request's own
// admission; concurrent tool-loop re-calls on the same ctx would overwrite
// it, which is fine because handlers read it once, immediately after the
// FIRST Complete/Stream call returns and before any response body byte is
// written — precisely the "position/wait observed at the moment this
// request was dispatched" the issue asks for.
type queueObservation struct {
	mu       sync.Mutex
	set      bool
	Backend  string
	Position int
	WaitMs   int64
}

type queueObservationCtxKey struct{}

// withQueueObservation returns a context carrying obs, retrievable via
// queueObservationFromContext.
func withQueueObservation(ctx context.Context, obs *queueObservation) context.Context {
	return context.WithValue(ctx, queueObservationCtxKey{}, obs)
}

// queueObservationFromContext returns the *queueObservation stashed in ctx by
// withQueueObservation, or nil if none is present (e.g. a call path — like
// dispatch or the autonomic consult — that never wires HTTP response
// headers).
func queueObservationFromContext(ctx context.Context) *queueObservation {
	obs, _ := ctx.Value(queueObservationCtxKey{}).(*queueObservation)
	return obs
}

// record stores this request's own admission stats. Safe on a nil receiver
// (no-op) so call sites don't need a nil check before calling it.
func (o *queueObservation) record(backend string, position int, waitMs int64) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.Backend = backend
	o.Position = position
	o.WaitMs = waitMs
	o.set = true
	o.mu.Unlock()
}

// snapshot returns the recorded stats and whether anything was ever
// recorded (false means no queuedProvider handled this request — e.g. a
// cloud provider — and headers should be omitted, not reported as zero).
func (o *queueObservation) snapshot() (backend string, position int, waitMs int64, ok bool) {
	if o == nil {
		return "", 0, 0, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.Backend, o.Position, o.WaitMs, o.set
}

// ── queuedProvider ───────────────────────────────────────────────────────────

// queuedProvider decorates a local backend's Provider with the backend's
// FIFO queue, gating exactly the three methods every call path funnels
// through: Complete, CompleteCancelSafe, and Stream. Mirrors countingProvider
// (local_agent_harness.go) in shape: embed the Provider interface, override
// only what needs new behavior, let embedding promote the rest (Name, Model,
// Available, Capabilities, Ping) unchanged.
//
// ListModels/ListModelsWithContext are the one place embedding-promotion
// does NOT do what you'd want here — see the doc comments on those methods
// below — so they are forwarded explicitly.
type queuedProvider struct {
	Provider
	name  string
	queue *backendQueue
}

// newQueuedProvider wraps inner in a queuedProvider gated by the SHARED
// *backendQueue registered under queueKey in the process-wide backendQueues
// registry, so the vitals sampler and GET /v1/queue can find it without
// Server/Router plumbing. displayName is the queuedProvider's own identity —
// used for per-request observation (queueObservation.Backend) and as the
// queue's initial Snapshot().Name — and is deliberately independent of
// queueKey; see below for why.
//
// queueKey MUST be the backend's normalized physical endpoint
// (normalizeLocalLLMEndpoint / resolveLocalLLMEndpoint), NOT the provider's
// config/display name. #556 repair (round 2): round-1 keyed the registry on
// name, which let the SAME physical LM Studio process end up fronted by up
// to three independent concurrency-1 queues simultaneously —
// "agent-local" (buildLocalProvider's fixed literal), "lmstudio-darkstar"
// (makeProvider from providers.yaml), and "lmstudio" (autoDiscoverOpenAICompat)
// — because each path names it differently. Keying on the resolved endpoint
// instead means every path that ends up hitting http://localhost:1234 shares
// the one queue regardless of what each caller happens to call it, closing
// the gap where LMS could still receive concurrent generations despite
// parallel=1 declared on every path individually. The inverse also holds:
// two genuinely distinct endpoints (e.g. via COGOS_LOCAL_LLM_ENDPOINT
// pointing somewhere other than :1234) get their own queues instead of
// collapsing into one over-serialized queue, because a fixed name no longer
// collapses them.
//
// Uses LoadOrStore rather than Store: makeProvider (router.go) is called
// once per dispatch call, not once per backend, so multiple concurrent
// callers naming the same backend (e.g. two cog_dispatch_to_harness calls
// both resolving to the same endpoint) each reach this constructor
// independently. A plain Store here would let each call clobber the registry
// with its own brand-new, empty *backendQueue — the losing caller's queue
// becomes orphaned (still referenced by its queuedProvider, invisible to GET
// /v1/queue and the vitals gauges) while inFlight/concurrency enforcement
// silently splits across two separate queue objects instead of being shared,
// defeating the parallel=1 guarantee #555/#556 exist to provide.
// LoadOrStore makes registration idempotent per endpoint: the first caller
// to register a given endpoint wins and every subsequent caller (including
// this one, if it lost the race) reuses that same *backendQueue object, so
// admission and observability are correctly shared no matter how many
// Provider instances get built for the same backend.
//
// #556 repair (round 2), second regression closed here: a load-hit (this
// call lost the race and is reusing an existing queue) reconciles the
// existing queue's concurrency to THIS call's declared value via
// setConcurrency, rather than silently discarding it. Without this, a
// backend's declared parallelism froze at whatever the first-ever
// registration happened to specify — an operator lowering
// options.model_state.parallel in providers.yaml would have that tightening
// silently ignored for the life of the process, since
// local_agent_harness.go's DispatchToHarness path 1 re-reads providers.yaml
// and calls makeProvider fresh on every dispatch but always hit the
// load-and-discard branch here.
// concurrency is 0/negative when the CALLER has no declared value to offer
// ("unspecified") — buildLocalProvider (local_llm.go) and
// autoDiscoverOpenAICompat (router.go) both pass 0 for this reason, since
// neither has a providers.yaml entry to read options.model_state.parallel
// from. makeProvider's providers.yaml-declared path always passes a real
// value >= 1 (#556 repair round 4 restored its clamp — see router.go's
// makeProvider doc comment — because an ABSENT
// options.model_state.parallel key there is still a declaration, of the
// #555 backstop default). newBackendQueue still clamps an unspecified 0 to
// that same backstop the FIRST time a given queueKey is registered (a
// queue that admits nothing is a deadlock), but a load-hit (an existing
// queue for this queueKey) only reconciles concurrency when THIS call
// actually declared one (concurrency >= 1) — #556 repair (round 3):
// unspecified must never win a reconciliation, otherwise whichever of the
// three call sites happens to run next (all three share the process-wide
// registry) silently clobbers an operator-declared parallelism back down
// to the undeclared default, flapping the whole kernel's local queue
// concurrency between 4 and 1 on every autonomic tick / dispatch.
//
// #556 repair (round 4): a load-hit reconciliation used to overwrite
// outright (last writer wins). Two differently-NAMED providers.yaml
// entries can resolve to the same physical endpoint (the shared-queue-by-
// endpoint design round 2 introduced) with two different declared
// options.model_state.parallel values, and both go through this function
// within a single BuildRouter pass; Go's map iteration order over
// pcfg.Providers is randomized per process, so which declared value "won"
// was non-deterministic across otherwise-identical runs of the same
// config (verified: TestAdvR4_TwoDeclaredEntriesSameEndpointFlap — the
// same two entries settled at 4 in one call order and 1 in the reverse
// order). narrowConcurrencyTo takes the minimum instead of overwriting,
// which is order-independent (both orders now settle at 1) and is also
// the operationally safe choice: a single physical backend shared by two
// disagreeing declarations can only actually sustain the smaller of the
// two ceilings. Trade-off, and the reason this is a MIN and not a
// symmetric reconcile: once a queue has narrowed to a lower declared
// value it can only narrow further, never widen back up, without a
// process restart (a fresh registry) — there is no way for this function
// to tell "a second, still-currently-configured entry disagrees" apart
// from "the operator edited the same entry's value upward since the
// queue was created." Given #556's local backends already run parallel=1
// almost everywhere (the #555 backstop) this is judged the safer default;
// see narrowConcurrencyTo's own doc comment.
func newQueuedProvider(displayName, queueKey string, inner Provider, concurrency int) *queuedProvider {
	q := newBackendQueue(displayName, concurrency)
	q.key = queueKey
	actual, loaded := backendQueues.LoadOrStore(queueKey, q)
	if shared, ok := actual.(*backendQueue); ok {
		q = shared
		if loaded && concurrency >= 1 {
			q.narrowConcurrencyTo(concurrency)
		}
	}
	return &queuedProvider{Provider: inner, name: displayName, queue: q}
}

// narrowConcurrencyTo reconciles a second (or subsequent) DECLARED
// concurrency value onto an already-registered queue by taking the
// minimum of the queue's current concurrency and the newly-declared
// value n, rather than overwriting outright. See newQueuedProvider's doc
// comment (#556 repair, round 4) for why: this makes reconciliation
// order-independent when two differently-named providers.yaml entries
// declare different values for the same physical endpoint, at the cost of
// concurrency only ever being able to narrow (not widen) across separate
// declarations for a given queueKey within one process lifetime. No-op
// when n is not strictly smaller than the current value.
func (q *backendQueue) narrowConcurrencyTo(n int) {
	q.mu.Lock()
	current := q.concurrency
	q.mu.Unlock()
	if n < current {
		q.setConcurrency(n)
	}
}

// callerAttribution reads RequestMetadata.Attribution off req, falling back
// to "anonymous" — mirrors the fallback serve.go/serve_anthropic.go apply
// when populating Attribution from a bound identity.
func callerAttribution(req *CompletionRequest) string {
	if req != nil && req.Metadata.Attribution != "" {
		return req.Metadata.Attribution
	}
	return "anonymous"
}

// writeQueueHeaders sets X-Cogos-Queue-Depth / X-Cogos-Queue-Wait-Ms on w
// from obs, MUST be called before any response body byte is written (for
// streaming, before the first SSE flush). Depth/wait are omitted entirely —
// not reported as a fabricated zero — when obs never recorded anything,
// i.e. this request never passed through a queuedProvider (a cloud/remote
// provider, or the router-selection error path). When it did, zero IS
// reported (an immediately-granted slot at an idle queue is a real,
// meaningful "0" — the party adding load gets to see themselves NOT
// standing in line, which is the issue's own framing).
func writeQueueHeaders(w http.ResponseWriter, obs *queueObservation) {
	_, position, waitMs, ok := obs.snapshot()
	if !ok {
		return
	}
	h := w.Header()
	h.Set("X-Cogos-Queue-Depth", strconv.Itoa(position))
	h.Set("X-Cogos-Queue-Wait-Ms", strconv.FormatInt(waitMs, 10))
}

// attributionFor derives a RequestMetadata.Attribution value from a resolved
// BoundIdentity (serve.go), falling back to "anonymous" when unbound. Shared
// by handleChat and handleAnthropicMessages so both external HTTP entry
// points populate Attribution identically (#556).
func attributionFor(bound BoundIdentity) string {
	if bound.Bound && bound.Subject != "" {
		return bound.Subject
	}
	return "anonymous"
}

// Complete acquires this backend's queue slot, holds it for the full
// synchronous call, and releases on return.
func (p *queuedProvider) Complete(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	release, position, waitMs, err := p.queue.Acquire(ctx, callerAttribution(req))
	if err != nil {
		return nil, err
	}
	defer release()
	queueObservationFromContext(ctx).record(p.name, position, waitMs)
	return p.Provider.Complete(ctx, req)
}

// CompleteCancelSafe acquires this backend's queue slot, holds it for the
// full synchronous call, and calls the INNER provider's CompleteCancelSafe
// directly — NOT through completeCancelSafeIfSupportedRaw, and NOT through
// this wrapper's own Stream method.
//
// Why the second exclusion matters: OpenAICompatProvider.CompleteCancelSafe
// (#432) internally calls its OWN Stream to get real cancellation
// propagation. If this method were instead implemented by delegating to the
// wrapper's own Stream (p.Stream(ctx, req)), that Stream call would try to
// Acquire a SECOND slot from the same backendQueue while this call already
// holds one — backendQueue is not reentrant, so at concurrency=1 that
// self-deadlocks forever. Calling the inner provider's CompleteCancelSafe
// (or, absent that, its plain Complete) directly stays entirely inside the
// slot already held here.
//
// Why NOT completeCancelSafeIfSupportedRaw: that helper is the right choice
// for a wrapper (like countingProvider) that has no queue slot of its own to
// protect — it exists purely to avoid double-instrumenting the RFC-040 S0
// tap. Here the acquire/release bracket below is the thing that must
// surround the inner call; spelling it out directly keeps that bracket
// visible at the call site rather than hidden behind a second layer of
// delegation.
func (p *queuedProvider) CompleteCancelSafe(ctx context.Context, req *CompletionRequest) (*CompletionResponse, error) {
	release, position, waitMs, err := p.queue.Acquire(ctx, callerAttribution(req))
	if err != nil {
		return nil, err
	}
	defer release()
	queueObservationFromContext(ctx).record(p.name, position, waitMs)
	if cs, ok := p.Provider.(CancelSafeCompleter); ok {
		return cs.CompleteCancelSafe(ctx, req)
	}
	return p.Provider.Complete(ctx, req)
}

// Stream acquires this backend's queue slot BEFORE calling the inner
// provider's Stream, and holds it until the inner channel closes — NOT until
// Stream() returns, which happens immediately while generation continues in
// a goroutine. A forwarding goroutine relays every chunk from the inner
// channel to a fresh outer channel (mirrors parseOpenAISSE's goroutine shape
// in provider_openai.go) and releases the slot once the backend is actually
// free: either the inner channel closes cleanly, or — if ctx ends first —
// once a background drain (drainInnerThenRelease below) observes the inner
// channel close. The slot is deliberately NOT released the instant ctx ends;
// see drainInnerThenRelease's doc comment for why.
//
// Because Acquire blocks synchronously here, Stream itself does not return
// to the caller until a slot is actually granted — the SAME mechanism that
// serves both purposes the issue asks for: (1) it IS the interstitial queue
// for streaming traffic, and (2) a caller that disconnects WHILE WAITING
// (ctx canceled before a slot is granted) is excised from the wait list by
// Acquire's own ctx.Done() branch, so Stream returns ctx.Err() and never
// touches the inner provider at all. A caller that disconnects AFTER a slot
// was granted and generation is underway is handled by the forwarding
// goroutine's own ctx.Done() case below, which stops relaying chunks to the
// (now-abandoned) caller immediately but does not hand the slot to the next
// FIFO waiter until the backend itself is confirmed free.
func (p *queuedProvider) Stream(ctx context.Context, req *CompletionRequest) (<-chan StreamChunk, error) {
	release, position, waitMs, err := p.queue.Acquire(ctx, callerAttribution(req))
	if err != nil {
		return nil, err
	}
	queueObservationFromContext(ctx).record(p.name, position, waitMs)

	inner, err := p.Provider.Stream(ctx, req)
	if err != nil {
		release()
		return nil, err
	}

	out := make(chan StreamChunk, 32)
	go func() {
		defer close(out)
		for {
			select {
			case chunk, ok := <-inner:
				if !ok {
					// The inner provider's own goroutine is done producing —
					// the backend is genuinely free now. Release immediately.
					release()
					return
				}
				select {
				case out <- chunk:
				case <-ctx.Done():
					drainInnerThenRelease(inner, release)
					return
				}
			case <-ctx.Done():
				drainInnerThenRelease(inner, release)
				return
			}
		}
	}()
	return out, nil
}

// drainInnerThenRelease is the ctx-canceled exit path from queuedProvider's
// Stream forwarding goroutine above. It must NOT simply call release()
// immediately: release() frees this backend's queue slot for the next FIFO
// waiter, and the queue's whole invariant (in_flight never exceeds the
// backend's declared capacity) is only true if that slot is actually free —
// i.e. the inner provider (target.Backend's real HTTP call) has actually
// stopped consuming the backend, not merely that THIS caller stopped
// listening. A ctx-respecting provider (OpenAICompatProvider) does tear its
// upstream request down promptly on cancellation, but nothing here can
// assume every current or future inner Provider implementation does the
// same — the wrapper's own bookkeeping should not get ahead of reality.
//
// So: keep draining (discarding) inner in the background, off the hot
// forwarding-goroutine path so the disconnecting caller isn't blocked on it,
// and release only once inner actually closes — the same signal the normal
// (!ok) exit path above already treats as "the backend is free."
func drainInnerThenRelease(inner <-chan StreamChunk, release func()) {
	go func() {
		defer release()
		for range inner {
		}
	}()
}

// ListModels forwards to the inner provider when it implements ModelLister.
//
// This override exists to close a real, easy-to-miss regression: Go's
// embedded-interface method promotion only promotes methods declared on the
// EMBEDDED INTERFACE TYPE (Provider), not extra methods the underlying
// concrete type (e.g. *OpenAICompatProvider) happens to also implement.
// ModelLister is a separate interface, asserted via p.(ModelLister) in
// serve_compat.go's liveModelEntries. Without this explicit forwarding
// method, wrapping a local backend in queuedProvider would silently drop it
// out of the /v1/models menu the moment #556 shipped.
func (p *queuedProvider) ListModels(ctx context.Context) ([]string, error) {
	if ml, ok := p.Provider.(ModelLister); ok {
		return ml.ListModels(ctx)
	}
	return nil, fmt.Errorf("queuedProvider(%s): inner provider does not implement ModelLister", p.name)
}

// ListModelsWithContext forwards to the inner provider when it implements
// ModelContextLister. See ListModels's doc comment — same embedded-interface
// promotion gap, this time guarding the #518 context-length enrichment path
// specifically (LM Studio's per-model context window).
func (p *queuedProvider) ListModelsWithContext(ctx context.Context) ([]ModelListing, error) {
	if mcl, ok := p.Provider.(ModelContextLister); ok {
		return mcl.ListModelsWithContext(ctx)
	}
	return nil, fmt.Errorf("queuedProvider(%s): inner provider does not implement ModelContextLister", p.name)
}
