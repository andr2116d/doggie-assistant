//go:build !unix

package console

// watchResize reports nothing: without SIGWINCH, the width is read again on
// every redraw.
func watchResize() (resized <-chan struct{}, stop func()) {
	return nil, func() {}
}
