package kindregistry_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/cogblock"
	"github.com/myrgic/cogos/pkg/cogblock/kindregistry"
)

func TestRegisterAndDispatch(t *testing.T) {
	kindregistry.Reset()

	called := false
	kindregistry.Register(cogblock.BlockMessage, func(block *cogblock.CogBlock) error {
		called = true
		return nil
	})

	block := &cogblock.CogBlock{Kind: cogblock.BlockMessage}
	if err := kindregistry.Dispatch(block); err != nil {
		t.Fatalf("Dispatch returned unexpected error: %v", err)
	}
	if !called {
		t.Fatal("handler was not called")
	}
}

func TestDuplicateRegistrationPanics(t *testing.T) {
	kindregistry.Reset()

	noop := func(block *cogblock.CogBlock) error { return nil }
	kindregistry.Register(cogblock.BlockToolCall, noop)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on duplicate registration, got none")
		}
	}()
	kindregistry.Register(cogblock.BlockToolCall, noop)
}

func TestDispatchUnregisteredKindReturnsErrNoHandler(t *testing.T) {
	kindregistry.Reset()

	block := &cogblock.CogBlock{Kind: cogblock.BlockImport}
	err := kindregistry.Dispatch(block)
	if err == nil {
		t.Fatal("expected error for unregistered Kind, got nil")
	}
	if !errors.Is(err, kindregistry.ErrNoHandler) {
		t.Fatalf("expected ErrNoHandler, got %v", err)
	}
}

func TestDispatchNilBlock(t *testing.T) {
	kindregistry.Reset()

	err := kindregistry.Dispatch(nil)
	if err == nil {
		t.Fatal("expected error for nil block, got nil")
	}
}

func TestRegisteredReturnsSortedKinds(t *testing.T) {
	kindregistry.Reset()

	noop := func(block *cogblock.CogBlock) error { return nil }
	kindregistry.Register(cogblock.BlockToolResult, noop)
	kindregistry.Register(cogblock.BlockMessage, noop)
	kindregistry.Register(cogblock.BlockAttention, noop)

	got := kindregistry.Registered()
	if len(got) != 3 {
		t.Fatalf("expected 3 registered kinds, got %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Errorf("Registered() not sorted: %v", got)
		}
	}
}

func TestConcurrentRegisterAndDispatch(t *testing.T) {
	kindregistry.Reset()

	// Register a set of kinds sequentially first (to avoid triggering the
	// duplicate-panic during concurrent registration, which is correct behavior
	// but not what this test exercises).
	kinds := []cogblock.CogBlockKind{
		cogblock.BlockMessage,
		cogblock.BlockToolCall,
		cogblock.BlockToolResult,
		cogblock.BlockImport,
		cogblock.BlockAttention,
	}
	for _, k := range kinds {
		k := k
		kindregistry.Register(k, func(block *cogblock.CogBlock) error { return nil })
	}

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make(chan error, goroutines)

	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			block := &cogblock.CogBlock{
				Kind: kinds[i%len(kinds)],
			}
			if err := kindregistry.Dispatch(block); err != nil {
				errs <- err
			}
		}(i)
	}

	// Give goroutines a moment, then check for races via -race flag.
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

func TestHandlerErrorPropagates(t *testing.T) {
	kindregistry.Reset()

	sentinel := errors.New("handler error")
	kindregistry.Register(cogblock.BlockSystemEvent, func(block *cogblock.CogBlock) error {
		return sentinel
	})

	block := &cogblock.CogBlock{Kind: cogblock.BlockSystemEvent}
	err := kindregistry.Dispatch(block)
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected sentinel error, got %v", err)
	}
}
