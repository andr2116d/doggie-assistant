//go:build !unix

package console

// watchResize reports nothing: without SIGWINCH, the size is read again on
// every frame.
func watchResize() (resized <-chan struct{}, stop func()) {
	return nil, func() {}
}
