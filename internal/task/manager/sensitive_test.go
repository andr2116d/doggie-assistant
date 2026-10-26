package manager

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/task/worker"
)

func TestSensitiveTaskStaysOutOfTheLog(t *testing.T) {
	h := newHarness(t, testOptions(), map[string]worker.Func{"test.echo": echo})
	id := h.mustStart(events.TaskSpec{
		Kind: "test.echo", Sensitive: true, Description: "send a message",
		Input: json.RawMessage(`{"secret":"código-4821"}`),
	})
	info := h.waitStatus(id, events.StatusDone)
	if string(info.Result) != `{"secret":"código-4821"}` {
		t.Errorf("requester did not get the result: %s", info.Result)
	}

	logged, err := bus.History(context.Background(), h.client, time.Time{}, "task.>")
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range logged {
		if strings.Contains(string(env.Data), "4821") {
			t.Errorf("sensitive data in the event log: %s %s", env.Type, env.Data)
		}
	}
	// The manager logs every event of the task, in order.
	var types []string
	for _, env := range logged {
		types = append(types, env.Type)
	}
	if want := []string{events.TaskCreated.Type(), events.TaskRunning.Type(), events.TaskDone.Type()}; !slices.Equal(types, want) {
		t.Errorf("logged %q, want %q", types, want)
	}
	if done := logged[len(logged)-1]; !strings.Contains(string(done.Data), `"redacted":true`) {
		t.Errorf("task.done is not redacted: %s", done.Data)
	}

	// After a restart nothing sensitive survives: it only lived in memory.
	h.stopManager()
	h.startManager()
	if info := h.get(id); info.Status != events.StatusDone || len(info.Input) != 0 || len(info.Result) != 0 {
		t.Errorf("after restart: %+v, want done with no input or result", info)
	}
}

// Reports are taken only from the worker that accepted the task, and a
// task is only accepted with the claim of its run request.
func TestOnlyTheTasksWorkerReportsOnIt(t *testing.T) {
	b := newBlocker()
	h := newHarness(t, testOptions(), map[string]worker.Func{"test.block": b.run})
	id := h.mustStart(events.TaskSpec{Kind: "test.block"})
	recv(t, b.started, "task start")
	ctx := context.Background()
	impostor := connect(t, h.srv, "impostor")
	as := impostor.Identity()

	for name, call := range map[string]func() error{
		"done": func() error {
			_, err := bus.Request(ctx, impostor, events.WorkerDone.With(as), events.TaskDoneData{TaskID: id, Result: json.RawMessage("1")})
			return err
		},
		"failed": func() error {
			_, err := bus.Request(ctx, impostor, events.WorkerFailed.With(as), events.TaskFailedData{TaskID: id, Error: "x"})
			return err
		},
		"progress": func() error {
			_, err := bus.Request(ctx, impostor, events.WorkerProgress.With(as), events.TaskProgressData{TaskID: id, Message: "x"})
			return err
		},
		"ask": func() error {
			_, err := bus.Request(ctx, impostor, events.WorkerAsk.With(as), events.TaskNeedsInputData{TaskID: id, Question: "x"})
			return err
		},
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "not run by impostor") {
			t.Errorf("%s from another worker: err = %v", name, err)
		}
	}
	// Accepting a task already taken, or without its claim, fails too.
	if _, err := bus.Request(ctx, impostor, events.WorkerAccept.With(as), events.TaskClaim{TaskID: id}); err == nil {
		t.Error("accepted a running task again")
	}
	if info := h.get(id); info.Status != events.StatusRunning || info.Worker != "worker" {
		t.Errorf("task = %s by %q, want running by worker", info.Status, info.Worker)
	}
}

// A task is taken by whoever has its claim, not by whoever guesses its ID
// first.
func TestATaskIsAcceptedOnlyWithItsClaim(t *testing.T) {
	opts := testOptions()
	opts.DispatchTimeout = 300 * time.Millisecond
	h := newHarness(t, opts, nil)
	f := newFakeWorker(h)
	tried := make(chan error, 1)
	f.serveRuns("test.guess", func(in bus.Incoming[events.TaskInfo, events.Ack]) {
		_, err := bus.Request(f.ctx, f.conn, events.WorkerAccept.With(f.conn.Identity()), events.TaskClaim{TaskID: in.Req.ID, Claim: "guess"})
		tried <- err
	})
	if _, err := h.start(events.TaskSpec{Kind: "test.guess"}); err == nil {
		t.Error("the task started without being accepted")
	}
	if err := <-tried; err == nil || !strings.Contains(err.Error(), "no such run request") {
		t.Errorf("accept with a wrong claim: err = %v", err)
	}
	if info := h.get("t1"); info.Status != events.StatusFailed || info.Worker != "" {
		t.Errorf("task = %s by %q, want failed by nobody", info.Status, info.Worker)
	}
}
