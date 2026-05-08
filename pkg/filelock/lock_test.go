package filelock_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/myrgic/cogos/pkg/filelock"
)

// TestAcquireRelease verifies basic lock/unlock lifecycle.
func TestAcquireRelease(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")

	lock, err := filelock.Acquire(lockPath, 2*time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// File should exist after Acquire.
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("lock file not created: %v", err)
	}
}

// TestAcquireReleaseTwice verifies that acquiring after releasing works.
func TestAcquireReleaseTwice(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")

	for i := 0; i < 2; i++ {
		lock, err := filelock.Acquire(lockPath, 2*time.Second)
		if err != nil {
			t.Fatalf("iteration %d: Acquire: %v", i, err)
		}
		if err := lock.Release(); err != nil {
			t.Fatalf("iteration %d: Release: %v", i, err)
		}
	}
}

// TestReleaseNilSafe verifies Release on nil is a no-op.
func TestReleaseNilSafe(t *testing.T) {
	var l *filelock.FileLock
	if err := l.Release(); err != nil {
		t.Fatalf("nil Release returned error: %v", err)
	}
}

// TestContentionTimeout verifies that a second lock attempt times out when
// the first lock is held past the deadline.
func TestContentionTimeout(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "contention.lock")

	// Acquire first lock in background goroutine; hold it for longer than
	// the second caller's timeout.
	firstAcquired := make(chan struct{})
	firstRelease := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		lock, err := filelock.Acquire(lockPath, 2*time.Second)
		if err != nil {
			t.Errorf("first Acquire: %v", err)
			close(firstAcquired)
			return
		}
		close(firstAcquired)
		<-firstRelease
		lock.Release()
	}()

	// Wait for first goroutine to hold the lock.
	<-firstAcquired

	// Second acquire with a short timeout should fail with ErrLockTimeout.
	_, err := filelock.Acquire(lockPath, 150*time.Millisecond)
	if !errors.Is(err, filelock.ErrLockTimeout) {
		t.Errorf("expected ErrLockTimeout, got: %v", err)
	}

	// Release first lock; verify a fresh acquire now succeeds.
	close(firstRelease)
	wg.Wait()

	lock2, err := filelock.Acquire(lockPath, 2*time.Second)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	defer lock2.Release()
}

// TestSerialWrite verifies that two parallel acquires both succeed when they
// run sequentially (first holds, second waits, then succeeds after first releases).
func TestSerialWrite(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "serial.lock")

	const writers = 2
	results := make([]error, writers)
	var wg sync.WaitGroup

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			lock, err := filelock.Acquire(lockPath, 3*time.Second)
			if err != nil {
				results[idx] = err
				return
			}
			// Simulate a short write.
			time.Sleep(20 * time.Millisecond)
			lock.Release()
		}(i)
	}
	wg.Wait()

	for i, err := range results {
		if err != nil {
			t.Errorf("writer %d: %v", i, err)
		}
	}
}
