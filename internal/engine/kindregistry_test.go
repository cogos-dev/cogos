package engine

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRegisterKindHandlerAndDispatch(t *testing.T) {
	resetKindRegistry()
	t.Cleanup(resetKindRegistry)

	called := false
	RegisterKindHandler(BlockMessage, func(block *CogBlock) error {
		called = true
		return nil
	})

	block := &CogBlock{Kind: BlockMessage}
	if err := DispatchKind(block); err != nil {
		t.Fatalf("DispatchKind returned unexpected error: %v", err)
	}
	if !called {
		t.Fatal("handler was not called")
	}
}

func TestRegisterKindHandlerDuplicatePanics(t *testing.T) {
	resetKindRegistry()
	t.Cleanup(resetKindRegistry)

	noop := func(block *CogBlock) error { return nil }
	RegisterKindHandler(BlockToolCall, noop)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on duplicate registration, got none")
		}
	}()
	RegisterKindHandler(BlockToolCall, noop)
}

func TestDispatchKindUnregisteredReturnsErrNoKindHandler(t *testing.T) {
	resetKindRegistry()
	t.Cleanup(resetKindRegistry)

	block := &CogBlock{Kind: BlockImport}
	err := DispatchKind(block)
	if err == nil {
		t.Fatal("expected error for unregistered Kind, got nil")
	}
	if !errors.Is(err, ErrNoKindHandler) {
		t.Fatalf("expected ErrNoKindHandler, got %v", err)
	}
}

func TestDispatchKindNilBlock(t *testing.T) {
	resetKindRegistry()
	t.Cleanup(resetKindRegistry)

	err := DispatchKind(nil)
	if err == nil {
		t.Fatal("expected error for nil block, got nil")
	}
}

func TestRegisteredKindsReturnsSorted(t *testing.T) {
	resetKindRegistry()
	t.Cleanup(resetKindRegistry)

	noop := func(block *CogBlock) error { return nil }
	RegisterKindHandler(BlockToolResult, noop)
	RegisterKindHandler(BlockMessage, noop)
	RegisterKindHandler(BlockAttention, noop)

	got := RegisteredKinds()
	if len(got) != 3 {
		t.Fatalf("expected 3 registered kinds, got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Errorf("RegisteredKinds() not sorted: %v", got)
		}
	}
}

func TestDispatchKindHandlerErrorPropagates(t *testing.T) {
	resetKindRegistry()
	t.Cleanup(resetKindRegistry)

	sentinel := errors.New("handler error")
	RegisterKindHandler(BlockSystemEvent, func(block *CogBlock) error {
		return sentinel
	})

	block := &CogBlock{Kind: BlockSystemEvent}
	err := DispatchKind(block)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}

func TestDispatchKindConcurrent(t *testing.T) {
	resetKindRegistry()
	t.Cleanup(resetKindRegistry)

	kinds := []CogBlockKind{
		BlockMessage,
		BlockToolCall,
		BlockToolResult,
		BlockImport,
		BlockAttention,
	}
	for _, k := range kinds {
		k := k
		RegisterKindHandler(k, func(block *CogBlock) error { return nil })
	}

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)

	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			block := &CogBlock{Kind: kinds[i%len(kinds)]}
			if err := DispatchKind(block); err != nil {
				errs <- err
			}
		}(i)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent dispatch timed out")
	}

	close(errs)
	for err := range errs {
		t.Errorf("concurrent dispatch error: %v", err)
	}
}
