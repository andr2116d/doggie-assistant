//go:build windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLock takes an exclusive lock on f without waiting. The system drops it
// when the process ends. It locks a byte far past the content: a locked
// byte can't be read on Windows, and who holds the lock must stay readable.
func tryLock(f *os.File) error {
	ol := &windows.Overlapped{OffsetHigh: 1}
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_IO_PENDING) {
		return errLocked
	}
	return err
}
