//go:build !unix && !windows

package main

import "os"

// tryLock does nothing where the system has no file locks DOGGIE knows of:
// two DOGGIEs could share a data dir there.
func tryLock(*os.File) error { return nil }
