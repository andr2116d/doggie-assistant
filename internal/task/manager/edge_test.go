package manager

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/task/worker"
)

// fakeWorker speaks the worker protocol by hand, so tests control the exact
// order of replies and events.
type fakeWorker struct {
	t    *testing.T
	conn *bus.Conn
	ctx  context.Context
}

func newFakeWorker(h *harness) *fakeWorker {
	ctx, cancel := context.WithCancel(context.Background())
	h.t.Cleanup(cancel)
	return &fakeWorker{t: h.t, conn: connect(h.t, h.srv, "fake-worker"), ctx: ctx}
}

// serveRuns calls onRun for every run request of kind.
func (f *fakeWorker) serveRuns(kind string, onRun func(in bus.Incoming[events.TaskInfo, events.Ack])) {
	f.t.Helper()
	runs, err := bus.Serve(f.ctx, f.conn, events.RunTask.With(kind))
	if err != nil {
		f.t.Fatal(err)
	}
	go func() {
		for in := range runs {
			go onRun(in)
		}
	}()
}

// serveControl calls onControl for every control request of a task.
func (f *fakeWorker) serveControl(id string, onControl func(in bus.Incoming[events.TaskControl, events.Ack])) {
	f.t.Helper()
	ctl, err := bus.Serve(f.ctx, f.conn, events.ControlTask.With(f.conn.Identity(), id))
	if err != nil {
		f.t.Error(err)
		return
	}
	go func() {
		for in := range ctl {
			onControl(in)
		}
	}()
}

func (f *fakeWorker) ask(id, question string) {
	if _, err := bus.Request(f.ctx, f.conn, events.WorkerAsk.With(f.conn.Identity()), events.TaskNeedsInputData{TaskID: id, Question: question}); err != nil {
		f.t.Error(err)
	}
}

// claim accepts the task of a run request, without answering the request.
func (f *fakeWorker) claim(in bus.Incoming[events.TaskInfo, events.Ack]) {
	if _, err := bus.Request(f.ctx, f.conn, events.WorkerAccept.With(f.conn.Identity()), events.TaskClaim{TaskID: in.Req.ID, Claim: in.Req.Claim}); err != nil {
		f.t.Error(err)
	}
}

// accept accepts the task of a run request, as a worker does.
func (f *fakeWorker) accept(in bus.Incoming[events.TaskInfo, events.Ack]) {
	f.claim(in)
	in.Reply(events.Ack{})
}

