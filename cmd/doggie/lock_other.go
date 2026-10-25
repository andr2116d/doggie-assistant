//go:build !unix

package main

import (
	"errors"
	"os"
)

var errRunning = errors.New("another DOGGIE is running with this data dir")

const (
	roleRun  = "run"
	roleChat = "chat"
)

// runningError is errRunning; without flock the holder is unknown.
type runningError struct {
	pid  int
	role string
}

func (e *runningError) Error() string { return errRunning.Error() }
func (e *runningError) Unwrap() error { return errRunning }

// lockDataDir does nothing where flock doesn't exist.
func lockDataDir(string, string) (unlock func(), err error) { return func() {}, nil }

func askToHandOver(int) error { return errors.ErrUnsupported }

func handoverSignals() (<-chan os.Signal, func()) { return nil, func() {} }
