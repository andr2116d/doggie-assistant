package timer

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/task/manager"
)

// setup runs a bus, the task manager and the timer worker as tune leaves
// it, and returns a client connection.
func setup(t *testing.T, tune func(*Timer)) *bus.Conn {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := bus.Start(context.Background(), bus.Options{
		Token: "t", StoreDir: t.TempDir(), MaxAge: time.Hour, MaxBytes: 64 << 20, Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	connect := func(name string) *bus.Conn {
		c, err := srv.Connect(name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go manager.New(connect("tasks"), log, manager.DefaultOptions()).Run(ctx)
	tm := New(connect("timer"), log)
	tune(tm)
	go tm.Run(ctx)

	client := connect("client")
	deadline := time.Now().Add(5 * time.Second)
	for {
		// Ready when both the manager and the timer worker answer.
		rctx, rcancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, err := bus.Request(rctx, client, events.StartTask, events.TaskSpec{Kind: "timer", Input: json.RawMessage(`{"seconds":0}`)})
		rcancel()
		if err == nil {
			return client
		}
		if time.Now().After(deadline) {
			t.Fatalf("timer not ready: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// shortSeconds makes a timer's second last 20 ms.
func shortSeconds(tm *Timer) { tm.second, tm.check = 20*time.Millisecond, 5*time.Millisecond }

func start(t *testing.T, c *bus.Conn, input string) string {
	t.Helper()
	ref, err := bus.Request(context.Background(), c, events.StartTask, events.TaskSpec{Kind: "timer", Input: json.RawMessage(input)})
	if err != nil {
		t.Fatal(err)
	}
	return ref.TaskID
}

func get(t *testing.T, c *bus.Conn, id string) events.TaskInfo {
	t.Helper()
	info, err := bus.Request(context.Background(), c, events.GetTask, events.TaskRef{TaskID: id})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// waitFor waits until the task passes ok.
func waitFor(t *testing.T, c *bus.Conn, id, what string, ok func(events.TaskInfo) bool) events.TaskInfo {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		info := get(t, c, id)
		if ok(info) {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is %s (%s), want %s", id, info.Status, info.Error, what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func is(status events.TaskStatus) func(events.TaskInfo) bool {
	return func(info events.TaskInfo) bool { return info.Status == status }
}

// A timer tells at once when it will end, counted from when it was asked
// for, and ends then, not before.
func TestTimerEndsWhenItsTimeIsOver(t *testing.T) {
	c := setup(t, shortSeconds)
	id := start(t, c, `{"seconds":5,"label":"té"}`)
	info := waitFor(t, c, id, "telling its end", func(info events.TaskInfo) bool { return !info.EndsAt.IsZero() })
	ends := info.CreatedAt.Add(5 * 20 * time.Millisecond)
	if info.Status != events.StatusRunning || !info.EndsAt.Equal(ends) {
		t.Errorf("the timer is %s and ends at %v, want it running until %v", info.Status, info.EndsAt, ends)
	}
	info = waitFor(t, c, id, "done", is(events.StatusDone))
	if info.UpdatedAt.Before(ends) {
		t.Errorf("the timer ended %s early", ends.Sub(info.UpdatedAt))
	}
	if string(info.Result) != `{"seconds":5,"label":"té"}` {
		t.Errorf("Result = %s", info.Result)
	}
}

func TestTimerRejectsBadInput(t *testing.T) {
	c := setup(t, shortSeconds)
	long := `{"seconds":1,"label":"` + strings.Repeat("ñ", events.MaxTimerLabel+1) + `"}`
	// 9e18 s overflows time.Duration: the bound must be checked before converting.
	for _, input := range []string{``, `{}`, `{"seconds":0}`, `{"seconds":-3}`, `{"seconds":86341}`, `{"seconds":9000000000000000000}`, `{"seconds":"x"}`, long} {
		info := waitFor(t, c, start(t, c, input), "failed", is(events.StatusFailed))
		if info.Error == "" {
			t.Errorf("%s: failed without a reason", input)
		}
	}
	// The longest label and time are fine.
	id := start(t, c, `{"seconds":86340,"label":"`+strings.Repeat("ñ", events.MaxTimerLabel)+`"}`)
	waitFor(t, c, id, "telling its end", func(info events.TaskInfo) bool { return !info.EndsAt.IsZero() })
}

func TestTimerIsCancelled(t *testing.T) {
	c := setup(t, func(*Timer) {})
	id := start(t, c, `{"seconds":3600}`)
	waitFor(t, c, id, "telling its end", func(info events.TaskInfo) bool { return !info.EndsAt.IsZero() })
	if _, err := bus.Request(context.Background(), c, events.CancelTask, events.TaskCancelRequest{TaskID: id}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, c, id, "cancelled", is(events.StatusCancelled))
}

// A timer follows the clock: when the machine sleeps its waits stand
// still, and on waking the timer ends at once if its time has passed.
func TestTimerEndsByTheClock(t *testing.T) {
	var slept atomic.Int64
	c := setup(t, func(tm *Timer) {
		tm.check = 5 * time.Millisecond
		tm.now = func() time.Time { return time.Now().Add(time.Duration(slept.Load())) }
	})
	id := start(t, c, `{"seconds":3600}`)
	waitFor(t, c, id, "telling its end", func(info events.TaskInfo) bool { return !info.EndsAt.IsZero() })
	slept.Store(int64(59 * time.Minute))
	time.Sleep(20 * time.Millisecond)
	if info := get(t, c, id); info.Status != events.StatusRunning {
		t.Fatalf("the timer is %s with a minute left", info.Status)
	}
	slept.Store(int64(time.Hour))
	waitFor(t, c, id, "done", is(events.StatusDone))
}