// waitQuestion polls the manager until task id waits on question.
func (h *harness) waitQuestion(id, question string) events.TaskInfo {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		info := h.get(id)
		if info.Status == events.StatusWaitingInput && info.Question == question {
			return info
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("task %s is %s with question %q, want waiting on %q", id, info.Status, info.Question, question)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRestartedRunRebuildsStateAndStopsOrphans(t *testing.T) {
	b := newBlocker()
	h := newHarness(t, testOptions(), map[string]worker.Func{"test.block": b.run, "test.echo": echo})
	m := New(connect(t, h.srv, "tasks-2"), discardLog(), testOptions())
	h.stopManager() // the harness manager; m plays a supervisor-restarted one

	run := func() (stop func()) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { m.Run(ctx); close(done) }()
		waitReady(t, h.client)
		return func() { cancel(); <-done }
	}

	stop := run()
	id := h.mustStart(events.TaskSpec{Kind: "test.block"})
	recv(t, b.started, "task start")
	stop()

	// Same Manager value, like a supervisor restart after a crash.
	stop = run()
	defer stop()
	info := h.get(id)
	if info.Status != events.StatusFailed || !strings.Contains(info.Error, "restarted") {
		t.Errorf("task = %s (%q), want failed by restart", info.Status, info.Error)
	}
	// The worker kept running the task: the manager must tell it to stop.
	recv(t, b.stopped, "orphaned worker to be cancelled")

	list, _ := bus.Request(context.Background(), h.client, events.ListTasks, events.TaskListRequest{})
	if list.Total != 1 {
		t.Errorf("tasks after restart = %d, want 1 (no duplicated state)", list.Total)
	}
	if next := h.mustStart(events.TaskSpec{Kind: "test.echo"}); next != "t2" {
		t.Errorf("next ID = %q, want t2", next)
	}
}

func TestIDsContinueBeyondTheRecoveryWindow(t *testing.T) {
	opts := testOptions()
	opts.Retention = time.Second
	h := newHarness(t, opts, map[string]worker.Func{"test.echo": echo})
	h.waitStatus(h.mustStart(events.TaskSpec{Kind: "test.echo"}), events.StatusDone)
	h.waitStatus(h.mustStart(events.TaskSpec{Kind: "test.echo"}), events.StatusDone)

	time.Sleep(1200 * time.Millisecond) // both tasks fall out of the window
	h.stopManager()
	h.startManager()

	if next := h.mustStart(events.TaskSpec{Kind: "test.echo"}); next != "t3" {
		t.Errorf("next ID = %q, want t3: t1 and t2 are still in the event log", next)
	}
}

func TestTimeoutIsValidated(t *testing.T) {
	h := newHarness(t, testOptions(), map[string]worker.Func{"test.echo": echo})
	for _, tt := range []struct {
		timeoutS int
		want     string
	}{
		{-5, "must not be negative"},
		{1 << 40, "at most"},                        // would overflow time.Duration
		{int(time.Hour/time.Second) + 1, "at most"}, // above MaxTimeout (Retention: 1h)
	} {
		_, err := h.start(events.TaskSpec{Kind: "test.echo", TimeoutS: tt.timeoutS})
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("timeout_s=%d: err = %v, want %q", tt.timeoutS, err, tt.want)
		}
	}
	id := h.mustStart(events.TaskSpec{Kind: "test.echo", TimeoutS: 3600})
	h.waitStatus(id, events.StatusDone)
}

func TestAnsweringKeepsTheNextQuestion(t *testing.T) {
	h := newHarness(t, testOptions(), nil)
	f := newFakeWorker(h)
	f.serveRuns("test.chain", func(in bus.Incoming[events.TaskInfo, events.Ack]) {
		id := in.Req.ID
		f.serveControl(id, func(c bus.Incoming[events.TaskControl, events.Ack]) {
			// Ask the next question and let the manager record it BEFORE
			// confirming the first answer: the worst-case ordering.
			f.ask(id, "Q2")
			h.waitQuestion(id, "Q2")
			c.Reply(events.Ack{})
		})
		f.accept(in)
		f.ask(id, "Q1")
	})

	id := h.mustStart(events.TaskSpec{Kind: "test.chain"})
	h.waitQuestion(id, "Q1")
	if err := h.answer(id, "A1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if info := h.get(id); info.Status != events.StatusWaitingInput || info.Question != "Q2" {
		t.Fatalf("task = %s with question %q: the second question was lost", info.Status, info.Question)
	}
}

func TestConcurrentAnswersAreRejected(t *testing.T) {
	h := newHarness(t, testOptions(), nil)
	f := newFakeWorker(h)
	var delivered atomic.Int32
	inFlight, release := make(chan string, 1), make(chan struct{})
	f.serveRuns("test.slow", func(in bus.Incoming[events.TaskInfo, events.Ack]) {
		id := in.Req.ID
		f.serveControl(id, func(c bus.Incoming[events.TaskControl, events.Ack]) {
			delivered.Add(1)
			inFlight <- id
			<-release // keep the first answer in flight
			c.Reply(events.Ack{})
		})
		f.accept(in)
		f.ask(id, "Q1")
	})

	id := h.mustStart(events.TaskSpec{Kind: "test.slow"})
	h.waitQuestion(id, "Q1")
	first := make(chan error, 1)
	go func() { first <- h.answer(id, "A1") }()
	recv(t, inFlight, "the first answer to reach the worker")

	if err := h.answer(id, "A2"); err == nil || !strings.Contains(err.Error(), "already being delivered") {
		t.Errorf("second answer: err = %v, want already being delivered", err)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first answer: %v", err)
	}
	if n := delivered.Load(); n != 1 {
		t.Errorf("worker received %d answers, want 1", n)
	}
}

