package bus

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shortTries makes a publish give up on a silent try quickly.
func shortTries(t *testing.T) {
	t.Helper()
	old := publishTry
	publishTry = 300 * time.Millisecond
	t.Cleanup(func() { publishTry = old })
}

// outage is a satellite behind a fault proxy, and a core connection that
// sees what reaches the bus.
type outage struct {
	proxy  *faultProxy
	states *connStates
	sat    *Conn
	core   *Conn
}

func newOutage(t *testing.T) *outage {
	t.Helper()
	shortPings(t)
	shortTries(t)
	s := startTestServer(t, "127.0.0.1:-1")
	o := &outage{proxy: newFaultProxy(t, strings.TrimPrefix(s.ClientURL(), "nats://")), states: newConnStates(), core: connect(t, s, "core")}
	o.sat = o.states.dial(t, o.proxy.URL())
	t.Cleanup(func() { o.sat.Close() })
	return o
}

// cut breaks the link and waits until the satellite knows.
func (o *outage) cut(t *testing.T) {
	t.Helper()
	o.proxy.cut()
	waitState(t, o.states.down, "the cut to be noticed")
}

func (o *outage) restore(t *testing.T) {
	t.Helper()
	o.proxy.restore()
	waitState(t, o.states.up, "the reconnection")
}

// countPings counts the pings that reach the bus.
func countPings(t *testing.T, c *Conn) *atomic.Int32 {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	got, err := Subscribe(ctx, c, testPing)
	if err != nil {
		t.Fatal(err)
	}
	var n atomic.Int32
	go func() {
		for range got {
			n.Add(1)
		}
	}()
	return &n
}

// settle gives late copies time to show up: what is checked is that
// nothing more arrives.
const settle = 500 * time.Millisecond

func waitCount(t *testing.T, n *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for n.Load() < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(settle)
	if got := n.Load(); got != want {
		t.Fatalf("%d reached the bus, want %d", got, want)
	}
}

// An event published while the link is down waits for it, and is stored
// once it is back.
func TestPublishDuringACutWaitsAndArrivesOnce(t *testing.T) {
	o := newOutage(t)
	pings := countPings(t, o.core)
	o.cut(t)

	done := make(chan error, 1)
	go func() {
		_, err := Publish(context.Background(), o.sat, testPing, ping{N: 1})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("publish returned during the cut: %v", err)
	case <-time.After(settle):
	}

	o.restore(t)
	if err := <-done; err != nil {
		t.Fatalf("publish after the link came back: %v", err)
	}
	waitCount(t, pings, 1)
}

// When the link dies right after an event got through, the try times out
// without its ack; sent again once the link is back, it is still stored
// once.
func TestPublishWhoseAckWasLostIsStoredOnce(t *testing.T) {
	o := newOutage(t)
	pings := countPings(t, o.core)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	envs, err := SubscribeEnvelopes(ctx, o.core, testPing.Type())
	if err != nil {
		t.Fatal(err)
	}
	var seen atomic.Int32
	go func() {
		for range envs {
			seen.Add(1)
		}
	}()
	o.proxy.blackholeReplies()

	done := make(chan error, 1)
	go func() {
		_, err := Publish(context.Background(), o.sat, testPing, ping{N: 1})
		done <- err
	}()
	waitState(t, o.states.down, "the silent link to be noticed")
	o.restore(t)
	if err := <-done; err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitCount(t, pings, 1)
	waitCount(t, &seen, 1)
	logged, err := History(ctx, o.core, time.Time{}, testPing.Type())
	if err != nil || len(logged) != 1 {
		t.Errorf("the log holds %d copies (err %v), want 1", len(logged), err)
	}
}

// A publish the bus refuses on a live link still fails, without waiting.
func TestRefusedPublishStillFails(t *testing.T) {
	_, sat := startRestricted(t)
	start := time.Now()
	if _, err := Publish(context.Background(), sat, testManagerEvent, ping{N: 1}); err == nil {
		t.Fatal("a forbidden publish succeeded")
	}
	if took := time.Since(start); took > publishTry+probeTimeout+time.Second {
		t.Errorf("a refused publish took %s", took)
	}
}

// A request sent while the link is down fails at once, and is never
// carried out later.
func TestRequestDuringACutFailsAtOnce(t *testing.T) {
	o := newOutage(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var handled atomic.Int32
	serveFunc(t, ctx, o.core, testEcho, func(r echoReq) (echoResp, error) {
		handled.Add(1)
		return echoResp{Text: r.Text}, nil
	})
	o.cut(t)

	start := time.Now()
	_, err := Request(ctx, o.sat, testEcho, echoReq{Text: "cancel t7"})
	if !errors.Is(err, ErrDisconnected) {
		t.Fatalf("err = %v, want ErrDisconnected", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("failing took %s", took)
	}

	o.restore(t)
	waitCount(t, &handled, 0)
}

// RequestThroughOutages sends a request that couldn't leave once the link
// is back, and it is carried out once.
func TestRequestThroughOutagesSendsItOnceBack(t *testing.T) {
	o := newOutage(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var handled atomic.Int32
	serveFunc(t, ctx, o.core, testEcho, func(r echoReq) (echoResp, error) {
		handled.Add(1)
		return echoResp{Text: r.Text}, nil
	})
	o.cut(t)

	done := make(chan error, 1)
	go func() {
		rctx, rcancel := context.WithTimeout(ctx, 10*time.Second)
		defer rcancel()
		_, err := RequestThroughOutages(rctx, o.sat, testEcho, echoReq{Text: "task done"})
		done <- err
	}()
	time.Sleep(settle) // it waits meanwhile
	o.restore(t)
	if err := <-done; err != nil {
		t.Fatalf("request after the link came back: %v", err)
	}
	waitCount(t, &handled, 1)
}

func TestRecentIDsForgetsTheOldest(t *testing.T) {
	r := newRecentIDs()
	for i := range recentLimit + 1 {
		if !r.fresh(strconv.Itoa(i)) {
			t.Fatalf("%d not fresh the first time", i)
		}
	}
	if r.fresh(strconv.Itoa(recentLimit)) {
		t.Error("the newest ID came twice")
	}
	if !r.fresh("0") {
		t.Error("the oldest ID was not forgotten")
	}
	if len(r.ids) != recentLimit {
		t.Errorf("remembers %d IDs, want %d", len(r.ids), recentLimit)
	}
}

// A try that times out on a link that died before the pings noticed is
// not taken for a refusal: the publish waits, and goes through once the
// link is back.
func TestPublishOnALinkThatDiedUnnoticedWaits(t *testing.T) {
	o := newOutage(t)
	pingInterval = time.Second // slower than a try: the link looks up when it times out
	states := newConnStates()
	sat := states.dial(t, o.proxy.URL())
	defer sat.Close()
	o.proxy.blackhole()

	done := make(chan error, 1)
	go func() {
		_, err := Publish(context.Background(), sat, testPing, ping{N: 1})
		done <- err
	}()
	waitState(t, states.down, "the silent link to be noticed")
	o.proxy.restore()
	waitState(t, states.up, "the reconnection")
	if err := <-done; err != nil {
		t.Fatalf("publish: %v", err)
	}
}
