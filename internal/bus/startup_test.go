package bus

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestPortInUseFailsFast(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	start := time.Now()
	_, err = Start(context.Background(), opts(t.TempDir(), ln.Addr().String(), discardLog()))
	if err == nil {
		t.Fatal("Start succeeded on a port that is in use")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Start took %s to fail, want fast failure", elapsed)
	}
	t.Logf("error: %v", err)
}

func TestShutdownTwice(t *testing.T) {
	s := mustStart(t, opts(t.TempDir(), "", discardLog()))
	done := make(chan struct{})
	go func() {
		s.Shutdown()
		s.Shutdown()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("second Shutdown hung")
	}
}
