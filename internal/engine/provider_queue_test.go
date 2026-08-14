// provider_queue_test.go — tests for the #556 kernel-owned per-backend FIFO
// queue: backendQueue's admission/cancellation semantics, and queuedProvider's
// three gated Provider methods plus its ModelLister/ModelContextLister
// forwarding.
package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

// ── test fixtures ────────────────────────────────────────────────────────────

// fakeQueueProvider is a minimal Provider used only to exercise
// queuedProvider's acquire/release brackets. Each gated method records its
// own call count so tests can assert exactly which inner method fired (in
// particular, that CompleteCancelSafe never triggers a second Stream call —
// the #432 bypass footgun) and Stream can be configured to trickle chunks out
// over time so tests can observe that a queue slot is held until the
// channel actually closes, not until Stream() returns.
type fakeQueueProvider struct {
	name string

	mu              sync.Mutex
	completeCalls   int
	cancelSafeCalls int
	streamCalls     int

	streamChunks []string
	streamDelay  time.Duration

	models        []string
	modelListings []ModelListing
}

func (p *fakeQueueProvider) Name() string                     { return p.name }
func (p *fakeQueueProvider) Model() string                    { return "fake" }
func (p *fakeQueueProvider) Available(_ context.Context) bool { return true }
func (p *fakeQueueProvider) Capabilities() ProviderCapabilities {
	return ProviderCapabilities{IsLocal: true}
}
func (p *fakeQueueProvider) Ping(_ context.Context) (time.Duration, error) { return 0, nil }

func (p *fakeQueueProvider) Complete(_ context.Context, _ *CompletionRequest) (*CompletionResponse, error) {
	p.mu.Lock()
	p.completeCalls++
	p.mu.Unlock()
	return &CompletionResponse{Content: "complete"}, nil
}

func (p *fakeQueueProvider) CompleteCancelSafe(_ context.Context, _ *CompletionRequest) (*CompletionResponse, error) {
	p.mu.Lock()
	p.cancelSafeCalls++
	p.mu.Unlock()
	return &CompletionResponse{Content: "cancel-safe"}, nil
}

func (p *fakeQueueProvider) Stream(ctx context.Context, _ *CompletionRequest) (<-chan StreamChunk, error) {
	p.mu.Lock()
	p.streamCalls++
	p.mu.Unlock()

	ch := make(chan StreamChunk)
	go func() {
		defer close(ch)
		for _, c := range p.streamChunks {
			if p.streamDelay > 0 {
				select {
				case <-time.After(p.streamDelay):
				case <-ctx.Done():
					return
				}
			}
			select {
			case ch <- StreamChunk{Delta: c}:
			case <-ctx.Done():
				return
			}
		}
		select {
		case ch <- StreamChunk{Done: true}:
		case <-ctx.Done():
		}
	}()
	return ch, nil
}

func (p *fakeQueueProvider) ListModels(_ context.Context) ([]string, error) {
	return p.models, nil
}

func (p *fakeQueueProvider) ListModelsWithContext(_ context.Context) ([]ModelListing, error) {
	return p.modelListings, nil
}

func (p *fakeQueueProvider) calls() (complete, cancelSafe, stream int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.completeCalls, p.cancelSafeCalls, p.streamCalls
}

// pollUntilQueue polls cond every millisecond until it's true or timeout
// elapses, failing the test on timeout. Queue admission state changes
// happen inside goroutines this test spawns, so assertions on it need to
// poll rather than assume a fixed sleep is long enough.
func pollUntilQueue(t *testing.T, timeout time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", msg)
		}
		time.Sleep(time.Millisecond)
	}
}

// ── backendQueue: FIFO ordering ───────────────────────────────────────────────

func TestBackendQueueFIFOOrdering(t *testing.T) {
	t.Parallel()
	q := newBackendQueue(t.Name(), 1)
	ctx := context.Background()

	// Hold the only slot so every subsequent Acquire below queues rather
	// than running immediately.
	release0, pos0, wait0, err := q.Acquire(ctx, "seed")
	if err != nil {
		t.Fatalf("seed acquire: %v", err)
	}
	if pos0 != 0 || wait0 != 0 {
		t.Fatalf("seed acquire should be immediate: pos=%d wait=%d", pos0, wait0)
	}

	const n = 5
	var mu sync.Mutex
	var order []int
	done := make(chan struct{}, n)

	for i := 0; i < n; i++ {
		go func(i int) {
			release, _, _, err := q.Acquire(ctx, fmt.Sprintf("caller-%d", i))
			if err != nil {
				t.Errorf("caller %d acquire: %v", i, err)
				done <- struct{}{}
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			release()
			done <- struct{}{}
		}(i)
		// Wait for THIS goroutine's ticket to actually be enqueued before
		// launching the next one, so PushBack order matches loop order —
		// otherwise two Acquire calls could race for who enqueues first
		// and the test wouldn't be asserting FIFO order deterministically.
		pollUntilQueue(t, 2*time.Second, fmt.Sprintf("caller %d to enqueue", i), func() bool {
			return q.Snapshot().Waiting == i+1
		})
	}

	release0()

	for i := 0; i < n; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for caller %d to finish", i)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for i, got := range order {
		if got != i {
			t.Fatalf("FIFO order violated: got order %v, want strictly 0..%d", order, n-1)
		}
	}
}

