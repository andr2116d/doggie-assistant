package bus

import (
	"context"
	"testing"
)

var testSignal = NewSignal[ping]("signaltest.beat")

func TestSignalsRoundTripAndStayOutOfTheLog(t *testing.T) {
	s := startTestServer(t, "")
	a, b := connect(t, s, "a"), connect(t, s, "b")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	beats, err := SubscribeSignal(ctx, b, testSignal)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := Emit(a, testSignal, ping{N: i}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 3 {
		if got := receive(t, beats); got.N != i {
			t.Errorf("signal %d = %+v", i, got)
		}
	}
	if n := eventLogInfo(t, a).State.Msgs; n != 0 {
		t.Errorf("event log has %d messages: signals must not be logged", n)
	}
	if got := testSignal.Wire(); got != "doggiesig.signaltest.beat" {
		t.Errorf("Wire = %q", got)
	}
}

func TestNewSignalValidation(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewSignal with a wildcard did not panic")
		}
	}()
	NewSignal[ping]("task.*")
}

// Signals under a signal arrive with the token they came under; others
// don't.
func TestSignalsUnderASignal(t *testing.T) {
	s := startTestServer(t, "")
	a, b := connect(t, s, "a"), connect(t, s, "b")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	beats, err := SubscribeSignalUnder(ctx, b, testSignal)
	if err != nil {
		t.Fatal(err)
	}
	Emit(a, testSignal, ping{N: 0})                // not under it
	Emit(a, testSignal.With("x", "y"), ping{N: 0}) // too deep
	Emit(a, testSignal.With("worker-1"), ping{N: 1})
	if got := receive(t, beats); got.Under != "worker-1" || got.Data.N != 1 {
		t.Errorf("got %+v", got)
	}
	if got := testSignal.With("w").Wire(); got != "doggiesig.signaltest.beat.w" {
		t.Errorf("Wire = %q", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("With accepted an invalid token")
		}
	}()
	testSignal.With("a b")
}
