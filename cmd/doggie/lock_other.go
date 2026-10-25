//go:build !unix

package main

import "errors"

var errRunning = errors.New("another DOGGIE is running with this data dir")

// lockDataDir does nothing where flock doesn't exist.
func lockDataDir(string) (unlock func(), err error) { return func() {}, nil }
