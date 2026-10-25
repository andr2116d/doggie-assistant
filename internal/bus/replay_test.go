package bus

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestReplayDeliversHistoryThenLive(t *testing.T) {
	s := startTestServer(t, "")
	c := connect(t, s, "test")
	for i := range 3 {
		Publish(c, testPing, ping{N: i})
	}
	waitForLogged(t, c, 3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := Replay(ctx, c, time.Now().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if got := receive(t, events).Data; !strings.Contains(string(got), fmt.Sprintf(`"n":%d`, i)) {
			t.Errorf("history event %d = %s", i, got)
		}
	}
	Publish(c, testPing, ping{N: 3})
	if got := receive(t, events).Data; !strings.Contains(string(got), `"n":3`) {
		t.Errorf("live event = %s", got)
	}
}

func TestReplayZeroSinceOnlyNew(t *testing.T) {
	s := startTestServer(t, "")
	c := connect(t, s, "test")
	Publish(c, testPing, ping{N: 1})
	waitForLogged(t, c, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, err := Replay(ctx, c, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	Publish(c, testPing, ping{N: 99})
	if got := receive(t, events).Data; !strings.Contains(string(got), `"n":99`) {
		t.Errorf("first event = %s, want only the new one", got)
	}
}

func TestReplayStopsOnCancel(t *testing.T) {
	s := startTestServer(t, "")
	c := connect(t, s, "test")
	ctx, cancel := context.WithCancel(context.Background())
	events, err := Replay(ctx, c, time.Time{})
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
		t.Fatal("replay channel not closed after cancel")
	}
}
