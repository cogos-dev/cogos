//go:build windows

// lock_windows.go — LockFileEx-backed lock primitives for Windows.

package filelock

import (
	"errors"

	"golang.org/x/sys/windows"
)

// lockWholeFile locks every byte; LockFileEx accepts a 64-bit length split
// into two uint32 halves.
const lockWholeFile = ^uint32(0)

// tryLock attempts a non-blocking exclusive lock on fd. Returns nil on
// success, errLockBusy if another process holds the lock, or a fatal error.
func tryLock(fd uintptr) error {
	overlapped := &windows.Overlapped{}
	err := windows.LockFileEx(
		windows.Handle(fd),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, lockWholeFile, lockWholeFile, overlapped,
	)
	if err == nil {
		return nil
	}
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return errLockBusy
	}
	return err
}

// unlock releases a lock held on fd.
func unlock(fd uintptr) error {
	overlapped := &windows.Overlapped{}
	return windows.UnlockFileEx(
		windows.Handle(fd),
		0, lockWholeFile, lockWholeFile, overlapped,
	)
}
