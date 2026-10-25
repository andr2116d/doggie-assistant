package bus

import (
	"context"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

type ping struct {
	N int `json:"n"`
}

var testPing = NewSubject[ping]("test.ping")

func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v, ok := <-ch:
		if !ok {
			t.Fatal("channel closed")
		}
		return v
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
	panic("unreachable")
}

func TestPublishSubscribeTyped(t *testing.T) {
	s := startTestServer(t, "")
	pub := connect(t, s, "publisher")
	sub := connect(t, s, "subscriber")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := Subscribe(ctx, sub, testPing)
	if err != nil {
		t.Fatal(err)
	}

	meta, err := Publish(context.Background(), pub, testPing, ping{N: 7})
	if err != nil {
		t.Fatal(err)
	}
	ev := receive(t, events)
	if ev.Data.N != 7 {
		t.Errorf("Data.N = %d, want 7", ev.Data.N)
	}
	if ev.ID != meta.ID || ev.Type != "test.ping" || ev.Source != "publisher" || ev.Time.IsZero() {
		t.Errorf("unexpected meta: %+v (published %+v)", ev.Meta, meta)
	}
}

func TestCausedByChainsEvents(t *testing.T) {
	s := startTestServer(t, "")
	c := connect(t, s, "test")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := Subscribe(ctx, c, testPing)
	if err != nil {
		t.Fatal(err)
	}

	first, err := Publish(context.Background(), c, testPing, ping{N: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Publish(context.Background(), c, testPing, ping{N: 2}, CausedBy(first.ID)); err != nil {
		t.Fatal(err)
	}
	receive(t, events)
	if second := receive(t, events); second.CausedBy != first.ID {
		t.Errorf("CausedBy = %q, want %q", second.CausedBy, first.ID)
	}
}

func TestSubscribeClosesOnCancel(t *testing.T) {
	s := startTestServer(t, "")
	c := connect(t, s, "test")

	ctx, cancel := context.WithCancel(context.Background())
	events, err := Subscribe(ctx, c, testPing)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case _, ok := <-events:
		if ok {
			t.Fatal("received an event after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after cancel")
	}
}

func TestEventLogStoresEvents(t *testing.T) {
	s := startTestServer(t, "")
	c := connect(t, s, "test")
	for i := range 3 {
		if _, err := Publish(context.Background(), c, testPing, ping{N: i}); err != nil {
			t.Fatal(err)
		}
	}

	js, err := jetstream.New(c.nc)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		stream, err := js.Stream(context.Background(), EventLogStream)
		if err != nil {
			t.Fatal(err)
		}
		info, err := stream.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.State.Msgs == 3 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("event log has %d messages, want 3", info.State.Msgs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
