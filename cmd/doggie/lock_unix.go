//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// errRunning means another DOGGIE already runs with this data dir.
var errRunning = errors.New("another DOGGIE is running with this data dir")

// Who holds the data dir lock: an explicit DOGGIE (doggie run) or one a chat
// runs in its own process, which gives way to an explicit one.
const (
	roleRun  = "run"
	roleChat = "chat"
)

// runningError is errRunning with who holds the lock, when known.
type runningError struct {
	dir  string
	pid  int
	role string
}

func (e *runningError) Error() string {
	if e.pid == 0 {
		return fmt.Sprintf("%v (%s)", errRunning, e.dir)
	}
	return fmt.Sprintf("%v (%s: doggie %s, pid %d)", errRunning, e.dir, e.role, e.pid)
}

func (e *runningError) Unwrap() error { return errRunning }

func lockPath(dir string) string { return filepath.Join(dir, "doggie.lock") }

// lockDataDir makes sure only one DOGGIE runs with dir: two would share its
// token, certificates and event log. The lock lasts until unlock is called
// or the process ends, even by kill -9. It records this process's pid and
// role for whoever finds it taken.
func lockDataDir(dir, role string) (unlock func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := lockPath(dir)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			pid, holder := lockHolder(dir)
			return nil, &runningError{dir: dir, pid: pid, role: holder}
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if err := f.Truncate(0); err == nil {
		f.WriteAt([]byte(fmt.Sprintf("%d %s\n", os.Getpid(), role)), 0)
	}
	return func() { f.Close() }, nil
}

// lockHolder reads who holds the lock; zero if unknown, e.g. while the
// holder is still writing it.
func lockHolder(dir string) (pid int, role string) {
	data, err := os.ReadFile(lockPath(dir))
	if err != nil {
		return 0, ""
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return 0, ""
	}
	pid, err = strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return 0, ""
	}
	return pid, fields[1]
}

// askToHandOver asks the chat holding the lock to give its DOGGIE up. Only
// processes of the same user can be asked.
func askToHandOver(pid int) error {
	return syscall.Kill(pid, syscall.SIGUSR1)
}

// handoverSignals delivers the requests to hand DOGGIE over, for a chat that
// may run it.
func handoverSignals() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGUSR1)
	return ch, func() { signal.Stop(ch) }
}
