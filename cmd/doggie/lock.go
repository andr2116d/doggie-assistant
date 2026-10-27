package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// errRunning means another DOGGIE already runs with this data dir.
var errRunning = errors.New("another DOGGIE is running with this data dir")

// errLocked is what tryLock returns when another process holds the lock.
var errLocked = errors.New("locked")

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
	if err := tryLock(f); err != nil {
		f.Close()
		if errors.Is(err, errLocked) {
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

// An explicit DOGGIE asks the chat that runs DOGGIE to hand it over with a
// file in the data dir, which only the owner's user can write: it works the
// same on every system, without signals and without the bus.

func handoverPath(dir string) string { return filepath.Join(dir, "doggie.handover") }

// handoverPoll is how often a chat that runs DOGGIE looks for a request. A
// variable so tests can shrink it.
var handoverPoll = 200 * time.Millisecond

// requestHandover asks the chat that runs DOGGIE with dir to hand it over.
func requestHandover(dir string) error {
	tmp, err := os.CreateTemp(dir, ".handover-*")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(tmp, "%d\n", os.Getpid())
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), handoverPath(dir))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// watchHandover delivers the requests to hand over the DOGGIE run with dir,
// until ctx is cancelled. A request older than the explicit DOGGIE waits
// for is left from one that gave up or died: it is dropped.
func watchHandover(ctx context.Context, dir string) <-chan struct{} {
	out := make(chan struct{}, 1)
	go func() {
		t := time.NewTicker(handoverPoll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			fi, err := os.Stat(handoverPath(dir))
			if err != nil {
				continue
			}
			os.Remove(handoverPath(dir))
			if time.Since(fi.ModTime()) > handoverTimeout {
				continue
			}
			select {
			case out <- struct{}{}:
			default: // one is already waiting
			}
		}
	}()
	return out
}