// ── backendQueue: concurrency limiting ────────────────────────────────────────

func TestBackendQueueConcurrencyLimit(t *testing.T) {
	t.Parallel()
	q := newBackendQueue(t.Name(), 2)
	ctx := context.Background()

	var mu sync.Mutex
	var pending []func() // granted release funcs not yet consumed by the test
	for i := 0; i < 5; i++ {
		go func(i int) {
			release, _, _, err := q.Acquire(ctx, fmt.Sprintf("c-%d", i))
			if err != nil {
				t.Errorf("acquire %d: %v", i, err)
				return
			}
			mu.Lock()
			pending = append(pending, release)
			mu.Unlock()
		}(i)
	}

	// takeOne pops and returns one granted-but-unconsumed release func,
	// polling until at least `want` are available.
	takeOne := func(want int) func() {
		pollUntilQueue(t, 2*time.Second, fmt.Sprintf("at least %d acquisitions granted", want), func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(pending) >= want
		})
		mu.Lock()
		defer mu.Unlock()
		r := pending[0]
		pending = pending[1:]
		return r
	}

	pollUntilQueue(t, 2*time.Second, "exactly 2 granted, 3 waiting, no more no less", func() bool {
		snap := q.Snapshot()
		mu.Lock()
		granted := len(pending)
		mu.Unlock()
		return snap.InFlight == 2 && snap.Waiting == 3 && granted == 2
	})

	// Release one — exactly one more of the three waiters should be
	// admitted, never more than concurrency allows.
	takeOne(1)()

	pollUntilQueue(t, 2*time.Second, "still inFlight=2 after one release, waiting drops to 2", func() bool {
		snap := q.Snapshot()
		return snap.InFlight == 2 && snap.Waiting == 2
	})
	mu.Lock()
	granted := len(pending)
	mu.Unlock()
	if granted != 1 {
		t.Fatalf("expected exactly 1 newly-granted (unconsumed) acquisition after the first release, got %d", granted)
	}

	// Drain the remaining four (1 currently held + 3 that cascade in as
	// each release admits the next waiter) so no goroutine leaks past the
	// test.
	for i := 0; i < 4; i++ {
		takeOne(1)()
	}

	pollUntilQueue(t, 2*time.Second, "queue fully drained", func() bool {
		snap := q.Snapshot()
		return snap.InFlight == 0 && snap.Waiting == 0
	})
}

// ── backendQueue: cancel while waiting ────────────────────────────────────────

func TestBackendQueueCancelWhileWaiting(t *testing.T) {
	t.Parallel()
	q := newBackendQueue(t.Name(), 1)

	release0, _, _, err := q.Acquire(context.Background(), "seed")
	if err != nil {
		t.Fatalf("seed acquire: %v", err)
	}

	ctx1, cancel1 := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, _, _, err := q.Acquire(ctx1, "waiter-1")
		waiterDone <- err
	}()
	pollUntilQueue(t, 2*time.Second, "waiter-1 to enqueue", func() bool {
		return q.Snapshot().Waiting == 1
	})

	cancel1()

	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not return promptly after ctx cancel")
	}

	// The canceled waiter must be excised from the wait list — not merely
	// abandoned in place — so the next real waiter isn't stacked behind a
	// dead ticket.
	pollUntilQueue(t, 2*time.Second, "canceled waiter removed from the wait list", func() bool {
		return q.Snapshot().Waiting == 0
	})

	// A second, real waiter enqueued now must land at position 1 — the
	// freed position — not position 2, proving the removal actually
	// happened rather than merely being ignored on the eventual Release.
	relCh := make(chan func(), 1)
	go func() {
		release, _, _, err := q.Acquire(context.Background(), "waiter-2")
		if err != nil {
			t.Errorf("waiter-2 acquire: %v", err)
			return
		}
		relCh <- release
	}()
	pollUntilQueue(t, 2*time.Second, "waiter-2 to enqueue", func() bool {
		return q.Snapshot().Waiting == 1
	})

	snap := q.Snapshot()
	if len(snap.Callers) != 1 || snap.Callers[0].Position != 1 {
		t.Fatalf("expected waiter-2 at position 1 (the freed slot), got callers=%+v", snap.Callers)
	}

	release0()
	select {
	case release := <-relCh:
		release()
	case <-time.After(2 * time.Second):
		t.Fatal("waiter-2 was never granted after release0")
	}
}

// ── queuedProvider: Stream holds its slot until the channel actually closes ──

