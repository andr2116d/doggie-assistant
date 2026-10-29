package bus

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Drills for the audit: they show how the bus behaves when the network
// breaks. Run with DOGGIE_DRILL=1.

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

// What a satellite publishes while it is cut off fails for it, then
// reaches the bus once it reconnects.
func TestDrillPublishDuringACutArrivesLater(t *testing.T) {
	drill(t)
	s := startTestServer(t, "127.0.0.1:-1")
	p := newFaultProxy(t, strings.TrimPrefix(s.ClientURL(), "nats://"))
	core := connect(t, s, "core")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, err := Subscribe(ctx, core, testPing)
	if err != nil {
		t.Fatal(err)
	}
	states := newConnStates()
	sat := states.dial(t, p.URL())
	defer sat.Close()

	p.cut()
	waitState(t, states.down, "the cut to be noticed")
	pctx, pcancel := context.WithTimeout(ctx, 2*time.Second)
	_, perr := Publish(pctx, sat, testPing, ping{N: 42})
	pcancel()
	t.Logf("publish during the cut: %v", perr)

	p.restore()
	waitState(t, states.up, "the reconnection")
	select {
	case ev := <-got:
		t.Logf("it reached the bus after the reconnection: ping %d, %s after it was sent", ev.Data.N, time.Since(ev.Time).Round(time.Millisecond))
	case <-time.After(5 * time.Second):
		t.Log("it never reached the bus")
	}
}

// A request sent while cut off times out for the requester, and is then
// carried out once the connection is back.
func TestDrillRequestDuringACutRunsLater(t *testing.T) {
	drill(t)
	s := startTestServer(t, "127.0.0.1:-1")
	p := newFaultProxy(t, strings.TrimPrefix(s.ClientURL(), "nats://"))
	core := connect(t, s, "core")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var handled atomic.Int32
	serveFunc(t, ctx, core, testEcho, func(r echoReq) (echoResp, error) {
		handled.Add(1)
		return echoResp{Text: r.Text}, nil
	})
	states := newConnStates()
	sat := states.dial(t, p.URL())
	defer sat.Close()

	p.cut()
	waitState(t, states.down, "the cut to be noticed")
	rctx, rcancel := context.WithTimeout(ctx, 2*time.Second)
	_, rerr := Request(rctx, sat, testEcho, echoReq{Text: "cancel t7"})
	rcancel()
	t.Logf("request during the cut: %v (handled %d)", rerr, handled.Load())

	p.restore()
	waitState(t, states.up, "the reconnection")
	deadline := time.Now().Add(5 * time.Second)
	for handled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	t.Logf("after the reconnection the handler ran it %d times", handled.Load())
}
