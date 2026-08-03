// failure_backoff.go — instance-scoped consecutive-failure backoff with jitter.
//
// RFC-041 (Anchors) census §T8 found FOUR independent hand-rolled exponential
// backoff implementations in this codebase, none of them using jitter:
//
//  1. autonomic_ticker.go   healBackoff*     — self-heal skip-tick window
//  2. bep_engine.go         runDialer        — peer reconnect
//  3. pkg/modality/supervisor.go healthLoop  — subprocess restart
//  4. harness/harness.go                     — inference-call retry
//
// This type is NOT a fifth. It is (1) extracted into an instance-scoped struct
// so a second scheduler can hold its own, plus the jitter all four lack.
// autonomic_ticker.go is migrated onto it in the same change, so the census
// count stays at four (three hand-rolled + one shared) rather than growing.
//
// Scope note: this is deliberately a leaf helper in internal/engine, NOT
// RFC-041's designed pkg/substrate/anchor.Backoff. Building the Anchor
// registry and run loop is out of scope here. When that lands, this type is
// the natural first migration: maxSkip and jitter map 1:1 onto RFC-041's
// Backoff{Base, Max, Mult, Jitter} leaf, and the two callers move together.
//
// Model: callers own a monotonic tick counter (the scheduler's own loop
// iteration). A key that fails on tick T is not Ready again until
// T + skip(n), where skip doubles per consecutive failure and is capped at
// maxSkip. A success resets the key to immediately-ready.
package engine

import (
	"math"
	"math/rand/v2"
	"sync"
)

// failureBackoff tracks consecutive failures per key and the scheduler tick at
// which each key may next be attempted. Thread-safe; all state is
// instance-scoped so two schedulers ticking at different intervals cannot
// contaminate each other's failure counts or tick arithmetic (the specific
// reason autonomic_ticker.go's package-global maps could not simply be shared
// with ReconcileDaemon: same provider names, different tick clocks).
type failureBackoff struct {
	mu      sync.Mutex
	fails   map[string]int
	retryAt map[string]int
	maxSkip int
	jitter  float64
	rnd     *rand.Rand
}

// newFailureBackoff returns a backoff capped at maxSkip ticks. jitter is the
// fraction of the nominal skip randomized away in each direction (0.25 = the
// window lands uniformly in [0.75x, 1.25x]); pass 0 to disable.
func newFailureBackoff(maxSkip int, jitter float64) *failureBackoff {
	return newFailureBackoffSeeded(maxSkip, jitter, rand.Uint64(), rand.Uint64())
}

// newFailureBackoffSeeded is newFailureBackoff with an explicit PRNG seed, so
// jitter is reproducible under test.
func newFailureBackoffSeeded(maxSkip int, jitter float64, s1, s2 uint64) *failureBackoff {
	if maxSkip < 1 {
		maxSkip = 1
	}
	if jitter < 0 {
		jitter = 0
	}
	return &failureBackoff{
		fails:   make(map[string]int),
		retryAt: make(map[string]int),
		maxSkip: maxSkip,
		jitter:  jitter,
		rnd:     rand.New(rand.NewPCG(s1, s2)),
	}
}

// nominalSkip is the un-jittered skip window for the nth consecutive failure:
// 2^(n-1), capped at maxSkip. Preserves autonomic_ticker.go's original curve
// exactly so migrating that caller is behaviour-identical modulo jitter.
func (b *failureBackoff) nominalSkip(n int) int {
	skip := 1
	for i := 1; i < n && skip < b.maxSkip; i++ {
		skip *= 2
	}
	if skip > b.maxSkip {
		skip = b.maxSkip
	}
	return skip
}

// applyJitter spreads skip within +/- jitter of nominal. Caller holds b.mu
// (b.rnd is not thread-safe).
//
// Why jitter at all, when none of the four existing sites use it: without it,
// every provider that begins failing on the same tick — the normal shape of a
// shared upstream outage, e.g. LM Studio going down and taking every
// lms-model-state backend with it — retries in permanent lockstep, so the
// recovery attempt arrives as a thundering herd on exactly the resource that
// just failed.
func (b *failureBackoff) applyJitter(skip int) int {
	if b.jitter <= 0 || skip <= 1 {
		return skip
	}
	delta := b.jitter * float64(skip)
	lo := int(math.Round(float64(skip) - delta))
	hi := int(math.Round(float64(skip) + delta))
	if lo < 1 {
		lo = 1
	}
	if hi > b.maxSkip {
		hi = b.maxSkip
	}
	if hi <= lo {
		return lo
	}
	return lo + b.rnd.IntN(hi-lo+1)
}

// Ready reports whether key may be attempted on this tick. Unknown keys and
// keys with no outstanding skip window are always ready.
func (b *failureBackoff) Ready(key string, tick int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	retryAt, ok := b.retryAt[key]
	return !ok || tick >= retryAt
}

// RecordFailure registers a consecutive failure for key and arms the next skip
// window. Returns the new consecutive-failure count and the skip window
// actually applied (post-jitter).
func (b *failureBackoff) RecordFailure(key string, tick int) (fails, skip int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails[key]++
	n := b.fails[key]
	skip = b.applyJitter(b.nominalSkip(n))
	b.retryAt[key] = tick + skip
	return n, skip
}

// Hold re-arms key's skip window at maximum depth WITHOUT counting a failure.
// Used for a key that is deliberately not being actuated (quarantined): the
// caller still wants to observe it periodically, but at the widened cadence
// rather than every tick. Returns the applied skip window.
func (b *failureBackoff) Hold(key string, tick int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	skip := b.applyJitter(b.maxSkip)
	b.retryAt[key] = tick + skip
	return skip
}

// RecordSuccess clears key's failure streak and skip window. Returns the
// consecutive-failure count that was cleared and whether the key was in fact
// failing (so callers can log recovery exactly once).
func (b *failureBackoff) RecordSuccess(key string) (wasFailing int, recovered bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.fails[key]
	delete(b.fails, key)
	delete(b.retryAt, key)
	return n, n > 0
}

// Failures returns key's current consecutive-failure count.
func (b *failureBackoff) Failures(key string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.fails[key]
}

// Forget drops all state for key — called when a key leaves the registry, so
// the maps do not grow without bound across a long-lived process.
func (b *failureBackoff) Forget(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.fails, key)
	delete(b.retryAt, key)
}
