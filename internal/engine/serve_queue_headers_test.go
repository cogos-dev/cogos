// serve_queue_headers_test.go — HTTP-level tests for #556's
// X-Cogos-Queue-Depth / X-Cogos-Queue-Wait-Ms response headers on
// POST /v1/chat/completions, covering both the non-streaming and streaming
// paths, plus the disconnected-caller-while-waiting dequeue behavior and a
// direct check that queuedProvider.Stream does not buffer/delay chunk
// delivery relative to an unwrapped provider.
package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestHandleChat_QueueHeadersPresentOnIdleQueue exercises the non-streaming
// path: a request against a queuedProvider-wrapped backend with nothing else
// in flight must come back with X-Cogos-Queue-Depth: 0 and
// X-Cogos-Queue-Wait-Ms: 0 — an immediately-granted slot at an idle queue is
// a real, meaningful "0" per writeQueueHeaders' documented contract, not an
// omitted header.
func TestHandleChat_QueueHeadersPresentOnIdleQueue(t *testing.T) {
	t.Parallel()
	resetBackendQueuesForTest()
	t.Cleanup(resetBackendQueuesForTest)

	srv := newTestServer(t)
	qp := newQueuedProvider("stub", NewStubProvider("stub", "hello world"), 1)
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(qp)
	srv.SetRouter(router)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"local","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Cogos-Queue-Depth"); got != "0" {
		t.Errorf("X-Cogos-Queue-Depth = %q, want 0", got)
	}
	if got := w.Header().Get("X-Cogos-Queue-Wait-Ms"); got != "0" {
		t.Errorf("X-Cogos-Queue-Wait-Ms = %q, want 0", got)
	}
}

// TestHandleChat_QueueHeadersReflectActualWait puts a request through with
// the backend's only slot already held, so it must wait, then verifies the
// eventual response headers report a nonzero depth/wait matching that real
// admission — not a fabricated or omitted value.
func TestHandleChat_QueueHeadersReflectActualWait(t *testing.T) {
	t.Parallel()
	resetBackendQueuesForTest()
	t.Cleanup(resetBackendQueuesForTest)

	srv := newTestServer(t)
	qp := newQueuedProvider("stub", NewStubProvider("stub", "hello world"), 1)
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(qp)
	srv.SetRouter(router)

	// Hold the only slot from outside the HTTP path, releasing only once
	// the HTTP request below has actually enqueued behind it — polling
	// rather than a fixed sleep, so this isn't sensitive to how long
	// newTestServer/handleChat setup takes under -race or a loaded CI box.
	release, _, _, err := qp.queue.Acquire(context.Background(), "occupant")
	if err != nil {
		t.Fatalf("occupant acquire: %v", err)
	}

	const minObservableWait = 15 * time.Millisecond
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for qp.queue.Snapshot().Waiting < 1 {
			if time.Now().After(deadline) {
				return
			}
			time.Sleep(time.Millisecond)
		}
		// The request is now queued behind us — hold a little longer so
		// its recorded wait time is unambiguously nonzero, then let it in.
		time.Sleep(minObservableWait)
		release()
	}()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"local","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200: %s", w.Code, w.Body.String())
	}
	depth, err := strconv.Atoi(w.Header().Get("X-Cogos-Queue-Depth"))
	if err != nil {
		t.Fatalf("X-Cogos-Queue-Depth not an int: %v", err)
	}
	if depth != 1 {
		t.Errorf("X-Cogos-Queue-Depth = %d, want 1 (this request was 1st in line behind the held slot)", depth)
	}
	waitMs, err := strconv.ParseInt(w.Header().Get("X-Cogos-Queue-Wait-Ms"), 10, 64)
	if err != nil {
		t.Fatalf("X-Cogos-Queue-Wait-Ms not an int: %v", err)
	}
	if waitMs <= 0 {
		t.Errorf("X-Cogos-Queue-Wait-Ms = %d, want > 0 (this request actually waited ~%s)", waitMs, minObservableWait)
	}
}

// TestHandleChat_QueueHeadersOmittedForUnwrappedProvider verifies the other
// half of writeQueueHeaders' contract: a provider that never passed through
// a queuedProvider (e.g. a cloud provider in production) must not have these
// headers reported at all — not a fabricated zero.
func TestHandleChat_QueueHeadersOmittedForUnwrappedProvider(t *testing.T) {
	t.Parallel()
	resetBackendQueuesForTest()
	t.Cleanup(resetBackendQueuesForTest)

	srv := newTestServer(t)
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(NewStubProvider("stub", "hello world")) // NOT wrapped
	srv.SetRouter(router)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"local","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200: %s", w.Code, w.Body.String())
	}
	if _, ok := w.Header()["X-Cogos-Queue-Depth"]; ok {
		t.Error("expected X-Cogos-Queue-Depth absent for a provider never wrapped in queuedProvider")
	}
	if _, ok := w.Header()["X-Cogos-Queue-Wait-Ms"]; ok {
		t.Error("expected X-Cogos-Queue-Wait-Ms absent for a provider never wrapped in queuedProvider")
	}
}

