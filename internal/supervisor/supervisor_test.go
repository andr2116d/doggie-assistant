package supervisor

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeComponent crashes (by panicking or returning) for its first `crashes`
// runs, then blocks until ctx is cancelled.
type fakeComponent struct {
	name    string
	crashes int32
	panics  bool
	runs    atomic.Int32
	// ignoreCancel makes Run block forever, ignoring ctx.
	ignoreCancel bool
}

func (f *fakeComponent) Name() string { return f.name }

func (f *fakeComponent) Run(ctx context.Context) error {
	n := f.runs.Add(1)
	if n <= f.crashes {
		if f.panics {
			panic("boom")
		}
		return errors.New("failed")
	}
	if f.ignoreCancel {
		select {}
	}
	<-ctx.Done()
	return ctx.Err()
}

func testSupervisor() *Supervisor {
	return &Supervisor{
		Log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		MinBackoff:      time.Millisecond,
		MaxBackoff:      5 * time.Millisecond,
		StableAfter:     time.Minute,
		ShutdownTimeout: time.Second,
	}
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(time.Millisecond)
	}
}

func runInBackground(s *Supervisor, ctx context.Context, cs ...Component) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx, cs...) }()
	return errc
}

func TestRestartsCrashedComponents(t *testing.T) {
	tests := []struct {
		name   string
		panics bool
	}{
		{name: "panic", panics: true},
		{name: "early return", panics: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			c := &fakeComponent{name: "flaky", crashes: 2, panics: tt.panics}
			errc := runInBackground(testSupervisor(), ctx, c)

			waitFor(t, func() bool { return c.runs.Load() == 3 })
			cancel()
			if err := <-errc; err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := c.runs.Load(); got != 3 {
				t.Errorf("runs = %d, want 3 (no restart after shutdown)", got)
			}
		})
	}
}

func TestCrashDoesNotAffectOthers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	flaky := &fakeComponent{name: "flaky", crashes: 3, panics: true}
	steady := &fakeComponent{name: "steady"}
	errc := runInBackground(testSupervisor(), ctx, flaky, steady)

	waitFor(t, func() bool { return flaky.runs.Load() == 4 })
	if got := steady.runs.Load(); got != 1 {
		t.Errorf("steady runs = %d, want 1", got)
	}
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestShutdownTimeoutNamesStuckComponents(t *testing.T) {
	s := testSupervisor()
	s.ShutdownTimeout = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	stuck := &fakeComponent{name: "stuck", ignoreCancel: true}
	errc := runInBackground(s, ctx, stuck, &fakeComponent{name: "fine"})

	waitFor(t, func() bool { return stuck.runs.Load() == 1 })
	cancel()
	err := <-errc
	if err == nil || !strings.Contains(err.Error(), "stuck") || strings.Contains(err.Error(), "fine") {
		t.Fatalf("err = %v, want it to name only the stuck component", err)
	}
}
