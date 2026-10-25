package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// setup starts a bus and returns a worker connection and a client connection
// that plays the task manager.
func setup(t *testing.T) (workerConn, client *bus.Conn) {
	t.Helper()
	srv, err := bus.Start(context.Background(), bus.Options{
		Token: "t", StoreDir: t.TempDir(), MaxAge: time.Hour, MaxBytes: 1 << 20, Log: discardLog(),
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
	return connect("worker"), connect("manager")
}

func failures(t *testing.T, ctx context.Context, c *bus.Conn) <-chan bus.Event[events.TaskFailedData] {
	t.Helper()
	ch, err := bus.Subscribe(ctx, c, events.TaskFailed)
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

func sendRun(t *testing.T, c *bus.Conn, kind string, info events.TaskInfo) {
	t.Helper()
	info.Kind = kind
	if _, err := bus.Request(context.Background(), c, events.RunTask.With(kind), info); err != nil {
		t.Fatalf("run %s: %v", kind, err)
	}
}

func blockUntilDone(ctx context.Context, _ *Task) (any, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestStartRejectsInvalidKind(t *testing.T) {
	w, _ := setup(t)
	if _, err := Start(context.Background(), w, discardLog(), map[string]Func{"bad kind": blockUntilDone}); err == nil {
		t.Fatal("Start accepted an invalid kind")
	}
}

func TestStoppingTheWorkerFailsRunningTasks(t *testing.T) {
	w, client := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failed := failures(t, ctx, client)

	workerCtx, stopWorker := context.WithCancel(context.Background())
	wait, err := Start(workerCtx, w, discardLog(), map[string]Func{"test.block": blockUntilDone})
	if err != nil {
		t.Fatal(err)
	}
	sendRun(t, client, "test.block", events.TaskInfo{ID: "t1"})
	stopWorker()
	wait()

	select {
	case ev := <-failed:
		if ev.Data.TaskID != "t1" || ev.Data.Error != "worker stopped" {
			t.Errorf("failure = %+v, want t1 worker stopped", ev.Data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no failure published for the interrupted task")
	}
}

func TestDeadlineFailsAsTimedOut(t *testing.T) {
	w, client := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failed := failures(t, ctx, client)
	if _, err := Start(ctx, w, discardLog(), map[string]Func{"test.block": blockUntilDone}); err != nil {
		t.Fatal(err)
	}

	sendRun(t, client, "test.block", events.TaskInfo{ID: "t1", Deadline: time.Now().Add(100 * time.Millisecond)})
	select {
	case ev := <-failed:
		if ev.Data.Error != "timed out" {
			t.Errorf("Error = %q, want timed out", ev.Data.Error)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("task past its deadline did not fail")
	}
}

func TestAnswerWithoutQuestionIsRejected(t *testing.T) {
	w, client := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := Start(ctx, w, discardLog(), map[string]Func{"test.block": blockUntilDone}); err != nil {
		t.Fatal(err)
	}
	sendRun(t, client, "test.block", events.TaskInfo{ID: "t1"})

	_, err := bus.Request(ctx, client, events.ControlTask.With("t1"), events.TaskControl{Action: events.ControlAnswer, Answer: "x"})
	var remote *bus.RemoteError
	if !errors.As(err, &remote) || !strings.Contains(remote.Message, "not waiting for input") {
		t.Fatalf("err = %v, want not waiting for input", err)
	}
	_, err = bus.Request(ctx, client, events.ControlTask.With("t1"), events.TaskControl{Action: "explode"})
	if !errors.As(err, &remote) {
		t.Fatalf("unknown action: err = %v, want a RemoteError", err)
	}
}

func TestInvalidInputFailsTheTask(t *testing.T) {
	w, client := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failed := failures(t, ctx, client)
	if _, err := Start(ctx, w, discardLog(), map[string]Func{
		"test.typed": func(_ context.Context, t *Task) (any, error) {
			var in struct {
				Seconds int `json:"seconds"`
			}
			return nil, t.Input(&in)
		},
	}); err != nil {
		t.Fatal(err)
	}

	sendRun(t, client, "test.typed", events.TaskInfo{ID: "t1", TaskSpec: events.TaskSpec{Input: json.RawMessage(`{"seconds":"five"}`)}})
	select {
	case ev := <-failed:
		if !strings.Contains(ev.Data.Error, "invalid input for test.typed") {
			t.Errorf("Error = %q", ev.Data.Error)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("task with invalid input did not fail")
	}
}

func TestOversizedResultFailsTheTask(t *testing.T) {
	w, client := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	failed := failures(t, ctx, client)
	if _, err := Start(ctx, w, discardLog(), map[string]Func{
		"test.huge": func(context.Context, *Task) (any, error) {
			return strings.Repeat("x", 2<<20), nil // above the 1 MB message limit
		},
	}); err != nil {
		t.Fatal(err)
	}

	sendRun(t, client, "test.huge", events.TaskInfo{ID: "t1"})
	select {
	case ev := <-failed:
		if !strings.Contains(ev.Data.Error, "publish result") {
			t.Errorf("Error = %q, want a publish failure", ev.Data.Error)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("task with an oversized result was left running")
	}
}
