//go:build unix

package main

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes an exclusive lock on f without waiting. The system drops it
// when the process ends.
func tryLock(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errLocked
	}
	return err
}
