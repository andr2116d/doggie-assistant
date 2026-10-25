package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/task/worker"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// harness runs a bus, a task manager and a worker, plus a client connection
// that plays the requester.
type harness struct {
	t      *testing.T
	srv    *bus.Server
	client *bus.Conn
	opts   Options

	stopManager func()
}

func newHarness(t *testing.T, opts Options, funcs map[string]worker.Func) *harness {
	t.Helper()
	srv, err := bus.Start(context.Background(), bus.Options{
		Token: "t", StoreDir: t.TempDir(), MaxAge: time.Hour, MaxBytes: 64 << 20, Log: discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	h := &harness{t: t, srv: srv, client: connect(t, srv, "client"), opts: opts}
	h.startManager()
	if funcs != nil {
		h.startWorker(funcs)
	}
	return h
}

func connect(t *testing.T, srv *bus.Server, name string) *bus.Conn {
	t.Helper()
	c, err := srv.Connect(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// startManager runs a manager and waits until it serves commands.
func (h *harness) startManager() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	m := New(connect(h.t, h.srv, "tasks"), discardLog(), h.opts)
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()
	h.stopManager = func() {
		cancel()
		<-done
	}
	h.t.Cleanup(h.stopManager)

	waitReady(h.t, h.client)
}

// waitReady waits until a manager answers on c. Short attempts: a request can
// also hit a manager that is stopping and be refused.
func waitReady(t *testing.T, c *bus.Conn) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, err := bus.Request(ctx, c, events.ListTasks, events.TaskListRequest{})
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("manager not ready: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) startWorker(funcs map[string]worker.Func) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	wait, err := worker.Start(ctx, connect(h.t, h.srv, "worker"), discardLog(), funcs)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() {
		cancel()
		wait()
	})
}

func (h *harness) start(spec events.TaskSpec) (string, error) {
	ref, err := bus.Request(context.Background(), h.client, events.StartTask, spec)
	return ref.TaskID, err
}

func (h *harness) mustStart(spec events.TaskSpec) string {
	h.t.Helper()
	id, err := h.start(spec)
	if err != nil {
		h.t.Fatalf("start %s: %v", spec.Kind, err)
	}
	return id
}

func (h *harness) get(id string) events.TaskInfo {
	h.t.Helper()
	info, err := bus.Request(context.Background(), h.client, events.GetTask, events.TaskRef{TaskID: id})
	if err != nil {
		h.t.Fatalf("get %s: %v", id, err)
	}
	return info
}

func (h *harness) waitStatus(id string, want events.TaskStatus) events.TaskInfo {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		info := h.get(id)
		if info.Status == want {
			return info
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("task %s is %s (%s), want %s", id, info.Status, info.Error, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) cancel(id, reason string) error {
	_, err := bus.Request(context.Background(), h.client, events.CancelTask, events.TaskCancelRequest{TaskID: id, Reason: reason})
	return err
}

func (h *harness) answer(id, answer string) error {
	_, err := bus.Request(context.Background(), h.client, events.AnswerTask, events.TaskAnswer{TaskID: id, Answer: answer})
	return err
}

func testOptions() Options {
	return Options{DefaultTimeout: time.Minute, Retention: time.Hour}
}

// blocker is a worker func that blocks until its task is cancelled and
// reports both moments.
type blocker struct {
	started, stopped chan string
}

func newBlocker() *blocker {
	return &blocker{started: make(chan string, 8), stopped: make(chan string, 8)}
}

func (b *blocker) run(ctx context.Context, t *worker.Task) (any, error) {
	b.started <- t.ID
	<-ctx.Done()
	b.stopped <- t.ID
	return nil, ctx.Err()
}

func recv(t *testing.T, ch <-chan string, what string) string {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
	return ""
}

func echo(_ context.Context, t *worker.Task) (any, error) {
	var in map[string]any
	if err := t.Input(&in); err != nil {
		return nil, err
	}
	return in, nil
}

func TestTaskRunsToDone(t *testing.T) {
	h := newHarness(t, testOptions(), map[string]worker.Func{"test.echo": echo})
	id := h.mustStart(events.TaskSpec{Kind: "test.echo", Input: json.RawMessage(`{"msg":"hola"}`), Description: "echo"})
	if id != "t1" {
		t.Errorf("first task ID = %q, want t1", id)
	}
	info := h.waitStatus(id, events.StatusDone)
	if string(info.Result) != `{"msg":"hola"}` || info.Description != "echo" {
		t.Errorf("unexpected task: %+v", info)
	}
	if next := h.mustStart(events.TaskSpec{Kind: "test.echo"}); next != "t2" {
		t.Errorf("second task ID = %q, want t2", next)
	}
}

func TestStartWithoutWorkerFails(t *testing.T) {
	h := newHarness(t, testOptions(), nil)
	_, err := h.start(events.TaskSpec{Kind: "nobody.home"})
	var remote *bus.RemoteError
	if !errors.As(err, &remote) || !strings.Contains(remote.Message, `no worker for task kind "nobody.home"`) {
		t.Fatalf("err = %v, want a no-worker error", err)
	}
	if info := h.get("t1"); info.Status != events.StatusFailed {
		t.Errorf("task is %s, want failed", info.Status)
	}
}

func TestStartRejectsInvalidRequests(t *testing.T) {
	h := newHarness(t, testOptions(), nil)
	for _, spec := range []events.TaskSpec{
		{Kind: "bad kind"},
		{Kind: "task.>"},
		{Kind: ""},
		{Kind: "test.echo", ParentID: "t99"},
	} {
		if _, err := h.start(spec); err == nil {
			t.Errorf("start %+v succeeded", spec)
		}
	}
	list, _ := bus.Request(context.Background(), h.client, events.ListTasks, events.TaskListRequest{})
	if len(list.Tasks) != 0 {
		t.Errorf("rejected requests created tasks: %+v", list.Tasks)
	}
}

func TestProgressQuestionAndAnswer(t *testing.T) {
	h := newHarness(t, testOptions(), map[string]worker.Func{
		"test.ask": func(ctx context.Context, t *worker.Task) (any, error) {
			if err := t.Progress(ctx, "looking for profiles", 50); err != nil {
				return nil, err
			}
			answer, err := t.Ask(ctx, "Which profile?", "Andreé", "Guest")
			if err != nil {
				return nil, err
			}
			return map[string]string{"profile": answer}, nil
		},
	})
	id := h.mustStart(events.TaskSpec{Kind: "test.ask"})

	info := h.waitStatus(id, events.StatusWaitingInput)
	if info.Question != "Which profile?" || len(info.Options) != 2 || info.Progress != "looking for profiles" || info.Percent != 50 {
		t.Errorf("unexpected waiting task: %+v", info)
	}
	if err := h.answer(id, "Andreé"); err != nil {
		t.Fatal(err)
	}
	info = h.waitStatus(id, events.StatusDone)
	if string(info.Result) != `{"profile":"Andreé"}` || info.Question != "" {
		t.Errorf("unexpected done task: %+v", info)
	}
}

func TestAnswerRequiresWaitingTask(t *testing.T) {
	b := newBlocker()
	h := newHarness(t, testOptions(), map[string]worker.Func{"test.block": b.run})
	id := h.mustStart(events.TaskSpec{Kind: "test.block"})
	recv(t, b.started, "task start")
	if err := h.answer(id, "x"); err == nil || !strings.Contains(err.Error(), "not waiting for input") {
		t.Errorf("err = %v, want not waiting for input", err)
	}
	if err := h.answer("t99", "x"); err == nil {
		t.Error("answering an unknown task succeeded")
	}
}

func TestCancelStopsTheWorker(t *testing.T) {
	b := newBlocker()
	h := newHarness(t, testOptions(), map[string]worker.Func{"test.block": b.run})
	id := h.mustStart(events.TaskSpec{Kind: "test.block"})
	recv(t, b.started, "task start")

	if err := h.cancel(id, "olvídalo"); err != nil {
		t.Fatal(err)
	}
	if info := h.get(id); info.Status != events.StatusCancelled || info.Error != "olvídalo" {
		t.Errorf("task not cancelled at once: %+v", info)
	}
	if got := recv(t, b.stopped, "worker to stop"); got != id {
		t.Errorf("stopped task %q, want %q", got, id)
	}
	// The worker must not turn the cancellation into a failure.
	time.Sleep(100 * time.Millisecond)
	if info := h.get(id); info.Status != events.StatusCancelled {
		t.Errorf("task became %s after cancel", info.Status)
	}

	if err := h.cancel(id, ""); err == nil {
		t.Error("cancelling a finished task succeeded")
	}
	if err := h.cancel("t99", ""); err == nil {
		t.Error("cancelling an unknown task succeeded")
	}
}

func TestTimeoutFailsTheTask(t *testing.T) {
	b := newBlocker()
	opts := testOptions()
	opts.DefaultTimeout = 300 * time.Millisecond
	h := newHarness(t, opts, map[string]worker.Func{"test.block": b.run})
	id := h.mustStart(events.TaskSpec{Kind: "test.block"})

	info := h.waitStatus(id, events.StatusFailed)
	if !strings.Contains(info.Error, "timed out") {
		t.Errorf("Error = %q, want a timeout", info.Error)
	}
	recv(t, b.stopped, "worker to stop after the timeout")
}

func TestSubtasksAreCancelledWithTheirParent(t *testing.T) {
	b := newBlocker()
	childID := make(chan string, 1)
	h := newHarness(t, testOptions(), map[string]worker.Func{
		"test.block": b.run,
		"test.parent": func(ctx context.Context, t *worker.Task) (any, error) {
			id, err := t.Spawn(ctx, events.TaskSpec{Kind: "test.block"})
			if err != nil {
				return nil, err
			}
			childID <- id
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	parent := h.mustStart(events.TaskSpec{Kind: "test.parent", ThreadID: "voice"})
	child := recv(t, childID, "subtask")
	recv(t, b.started, "subtask start")
	if info := h.get(child); info.ParentID != parent || info.ThreadID != "voice" {
		t.Errorf("subtask %+v: want parent %s and inherited thread", info, parent)
	}

	if err := h.cancel(parent, ""); err != nil {
		t.Fatal(err)
	}
	info := h.waitStatus(child, events.StatusCancelled)
	if !strings.Contains(info.Error, "parent task "+parent) {
		t.Errorf("subtask Error = %q", info.Error)
	}
	recv(t, b.stopped, "subtask worker to stop")
}

func TestWorkerErrorsAndPanicsFailTheTask(t *testing.T) {
	h := newHarness(t, testOptions(), map[string]worker.Func{
		"test.error": func(context.Context, *worker.Task) (any, error) { return nil, errors.New("service unreachable") },
		"test.panic": func(context.Context, *worker.Task) (any, error) { panic("boom") },
		"test.echo":  echo,
	})
	for kind, want := range map[string]string{"test.error": "service unreachable", "test.panic": "panic: boom"} {
		info := h.waitStatus(h.mustStart(events.TaskSpec{Kind: kind}), events.StatusFailed)
		if info.Error != want {
			t.Errorf("%s: Error = %q, want %q", kind, info.Error, want)
		}
	}
	// The worker survived the panic.
	h.waitStatus(h.mustStart(events.TaskSpec{Kind: "test.echo"}), events.StatusDone)
}

func TestListAndGet(t *testing.T) {
	b := newBlocker()
	h := newHarness(t, testOptions(), map[string]worker.Func{"test.block": b.run, "test.echo": echo})
	done := h.mustStart(events.TaskSpec{Kind: "test.echo"})
	h.waitStatus(done, events.StatusDone)
	active := h.mustStart(events.TaskSpec{Kind: "test.block"})
	recv(t, b.started, "task start")

	all, _ := bus.Request(context.Background(), h.client, events.ListTasks, events.TaskListRequest{})
	if len(all.Tasks) != 2 || all.Tasks[0].ID != done || all.Tasks[1].ID != active {
		t.Errorf("list = %+v, want [%s %s] in order", all.Tasks, done, active)
	}
	act, _ := bus.Request(context.Background(), h.client, events.ListTasks, events.TaskListRequest{ActiveOnly: true})
	if len(act.Tasks) != 1 || act.Tasks[0].ID != active {
		t.Errorf("active list = %+v, want [%s]", act.Tasks, active)
	}
	if _, err := bus.Request(context.Background(), h.client, events.GetTask, events.TaskRef{TaskID: "t99"}); err == nil {
		t.Error("getting an unknown task succeeded")
	}
}

func TestRecoveryFailsInterruptedTasks(t *testing.T) {
	b := newBlocker()
	h := newHarness(t, testOptions(), map[string]worker.Func{"test.block": b.run, "test.echo": echo})
	done := h.mustStart(events.TaskSpec{Kind: "test.echo"})
	h.waitStatus(done, events.StatusDone)
	running := h.mustStart(events.TaskSpec{Kind: "test.block"})
	recv(t, b.started, "task start")

	h.stopManager()
	h.startManager()

	if info := h.get(done); info.Status != events.StatusDone {
		t.Errorf("finished task is %s after restart, want done", info.Status)
	}
	info := h.get(running)
	if info.Status != events.StatusFailed || !strings.Contains(info.Error, "restarted") {
		t.Errorf("interrupted task = %s (%q), want failed by restart", info.Status, info.Error)
	}
	if next := h.mustStart(events.TaskSpec{Kind: "test.echo"}); next != "t3" {
		t.Errorf("task ID after restart = %q, want t3 (IDs must not be reused)", next)
	}
}

func TestManyConcurrentTasks(t *testing.T) {
	h := newHarness(t, testOptions(), map[string]worker.Func{"test.echo": echo})
	const n = 50
	ids := make(chan string, n)
	errs := make(chan error, n)
	for i := range n {
		go func() {
			id, err := h.start(events.TaskSpec{Kind: "test.echo", Input: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i))})
			if err != nil {
				errs <- err
				return
			}
			ids <- id
		}()
	}
	seen := make(map[string]bool)
	for range n {
		select {
		case err := <-errs:
			t.Fatal(err)
		case id := <-ids:
			if seen[id] {
				t.Fatalf("duplicate task ID %s", id)
			}
			seen[id] = true
		case <-time.After(5 * time.Second):
			t.Fatal("timed out starting tasks")
		}
	}
	for id := range seen {
		h.waitStatus(id, events.StatusDone)
	}
}

func TestCancelByUserReachesItsEventsAndSubtasks(t *testing.T) {
	b := newBlocker()
	childID := make(chan string, 4)
	h := newHarness(t, testOptions(), map[string]worker.Func{
		"test.block": b.run,
		"test.parent": func(ctx context.Context, t *worker.Task) (any, error) {
			id, err := t.Spawn(ctx, events.TaskSpec{Kind: "test.block"})
			if err != nil {
				return nil, err
			}
			childID <- id
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	cancelled, err := bus.Subscribe(context.Background(), h.client, events.TaskCancelled)
	if err != nil {
		t.Fatal(err)
	}
	for _, byUser := range []bool{true, false} {
		parent := h.mustStart(events.TaskSpec{Kind: "test.parent"})
		child := recv(t, childID, "subtask")
		recv(t, b.started, "subtask start")
		_, err := bus.Request(context.Background(), h.client, events.CancelTask, events.TaskCancelRequest{TaskID: parent, ByUser: byUser})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for len(got) < 2 {
			select {
			case ev := <-cancelled:
				if ev.Data.ByUser != byUser {
					t.Errorf("by user %v: %s cancelled with ByUser = %v", byUser, ev.Data.TaskID, ev.Data.ByUser)
				}
				got[ev.Data.TaskID] = true
			case <-time.After(3 * time.Second):
				t.Fatalf("by user %v: cancelled %v, want %s and %s", byUser, got, parent, child)
			}
		}
		if !got[parent] || !got[child] {
			t.Errorf("by user %v: cancelled %v, want %s and %s", byUser, got, parent, child)
		}
		recv(t, b.stopped, "subtask worker to stop")
	}
}
