package bus

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentPublishers(t *testing.T) {
	s := startTestServer(t, "")
	sub := connect(t, s, "subscriber")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := Subscribe(ctx, sub, testPing)
	if err != nil {
		t.Fatal(err)
	}

	const workers, perWorker = 10, 50
	var wg sync.WaitGroup
	for w := range workers {
		pub := connect(t, s, fmt.Sprintf("pub-%d", w))
		wg.Go(func() {
			for i := range perWorker {
				if _, err := Publish(context.Background(), pub, testPing, ping{N: i}); err != nil {
					t.Error(err)
				}
			}
		})
	}

	seen := make(map[string]bool)
	for len(seen) < workers*perWorker {
		ev := receive(t, events)
		if seen[ev.ID] {
			t.Fatalf("duplicate event ID %s", ev.ID)
		}
		seen[ev.ID] = true
	}
	wg.Wait()
}

func TestSubscribeIsActiveWhenItReturns(t *testing.T) {
	s := startTestServer(t, "127.0.0.1:-1")
	proxy := delayProxy(t, strings.TrimPrefix(s.ClientURL(), "nats://"), 100*time.Millisecond)

	sat, err := Dial(proxy, DialOptions{Name: "slow-link", Token: "test-token", Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	defer sat.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := Subscribe(ctx, sat, testPing)
	if err != nil {
		t.Fatal(err)
	}
	// Published right after Subscribe returns, over a fast link, while the
	// subscriber's link is slow: it must still be delivered.
	core := connect(t, s, "core")
	if _, err := Publish(context.Background(), core, testPing, ping{N: 1}); err != nil {
		t.Fatal(err)
	}
	receive(t, events)
}

func TestSlowConsumerIsReported(t *testing.T) {
	defer func(old int) { pendingLimit = old }(pendingLimit)
	pendingLimit = 100

	h := &captureHandler{}
	s := mustStart(t, opts(t.TempDir(), "", slog.New(h)))
	defer s.Shutdown()
	slow, _ := s.Connect("slow")
	defer slow.Close()
	pub, _ := s.Connect("pub")
	defer pub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := Subscribe(ctx, slow, testPing); err != nil { // never read
		t.Fatal(err)
	}
	for i := range 5000 {
		Publish(context.Background(), pub, testPing, ping{N: i})
	}

	deadline := time.Now().Add(3 * time.Second)
	for !h.contains("slow consumer") {
		if time.Now().After(deadline) {
			t.Fatal("dropped events were not reported as a slow consumer")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCloseFlushesAndEventLogSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s1 := mustStart(t, opts(dir, "", discardLog()))
	c, err := s1.Connect("pub")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 500 {
		if _, err := Publish(context.Background(), c, testPing, ping{N: i}); err != nil {
			t.Fatal(err)
		}
	}
	// Close must not return before every publish reached the server, or
	// shutting down right after would lose events.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	s1.Shutdown()

	o := opts(dir, "", discardLog())
	o.MaxAge = 2 * time.Hour
	s2 := mustStart(t, o)
	defer s2.Shutdown()
	r := connect(t, s2, "reader")
	info := eventLogInfo(t, r)
	if info.State.Msgs != 500 {
		t.Errorf("event log has %d events after restart, want 500", info.State.Msgs)
	}
	if info.Config.MaxAge != 2*time.Hour {
		t.Errorf("MaxAge = %s after restart, want the new 2h", info.Config.MaxAge)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	s := startTestServer(t, "")
	c, _ := s.Connect("test")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestSatelliteReconnectsAfterRestart(t *testing.T) {
	dir := t.TempDir()
	s1 := mustStart(t, opts(dir, "127.0.0.1:-1", discardLog()))
	url := s1.ClientURL()

	sat, err := Dial(url, DialOptions{Name: "satellite", Token: "test-token", Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	defer sat.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := Subscribe(ctx, sat, testPing)
	if err != nil {
		t.Fatal(err)
	}

	s1.Shutdown()
	addr := strings.TrimPrefix(url, "nats://")
	s2 := mustStart(t, opts(dir, addr, discardLog()))
	defer s2.Shutdown()
	core := connect(t, s2, "core")

	// Keep publishing until the satellite has reconnected and resubscribed.
	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case ev := <-events:
			if ev.Source != "core" {
				t.Errorf("Source = %q, want core", ev.Source)
			}
			return
		case <-tick.C:
			Publish(context.Background(), core, testPing, ping{N: 1})
		case <-deadline:
			t.Fatal("satellite did not receive events after the core restarted")
		}
	}
}
