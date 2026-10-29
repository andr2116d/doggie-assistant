package bus

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Drills for the audit: they measure how the bus behaves when the network
// breaks, with the real timings. Run with DOGGIE_DRILL=1.

func drill(t *testing.T) {
	if os.Getenv("DOGGIE_DRILL") != "1" {
		t.Skip("set DOGGIE_DRILL=1")
	}
}

// How long a satellite takes to notice a link that went silent.
func TestDrillHalfOpenDetection(t *testing.T) {
	drill(t)
	s := startTestServer(t, "127.0.0.1:-1")
	p := newFaultProxy(t, strings.TrimPrefix(s.ClientURL(), "nats://"))
	states := newConnStates()
	sat := states.dial(t, p.URL())
	defer sat.Close()
	start := time.Now()
	p.blackhole()
	select {
	case err := <-states.down:
		t.Logf("noticed after %s: %v", time.Since(start).Round(time.Second), err)
	case <-time.After(7 * time.Minute):
		t.Logf("not noticed after %s", time.Since(start).Round(time.Second))
	}
}
