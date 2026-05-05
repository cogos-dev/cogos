//go:build unix

// lock_unix.go — flock(2)-backed lock primitives for Unix platforms.

package filelock

import (
	"errors"
	"syscall"
)

// tryLock attempts a non-blocking exclusive lock on fd. Returns nil on
// success, errLockBusy if another process holds the lock, or a fatal error.
func tryLock(fd uintptr) error {
	err := syscall.Flock(int(fd), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errLockBusy
	}
	return err
}

// unlock releases a lock held on fd.
func unlock(fd uintptr) error {
	return syscall.Flock(int(fd), syscall.LOCK_UN)
}
