package bus

import (
	"strings"
	"testing"
	"time"
)

// shortPings makes silent links show within a fraction of a second.
func shortPings(t *testing.T) {
	t.Helper()
	oldInterval, oldOut := pingInterval, maxPingsOut
	pingInterval, maxPingsOut = 100*time.Millisecond, 2
	t.Cleanup(func() { pingInterval, maxPingsOut = oldInterval, oldOut })
}

// silentAfter is how long a dead link may go unnoticed with shortPings.
const silentAfter = 2 * time.Second

// A satellite whose link goes silent, as when the laptop sleeps or the
// tunnel drops without closing, notices it and starts reconnecting.
func TestSatelliteNoticesASilentLink(t *testing.T) {
	shortPings(t)
	s := startTestServer(t, "127.0.0.1:-1")
	p := newFaultProxy(t, strings.TrimPrefix(s.ClientURL(), "nats://"))
	states := newConnStates()
	sat := states.dial(t, p.URL())
	defer sat.Close()

	p.blackhole()
	start := time.Now()
	select {
	case <-states.down:
		t.Logf("noticed after %s", time.Since(start).Round(time.Millisecond))
	case <-time.After(silentAfter):
		t.Fatalf("a silent link went unnoticed for %s", silentAfter)
	}
	p.cut()
	p.restore()
	waitState(t, states.up, "the reconnection")
}

// The server drops a satellite that went silent, so it stops counting on
// its subscriptions, e.g. as a worker for tasks.
func TestServerDropsASilentSatellite(t *testing.T) {
	shortPings(t)
	s := startTestServer(t, "127.0.0.1:-1")
	p := newFaultProxy(t, strings.TrimPrefix(s.ClientURL(), "nats://"))
	before := s.ns.NumClients()
	sat := newConnStates().dial(t, p.URL())
	defer sat.Close()
	if got := s.ns.NumClients(); got != before+1 {
		t.Fatalf("clients = %d, want %d", got, before+1)
	}

	p.blackhole()
	deadline := time.Now().Add(silentAfter)
	for s.ns.NumClients() > before {
		if time.Now().After(deadline) {
			t.Fatalf("the server kept a silent satellite for %s", silentAfter)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