func TestQueuedProviderStreamHoldsSlotUntilChannelClose(t *testing.T) {
	t.Parallel()
	inner := &fakeQueueProvider{
		name:         "fake-stream-hold",
		streamChunks: []string{"a", "b", "c"},
		streamDelay:  20 * time.Millisecond,
	}
	qp := newQueuedProvider(t.Name(), t.Name(), inner, 1)
	t.Cleanup(resetBackendQueuesForTest)

	ch, err := qp.Stream(context.Background(), &CompletionRequest{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Stream() has already returned above — a second Acquire on the SAME
	// queue must still block, because generation is continuing in the
	// forwarding goroutine and the slot isn't released until that channel
	// closes.
	acquired := make(chan struct{})
	go func() {
		release, _, _, err := qp.queue.Acquire(context.Background(), "second")
		if err != nil {
			t.Errorf("second acquire: %v", err)
			return
		}
		close(acquired)
		release()
	}()

	select {
	case <-acquired:
		t.Fatal("second Acquire granted before the first Stream's channel closed")
	case <-time.After(30 * time.Millisecond):
		// expected: still blocked while chunks are in flight
	}

	for range ch {
		// drain fully
	}

	select {
	case <-acquired:
		// slot released once the channel closed — correct
	case <-time.After(2 * time.Second):
		t.Fatal("second Acquire never granted after the first stream's channel closed")
	}
}

// ── queuedProvider: Stream releases its slot on ctx cancel mid-generation ────

func TestQueuedProviderStreamReleasesSlotOnCtxCancelMidGeneration(t *testing.T) {
	t.Parallel()
	inner := &fakeQueueProvider{
		name:         "fake-stream-cancel",
		streamChunks: []string{"a", "b", "c", "d", "e"},
		streamDelay:  50 * time.Millisecond,
	}
	qp := newQueuedProvider(t.Name(), t.Name(), inner, 1)
	t.Cleanup(resetBackendQueuesForTest)

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := qp.Stream(ctx, &CompletionRequest{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	<-ch // consume one chunk, then cancel mid-generation
	cancel()

	for range ch {
		// drain the terminating channel so the forwarding goroutine exits
	}

	done := make(chan struct{})
	go func() {
		release, _, _, err := qp.queue.Acquire(context.Background(), "after-cancel")
		if err != nil {
			t.Errorf("acquire after cancel: %v", err)
			return
		}
		release()
		close(done)
	}()

	select {
	case <-done:
		// no leaked slot
	case <-time.After(2 * time.Second):
		t.Fatal("slot leaked: Acquire after mid-generation cancel never succeeded")
	}
}

// ── queuedProvider: ModelLister / ModelContextLister forwarding ─────────────

func TestQueuedProviderForwardsModelListerAndModelContextLister(t *testing.T) {
	t.Parallel()
	inner := &fakeQueueProvider{
		name:   "fake-models",
		models: []string{"model-a", "model-b"},
		modelListings: []ModelListing{
			{ID: "model-a", ContextLength: 8192},
			{ID: "model-b", ContextLength: 4096},
		},
	}
	qp := newQueuedProvider(t.Name(), t.Name(), inner, 1)
	t.Cleanup(resetBackendQueuesForTest)

	var provider Provider = qp

	ml, ok := provider.(ModelLister)
	if !ok {
		t.Fatal("queuedProvider does not satisfy ModelLister despite wrapping a provider that does — this is the #518 regression the plan calls out")
	}
	got, err := ml.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if !reflect.DeepEqual(got, inner.models) {
		t.Fatalf("ListModels = %v, want %v (verbatim from inner)", got, inner.models)
	}

	mcl, ok := provider.(ModelContextLister)
	if !ok {
		t.Fatal("queuedProvider does not satisfy ModelContextLister despite wrapping a provider that does")
	}
	gotCtx, err := mcl.ListModelsWithContext(context.Background())
	if err != nil {
		t.Fatalf("ListModelsWithContext: %v", err)
	}
	if !reflect.DeepEqual(gotCtx, inner.modelListings) {
		t.Fatalf("ListModelsWithContext = %v, want %v (verbatim from inner)", gotCtx, inner.modelListings)
	}
}

// ── queuedProvider: CompleteCancelSafe delegates to the inner method directly ─

func TestQueuedProviderCompleteCancelSafeDelegatesToInnerDirectly(t *testing.T) {
	t.Parallel()
	inner := &fakeQueueProvider{name: "fake-cancel-safe"}
	qp := newQueuedProvider(t.Name(), t.Name(), inner, 1)
	t.Cleanup(resetBackendQueuesForTest)

	_, err := qp.CompleteCancelSafe(context.Background(), &CompletionRequest{})
	if err != nil {
		t.Fatalf("CompleteCancelSafe: %v", err)
	}

	complete, cancelSafe, stream := inner.calls()
	if cancelSafe != 1 {
		t.Fatalf("expected inner.CompleteCancelSafe called exactly once, got %d", cancelSafe)
	}
	if complete != 0 {
		t.Fatalf("expected inner.Complete NOT called, got %d calls", complete)
	}
	if stream != 0 {
		t.Fatalf("expected inner.Stream NOT called (the #432 bypass footgun this wrapper must avoid), got %d calls", stream)
	}
}
