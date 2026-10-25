package bus

import (
	"context"
	"testing"
	"time"
)

var (
	testTaskA = NewSubject[ping]("tasktest.a")
	testTaskB = NewSubject[ping]("tasktest.b")
	testOther = NewSubject[ping]("othertest.x")
)

func TestValidPattern(t *testing.T) {
	for p, want := range map[string]bool{
		"task.>": true, "task.*": true, "*.done": true, "task.done": true, ">": true,
		"": false, "task.>.x": false, "task..done": false, "task.d*": false, "task done": false,
	} {
		if got := validPattern(p); got != want {
			t.Errorf("validPattern(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestHistoryReturnsMatchingEventsInOrder(t *testing.T) {
	s := startTestServer(t, "")
	c := connect(t, s, "test")
	ctx := context.Background()
	Publish(ctx, c, testTaskA, ping{N: 1})
	Publish(ctx, c, testOther, ping{N: 2})
	Publish(ctx, c, testTaskB, ping{N: 3})

	got, err := History(ctx, c, time.Time{}, "tasktest.>")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Type != "tasktest.a" || got[1].Type != "tasktest.b" {
		t.Fatalf("history = %+v, want tasktest.a then tasktest.b", got)
	}
}

func TestHistorySince(t *testing.T) {
	s := startTestServer(t, "")
	c := connect(t, s, "test")
	ctx := context.Background()
	Publish(ctx, c, testTaskA, ping{N: 1})
	time.Sleep(20 * time.Millisecond)
	cut := time.Now()
	Publish(ctx, c, testTaskA, ping{N: 2})

	got, err := History(ctx, c, cut, "tasktest.>")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("history has %d events, want 1", len(got))
	}
	if ev, err := Decode(got[0], testTaskA); err != nil || ev.Data.N != 2 {
		t.Errorf("event = %+v, %v; want N=2", ev, err)
	}
}

func TestHistoryEmpty(t *testing.T) {
	s := startTestServer(t, "")
	got, err := History(context.Background(), connect(t, s, "test"), time.Time{}, "tasktest.>")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want empty", got, err)
	}
}

func TestSubscribeEnvelopesMatchesPattern(t *testing.T) {
	s := startTestServer(t, "")
	c := connect(t, s, "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	envs, err := SubscribeEnvelopes(ctx, c, "tasktest.>")
	if err != nil {
		t.Fatal(err)
	}
	Publish(ctx, c, testOther, ping{N: 1})
	Publish(ctx, c, testTaskB, ping{N: 2})

	env := receive(t, envs)
	if env.Type != "tasktest.b" {
		t.Errorf("Type = %q, want tasktest.b (othertest.x must not match)", env.Type)
	}
}

func TestDecodeChecksType(t *testing.T) {
	env := Envelope{Meta: Meta{Type: "tasktest.a"}, Data: []byte(`{"n":5}`)}
	if ev, err := Decode(env, testTaskA); err != nil || ev.Data.N != 5 {
		t.Errorf("Decode = %+v, %v", ev, err)
	}
	if _, err := Decode(env, testTaskB); err == nil {
		t.Error("Decode accepted an event of another type")
	}
}

func TestInvalidPatternsAreRejected(t *testing.T) {
	s := startTestServer(t, "")
	c := connect(t, s, "test")
	if _, err := SubscribeEnvelopes(context.Background(), c, "task.>.x"); err == nil {
		t.Error("SubscribeEnvelopes accepted an invalid pattern")
	}
	if _, err := History(context.Background(), c, time.Time{}, ""); err == nil {
		t.Error("History accepted an empty pattern")
	}
}

func TestLastEvent(t *testing.T) {
	s := startTestServer(t, "")
	c := connect(t, s, "test")
	ctx := context.Background()

	if _, ok, err := LastEvent(ctx, c, testTaskA); err != nil || ok {
		t.Fatalf("empty log: ok=%v err=%v, want none", ok, err)
	}
	Publish(ctx, c, testTaskA, ping{N: 1})
	Publish(ctx, c, testTaskA, ping{N: 2})
	Publish(ctx, c, testTaskB, ping{N: 3})

	ev, ok, err := LastEvent(ctx, c, testTaskA)
	if err != nil || !ok || ev.Data.N != 2 {
		t.Fatalf("LastEvent = %+v, %v, %v; want N=2", ev, ok, err)
	}
}