func TestAnswerToAGoneWorkerFailsTheTask(t *testing.T) {
	h := newHarness(t, testOptions(), nil)
	f := newFakeWorker(h)
	f.serveRuns("test.vanish", func(in bus.Incoming[events.TaskInfo, events.Ack]) {
		f.accept(in)
		f.ask(in.Req.ID, "Q1") // ...and never serves control: the worker is gone
	})

	id := h.mustStart(events.TaskSpec{Kind: "test.vanish"})
	h.waitQuestion(id, "Q1")
	if err := h.answer(id, "A1"); err == nil || !strings.Contains(err.Error(), "worker is gone") {
		t.Errorf("err = %v, want worker is gone", err)
	}
	info := h.get(id)
	if info.Status != events.StatusFailed || !strings.Contains(info.Error, "worker is gone") {
		t.Errorf("task = %s (%q), want failed: nobody can answer it anymore", info.Status, info.Error)
	}
}

func TestLostDispatchReplyStopsTheWorker(t *testing.T) {
	opts := testOptions()
	opts.DispatchTimeout = 200 * time.Millisecond
	h := newHarness(t, opts, nil)
	f := newFakeWorker(h)
	cancelled := make(chan string, 1)
	f.serveRuns("test.mute", func(in bus.Incoming[events.TaskInfo, events.Ack]) {
		// Accepts and starts working, but its reply never arrives.
		f.serveControl(in.Req.ID, func(c bus.Incoming[events.TaskControl, events.Ack]) {
			if c.Req.Action == events.ControlCancel {
				cancelled <- in.Req.ID
			}
			c.Reply(events.Ack{})
		})
		f.claim(in)
	})

	_, err := h.start(events.TaskSpec{Kind: "test.mute"})
	if err == nil || !strings.Contains(err.Error(), "dispatch failed") {
		t.Fatalf("err = %v, want dispatch failed", err)
	}
	if got := recv(t, cancelled, "orphaned worker to be cancelled"); got != "t1" {
		t.Errorf("cancelled %q, want t1", got)
	}
	if info := h.get("t1"); info.Status != events.StatusFailed {
		t.Errorf("task is %s, want failed", info.Status)
	}
}

func TestAnswerRacingATimeoutIsRejected(t *testing.T) {
	opts := testOptions()
	opts.DefaultTimeout = 200 * time.Millisecond
	h := newHarness(t, opts, nil)
	f := newFakeWorker(h)
	f.serveRuns("test.late", func(in bus.Incoming[events.TaskInfo, events.Ack]) {
		f.serveControl(in.Req.ID, func(c bus.Incoming[events.TaskControl, events.Ack]) {
			time.Sleep(400 * time.Millisecond) // the task times out meanwhile
			c.Reply(events.Ack{})
		})
		f.accept(in)
		f.ask(in.Req.ID, "Q1")
	})

	id := h.mustStart(events.TaskSpec{Kind: "test.late"})
	h.waitQuestion(id, "Q1")
	err := h.answer(id, "A1")
	var remote *bus.RemoteError
	if !errors.As(err, &remote) || !strings.Contains(remote.Message, "already failed") {
		t.Errorf("err = %v, want already failed: the answer arrived after the timeout", err)
	}
}

