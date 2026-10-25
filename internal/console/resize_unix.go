//go:build unix

package console

import (
	"os"
	"os/signal"
	"syscall"
)

// watchResize delivers a value each time the terminal changes size, until
// stop is called.
func watchResize() (resized <-chan struct{}, stop func()) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH)
	out := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-sig:
				select {
				case out <- struct{}{}:
				default: // one pending redraw is enough
				}
			case <-done:
				return
			}
		}
	}()
	return out, func() {
		signal.Stop(sig)
		close(done)
	}
}