// TestHandleChat_StreamingQueueHeadersSetBeforeFirstByte covers the
// streaming path specifically: headers can't follow first-byte on an SSE
// response, so this asserts both that the queue headers are present/correct
// AND that the streamed content itself arrived intact — proving headers were
// set at the right point in streamChat, not merely that the plumbing
// compiles.
func TestHandleChat_StreamingQueueHeadersSetBeforeFirstByte(t *testing.T) {
	t.Parallel()
	resetBackendQueuesForTest()
	t.Cleanup(resetBackendQueuesForTest)

	srv := newTestServer(t)
	stub := NewStubProvider("stub", "")
	stub.chunks = []string{"hel", "lo", " world"}
	qp := newQueuedProvider("stub", stub, 1)
	router := NewSimpleRouter(RoutingConfig{Default: "stub"})
	router.RegisterProvider(qp)
	srv.SetRouter(router)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"local","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.handleChat(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200: %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Cogos-Queue-Depth"); got != "0" {
		t.Errorf("X-Cogos-Queue-Depth = %q, want 0", got)
	}
	if got := w.Header().Get("X-Cogos-Queue-Wait-Ms"); got != "0" {
		t.Errorf("X-Cogos-Queue-Wait-Ms = %q, want 0", got)
	}

	var assembled strings.Builder
	scanner := bufio.NewScanner(w.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			break
		}
		var chunk oaiChatResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("decode chunk: %v", err)
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta != nil {
			assembled.WriteString(extractContent(chunk.Choices[0].Delta.Content))
		}
	}
	if assembled.String() != "hello world" {
		t.Errorf("assembled = %q; want hello world (queue wrapping must not corrupt streamed content)", assembled.String())
	}
}

// TestQueuedProviderStream_PassthroughNotBuffered is the direct, provider-level
// version of "the queue wrapper doesn't buffer streaming": chunks sent by the
// inner provider must arrive on the outer channel promptly, one at a time,
// rather than only after the inner channel fully closes — i.e. the wrapper's
// forwarding goroutine relays as it goes, it doesn't accumulate.
func TestQueuedProviderStream_PassthroughNotBuffered(t *testing.T) {
	t.Parallel()
	inner := &fakeQueueProvider{
		name:         "fake-passthrough",
		streamChunks: []string{"a", "b", "c"},
		streamDelay:  30 * time.Millisecond,
	}
	qp := newQueuedProvider(t.Name(), inner, 1)
	t.Cleanup(resetBackendQueuesForTest)

	ch, err := qp.Stream(context.Background(), &CompletionRequest{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	start := time.Now()
	var gotFirstAt time.Duration
	n := 0
	for chunk := range ch {
		if chunk.Delta != "" && n == 0 {
			gotFirstAt = time.Since(start)
		}
		if chunk.Delta != "" {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("expected 3 delta chunks forwarded, got %d", n)
	}
	// The first chunk should arrive shortly after ~1 streamDelay, not after
	// all 3 delays have elapsed (~90ms) — that would indicate the wrapper
	// buffered the whole generation before relaying anything.
	if gotFirstAt > 70*time.Millisecond {
		t.Errorf("first chunk arrived after %s — looks buffered, not passed through as generated (streamDelay=30ms)", gotFirstAt)
	}
}

// TestBackendQueue_DisconnectedCallerWhileWaitingIsDequeued is the HTTP-shaped
// version of the FIFO-dequeue guarantee: a caller whose context is canceled
// (e.g. an HTTP client disconnecting) while still WAITING — never granted a
// slot — must be removed from the queue so it doesn't block subsequent real
// callers, mirroring what net/http does to r.Context() on a dropped
// connection.
func TestBackendQueue_DisconnectedCallerWhileWaitingIsDequeued(t *testing.T) {
	t.Parallel()
	q := newBackendQueue(t.Name(), 1)

	release0, _, _, err := q.Acquire(context.Background(), "seed")
	if err != nil {
		t.Fatalf("seed acquire: %v", err)
	}
	t.Cleanup(release0)

	// Simulate an HTTP request whose client disconnects while queued.
	reqCtx, cancelReq := context.WithCancel(context.Background())
	disconnected := make(chan error, 1)
	go func() {
		_, _, _, err := q.Acquire(reqCtx, "disconnecting-client")
		disconnected <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for q.Snapshot().Waiting != 1 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the disconnecting client to enqueue")
		}
		time.Sleep(time.Millisecond)
	}

	cancelReq() // client drops connection

	select {
	case err := <-disconnected:
		if err == nil {
			t.Fatal("expected the disconnected caller's Acquire to return an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("disconnected caller's Acquire never returned")
	}

	deadline = time.Now().Add(2 * time.Second)
	for q.Snapshot().Waiting != 0 {
		if time.Now().After(deadline) {
			t.Fatal("disconnected caller was never dequeued — a real, non-departing waiter would be starved behind it")
		}
		time.Sleep(time.Millisecond)
	}

	// A genuine subsequent caller must be served promptly, proving the
	// dequeue actually freed the wait list rather than leaving a phantom
	// entry that Release still has to walk past.
	realCallerDone := make(chan struct{})
	go func() {
		release, _, _, err := q.Acquire(context.Background(), "real-caller")
		if err != nil {
			t.Errorf("real caller acquire: %v", err)
			return
		}
		release()
		close(realCallerDone)
	}()
	deadline = time.Now().Add(2 * time.Second)
	for q.Snapshot().Waiting != 1 {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the real caller to enqueue")
		}
		time.Sleep(time.Millisecond)
	}
	release0()
	select {
	case <-realCallerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("real caller never granted after release0")
	}
}