func TestListLimit(t *testing.T) {
	h := newHarness(t, testOptions(), map[string]worker.Func{"test.echo": echo})
	for range 5 {
		h.waitStatus(h.mustStart(events.TaskSpec{Kind: "test.echo"}), events.StatusDone)
	}
	list, err := bus.Request(context.Background(), h.client, events.ListTasks, events.TaskListRequest{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if list.Total != 5 || len(list.Tasks) != 2 || list.Tasks[0].ID != "t4" || list.Tasks[1].ID != "t5" {
		t.Errorf("list = total %d %+v, want the 2 most recent (t4, t5) of 5", list.Total, list.Tasks)
	}
	list, _ = bus.Request(context.Background(), h.client, events.ListTasks, events.TaskListRequest{Limit: 1_000_000})
	if len(list.Tasks) != 5 {
		t.Errorf("huge limit returned %d tasks, want 5", len(list.Tasks))
	}
}

func TestOptionsDefaults(t *testing.T) {
	o := Options{Retention: time.Hour, MaxTimeout: 48 * time.Hour}.withDefaults()
	if o.DispatchTimeout <= 0 || o.DefaultTimeout <= 0 {
		t.Errorf("zero fields not defaulted: %+v", o)
	}
	if o.MaxTimeout != time.Hour {
		t.Errorf("MaxTimeout = %s, want capped at Retention (1h)", o.MaxTimeout)
	}
}

// A fast worker can finish (task.done reaches the manager) before the
// manager processes the delivery of the answer that unblocked it. The answer
// was used, so answering must succeed.
func TestAnswerThatFinishesTheTaskSucceeds(t *testing.T) {
	h := newHarness(t, testOptions(), nil)
	f := newFakeWorker(h)
	f.serveRuns("test.quick", func(in bus.Incoming[events.TaskInfo, events.Ack]) {
		id := in.Req.ID
		f.serveControl(id, func(c bus.Incoming[events.TaskControl, events.Ack]) {
			bus.Request(f.ctx, f.conn, events.WorkerDone.With(f.conn.Identity()), events.TaskDoneData{TaskID: id})
			h.waitStatus(id, events.StatusDone) // done is recorded first...
			c.Reply(events.Ack{})               // ...then the delivery is confirmed
		})
		f.accept(in)
		f.ask(id, "Q1")
	})

	id := h.mustStart(events.TaskSpec{Kind: "test.quick"})
	h.waitQuestion(id, "Q1")
	if err := h.answer(id, "A1"); err != nil {
		t.Fatalf("answer that finished the task was reported as failed: %v", err)
	}
}

// A task cancelled while its worker is about to accept it: the requester
// still gets its ID, the worker can't take it and never runs it.
func TestCancelledBeforeItsAccepted(t *testing.T) {
	h := newHarness(t, testOptions(), nil)
	f := newFakeWorker(h)
	refused, got := make(chan error, 1), make(chan string, 1)
	f.serveRuns("test.hesitant", func(in bus.Incoming[events.TaskInfo, events.Ack]) {
		got <- in.Req.ID
		h.waitStatus(in.Req.ID, events.StatusCancelled)
		_, err := bus.Request(f.ctx, f.conn, events.WorkerAccept.With(f.conn.Identity()), events.TaskClaim{TaskID: in.Req.ID, Claim: in.Req.Claim})
		refused <- err
		in.Fail(err)
	})
	started := make(chan error, 1)
	go func() {
		_, err := h.start(events.TaskSpec{Kind: "test.hesitant"})
		started <- err
	}()
	id := recv(t, got, "the run request")
	if _, err := bus.Request(context.Background(), h.client, events.CancelTask, events.TaskCancelRequest{TaskID: id}); err != nil {
		t.Fatal(err)
	}
	if err := <-refused; err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("the worker took a cancelled task: %v", err)
	}
	if err := <-started; err != nil {
		t.Errorf("start failed for a task that exists: %v", err)
	}
	if info := h.get(id); info.Status != events.StatusCancelled || info.Worker != "" {
		t.Errorf("task = %s by %q", info.Status, info.Worker)
	}
}
