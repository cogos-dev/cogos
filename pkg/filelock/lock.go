// Package filelock provides advisory file locking with cross-platform
// semantics.
//
// On Unix the lock is implemented via flock(2); on Windows via
// LockFileEx (golang.org/x/sys/windows). The public API is identical on
// both: Acquire returns a *FileLock, Release returns the descriptor.
//
// This package is used by pkg/alias and any other component that needs
// exclusive write access to node-local YAML files (aliases.yaml, global.yaml).
// The lock is advisory — callers that don't use filelock can still read and
// write the files. All cog CLI write paths go through this package.
//
// Lock lifecycle:
//
//	lock, err := filelock.Acquire(path, 2*time.Second)
//	if err != nil {
//	    return err  // ErrLockTimeout if another process holds the lock
//	}
//	defer lock.Release()
//	// ... read / mutate / atomic-write ...
package filelock

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// ErrLockTimeout is returned when Acquire cannot obtain the lock within
// the requested timeout.
var ErrLockTimeout = errors.New("filelock: timed out waiting for lock")

// errLockBusy is the internal sentinel returned by tryLock when another
// process currently holds the lock. Acquire converts it to ErrLockTimeout
// after the deadline elapses.
var errLockBusy = errors.New("filelock: lock busy")

// lockPollInterval is how often Acquire retries after a failed non-blocking
// lock attempt.
const lockPollInterval = 50 * time.Millisecond

// FileLock holds an open file descriptor that has been exclusively locked.
// Callers must call Release when finished.
type FileLock struct {
	file *os.File
}

// Acquire opens (or creates) the file at path and takes an exclusive
// advisory lock on it, retrying until the lock is obtained or timeout
// elapses. Returns ErrLockTimeout if the lock cannot be obtained before
// the deadline.
func Acquire(path string, timeout time.Duration) (*FileLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("filelock: open %q: %w", path, err)
	}

	deadline := time.Now().Add(timeout)
	for {
		err := tryLock(f.Fd())
		if err == nil {
			return &FileLock{file: f}, nil
		}
		if !errors.Is(err, errLockBusy) {
			f.Close()
			return nil, fmt.Errorf("filelock: lock %q: %w", path, err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("%w: %s", ErrLockTimeout, path)
		}
		time.Sleep(lockPollInterval)
	}
}

// Release unlocks the file and closes the descriptor. It is safe to call
// Release more than once; subsequent calls are no-ops.
func (l *FileLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	if err := unlock(l.file.Fd()); err != nil {
		l.file.Close()
		l.file = nil
		return fmt.Errorf("filelock: unlock: %w", err)
	}
	err := l.file.Close()
	l.file = nil
	return err
}
