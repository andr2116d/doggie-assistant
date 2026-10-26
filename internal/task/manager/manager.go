// Package manager is the task manager component: the source of truth for the
// state of every task.
//
// It serves the task commands (start, cancel, answer, list, get), dispatches
// each task to a worker of its kind, takes the reports of the worker that
// accepted it, enforces timeouts and cancels subtasks with their parent. It
// is the only one that publishes task events, so they are logged in the
// order it applied them. On start it rebuilds its state from the event log
// and fails the tasks a restart interrupted.
//
// The manager is an actor: all state lives in the Run goroutine. Slow work
// (round trips to workers) runs in goroutines that send their outcome back to
// the loop as a closure.
package manager

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/task"
)

// controlTimeout bounds round trips to a worker (cancel, answer).
const controlTimeout = 5 * time.Second

// List limits: the reply must fit in one NATS message (1 MB by default).
const (
	defaultListLimit = 100
	maxListLimit     = 500
)

// Options configures the manager.
type Options struct {
	// DefaultTimeout applies to tasks that don't set their own.
	DefaultTimeout time.Duration
	// MaxTimeout caps the timeout a task may ask for. It must not exceed
	// Retention, so a running task is always within the recovery window.
	MaxTimeout time.Duration
	// DispatchTimeout bounds how long a worker may take to accept a task.
	DispatchTimeout time.Duration
	// Retention is how long finished tasks are kept, and how far back the
	// event log is read on start.
	Retention time.Duration
	// HeartbeatInterval is how often workers report each running task. A
	// task silent for heartbeatTolerance intervals fails: its worker died.
	HeartbeatInterval time.Duration
}

// heartbeatTolerance is how many heartbeat intervals a task may miss before
// its worker is considered dead.
const heartbeatTolerance = 3

// DefaultOptions returns production settings.
func DefaultOptions() Options {
	return Options{
		DefaultTimeout:    10 * time.Minute,
		MaxTimeout:        24 * time.Hour,
		DispatchTimeout:   10 * time.Second,
		Retention:         24 * time.Hour,
		HeartbeatInterval: 5 * time.Second,
	}
}

// Manager is the task manager component.
type Manager struct {
	conn *bus.Conn
	log  *slog.Logger
	opts Options

	tasks     map[string]*task.Task
	next      int
	timers    map[string]*time.Timer
	timeouts  chan string
	results   chan func()
	answering map[string]bool      // tasks with an answer being delivered
	lastBeat  map[string]time.Time // last sign of life of each active task
	claims    map[string]string    // what each task being dispatched is accepted with
}

// maxResult bounds a task's result: the task.done that carries it must fit
// in one message, with room for the envelope.
const maxResult = 900 << 10

// withDefaults fills zero fields with the defaults and keeps MaxTimeout
// within Retention.
func (o Options) withDefaults() Options {
	d := DefaultOptions()
	if o.DefaultTimeout <= 0 {
		o.DefaultTimeout = d.DefaultTimeout
	}
	if o.MaxTimeout <= 0 {
		o.MaxTimeout = d.MaxTimeout
	}
	if o.DispatchTimeout <= 0 {
		o.DispatchTimeout = d.DispatchTimeout
	}
	if o.Retention <= 0 {
		o.Retention = d.Retention
	}
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = d.HeartbeatInterval
	}
	o.MaxTimeout = min(o.MaxTimeout, o.Retention)
	o.DefaultTimeout = min(o.DefaultTimeout, o.MaxTimeout)
	return o
}

// New returns a task manager publishing and serving on conn. Zero fields in
// opts take their default value.
func New(conn *bus.Conn, log *slog.Logger, opts Options) *Manager {
	opts = opts.withDefaults()
	return &Manager{
		conn:     conn,
		log:      log,
		opts:     opts,
		timeouts: make(chan string),
		results:  make(chan func()),
	}
}

// reset clears the state, so a run restarted by the supervisor rebuilds it
// from the event log like a fresh process instead of mixing in stale state.
func (m *Manager) reset() {
	m.stopTimers()
	m.tasks = make(map[string]*task.Task)
	m.next = 0
	m.timers = make(map[string]*time.Timer)
	m.answering = make(map[string]bool)
	m.lastBeat = make(map[string]time.Time)
	m.claims = make(map[string]string)
}

// Name implements supervisor.Component.
func (m *Manager) Name() string { return "tasks" }

// Run implements supervisor.Component.
func (m *Manager) Run(ctx context.Context) error {
	m.reset()
	defer m.stopTimers()

	// Only the manager publishes task events, so the log holds them all.
	if err := m.recover(ctx); err != nil {
		return err
	}

	starts, err := bus.Serve(ctx, m.conn, events.StartTask)
	if err != nil {
		return err
	}
	cancels, err := bus.Serve(ctx, m.conn, events.CancelTask)
	if err != nil {
		return err
	}
	directCancels, err := bus.ServeUnder(ctx, m.conn, events.CancelTaskFrom)
	if err != nil {
		return err
	}
	answers, err := bus.Serve(ctx, m.conn, events.AnswerTask)
	if err != nil {
		return err
	}
	lists, err := bus.Serve(ctx, m.conn, events.ListTasks)
	if err != nil {
		return err
	}
	gets, err := bus.Serve(ctx, m.conn, events.GetTask)
	if err != nil {
		return err
	}
	// Every worker reports under its own name, which the bus enforces.
	accepts, err := bus.ServeUnder(ctx, m.conn, events.WorkerAccept)
	if err != nil {
		return err
	}
	progresses, err := bus.ServeUnder(ctx, m.conn, events.WorkerProgress)
	if err != nil {
		return err
	}
	asks, err := bus.ServeUnder(ctx, m.conn, events.WorkerAsk)
	if err != nil {
		return err
	}
	dones, err := bus.ServeUnder(ctx, m.conn, events.WorkerDone)
	if err != nil {
		return err
	}
	failures, err := bus.ServeUnder(ctx, m.conn, events.WorkerFailed)
	if err != nil {
		return err
	}
	beats, err := bus.SubscribeSignalUnder(ctx, m.conn, events.TaskAlive)
	if err != nil {
		return err
	}
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	liveness := time.NewTicker(m.opts.HeartbeatInterval)
	defer liveness.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case in, ok := <-starts:
			if !ok {
				return ctx.Err()
			}
			m.start(ctx, in)
		case in, ok := <-cancels:
			if !ok {
				return ctx.Err()
			}
			m.cancel(ctx, in, "")
		case in, ok := <-directCancels:
			if !ok {
				return ctx.Err()
			}
			if !events.ValidChannel(in.Under()) {
				in.Fail(fmt.Errorf("invalid channel %q", in.Under()))
				continue
			}
			m.cancel(ctx, in, in.Under())
		case in, ok := <-answers:
			if !ok {
				return ctx.Err()
			}
			m.answer(ctx, in)
		case in, ok := <-lists:
			if !ok {
				return ctx.Err()
			}
			in.Reply(m.list(in.Req))
		case in, ok := <-gets:
			if !ok {
				return ctx.Err()
			}
			if t, err := m.lookup(in.Req.TaskID); err != nil {
				in.Fail(err)
			} else {
				in.Reply(t.Info())
			}
		case in, ok := <-accepts:
			if !ok {
				return ctx.Err()
			}
			m.accept(ctx, in)
		case in, ok := <-progresses:
			if !ok {
				return ctx.Err()
			}
			m.progress(ctx, in)
		case in, ok := <-asks:
			if !ok {
				return ctx.Err()
			}
			m.ask(ctx, in)
		case in, ok := <-dones:
			if !ok {
				return ctx.Err()
			}
			m.done(ctx, in)
		case in, ok := <-failures:
			if !ok {
				return ctx.Err()
			}
			m.failed(ctx, in)
		case beat, ok := <-beats:
			if !ok {
				return ctx.Err()
			}
			if t, ok := m.tasks[beat.Data.TaskID]; ok && t.Info().Worker == beat.Under {
				m.alive(t.ID())
			}
		case <-liveness.C:
			m.checkLiveness(ctx)
		case id := <-m.timeouts:
			m.timeout(ctx, id)
		case fn := <-m.results:
			fn()
		case <-prune.C:
			m.prune()
		}
	}
}

// recover rebuilds the state from the event log and fails every task that
// was still in progress, since its worker cannot be trusted to still run it.
func (m *Manager) recover(ctx context.Context) error {
	history, err := bus.History(ctx, m.conn, time.Now().Add(-m.opts.Retention), "task.>")
	if err != nil {
		return fmt.Errorf("tasks: recover: %w", err)
	}
	for _, env := range history {
		m.apply(env)
	}
	// IDs must never be reused, even when the last task is older than the
	// recovery window: continue from the newest task in the whole log.
	last, ok, err := bus.LastEvent(ctx, m.conn, events.TaskCreated)
	if err != nil {
		return fmt.Errorf("tasks: recover: %w", err)
	}
	if n, valid := idNumber(last.Data.ID); ok && valid && n > m.next {
		m.next = n
	}
	interrupted := 0
	for _, t := range m.sorted() {
		if t.Status().Terminal() {
			continue
		}
		m.fail(ctx, t, "interrupted: DOGGIE restarted")
		// The worker may still be running it (e.g. only the manager
		// restarted): ask it to stop.
		m.controlWorker(ctx, t, events.TaskControl{Action: events.ControlCancel, Reason: "interrupted"})
		interrupted++
	}
	m.log.Info("tasks recovered", "tasks", len(m.tasks), "interrupted", interrupted, "next_id", m.next+1)
	return nil
}

// apply folds one logged event into the state.
func (m *Manager) apply(env bus.Envelope) {
	if env.Type == events.TaskCreated.Type() {
		ev, err := bus.Decode(env, events.TaskCreated)
		if err != nil {
			m.log.Warn("ignoring task event", "type", env.Type, "err", err)
			return
		}
		if _, exists := m.tasks[ev.Data.ID]; exists {
			return
		}
		t := task.FromInfo(ev.Data)
		m.tasks[t.ID()] = t
		if n, ok := idNumber(t.ID()); ok && n > m.next {
			m.next = n
		}
		return
	}

	id, err := task.EventTaskID(env)
	if err != nil {
		m.log.Warn("ignoring task event", "type", env.Type, "err", err)
		return
	}
	t, ok := m.tasks[id]
	if !ok {
		m.log.Debug("ignoring event for unknown task", "type", env.Type, "task", id)
		return
	}
	if err := t.Apply(env); err != nil {
		// An event that doesn't fit the task's state changes nothing.
		m.log.Debug("ignoring task event", "type", env.Type, "task", id, "err", err)
	}
}

func (m *Manager) start(ctx context.Context, in bus.Incoming[events.TaskSpec, events.TaskRef]) {
	spec := in.Req
	if !bus.ValidName(spec.Kind) {
		in.Fail(fmt.Errorf("invalid task kind %q", spec.Kind))
		return
	}
	if spec.ParentID != "" {
		parent, err := m.lookup(spec.ParentID)
		if err != nil {
			in.Fail(err)
			return
		}
		if parent.Status().Terminal() {
			in.Fail(fmt.Errorf("parent task %s is already %s", parent.ID(), parent.Status()))
			return
		}
	}
	timeout := m.opts.DefaultTimeout
	switch maxS := int64(m.opts.MaxTimeout / time.Second); {
	case spec.TimeoutS < 0:
		in.Fail(errors.New("timeout_s must not be negative"))
		return
	case int64(spec.TimeoutS) > maxS:
		in.Fail(fmt.Errorf("timeout_s must be at most %d", maxS))
		return
	case spec.TimeoutS > 0:
		timeout = time.Duration(spec.TimeoutS) * time.Second
	}

	m.next++
	id := "t" + strconv.Itoa(m.next)
	now := time.Now().UTC()
	t := task.New(id, spec, now, now.Add(timeout))
	m.tasks[id] = t
	logged := t.Info()
	if logged.Sensitive {
		logged.Input = nil // the worker gets it in the run request, never logged
	}
	publish(ctx, m, events.TaskCreated, logged)
	m.armTimer(ctx, id, timeout)

	info := t.Info()
	info.HeartbeatMS = int(m.opts.HeartbeatInterval / time.Millisecond)
	// Only a worker that got the run request can accept the task: task IDs
	// are easy to guess.
	info.Claim = newClaim()
	m.claims[id] = info.Claim
	go func() {
		rctx, cancel := context.WithTimeout(ctx, m.opts.DispatchTimeout)
		defer cancel()
		_, err := bus.Request(rctx, m.conn, events.RunTask.With(spec.Kind), info)
		m.do(ctx, func() { m.dispatched(ctx, in, id, err) })
	}()
}

// dispatched handles the worker's answer to a run request. A worker
// accepts the task (WorkerAccept) before it answers, so by now the task
// runs, unless it ended meanwhile.
func (m *Manager) dispatched(ctx context.Context, in bus.Incoming[events.TaskSpec, events.TaskRef], id string, err error) {
	delete(m.claims, id)
	t, ok := m.tasks[id]
	if !ok { // a callback from before a restart
		in.Fail(fmt.Errorf("unknown task %q", id))
		return
	}
	if t.Status().Terminal() {
		// Cancelled or timed out meanwhile, and so refused to its worker:
		// it exists, and its events tell how it ended.
		in.Reply(events.TaskRef{TaskID: id})
		return
	}
	reason := ""
	switch {
	case errors.Is(err, bus.ErrNoHandler):
		reason = fmt.Sprintf("no worker for task kind %q", t.Info().Kind)
	case err != nil:
		reason = fmt.Sprintf("dispatch failed: %v", err)
		// The worker may have accepted the task and only the reply got
		// lost: make sure it doesn't keep running an orphan.
		m.controlWorker(ctx, t, events.TaskControl{Action: events.ControlCancel, Reason: reason})
	case t.Status() == events.StatusPending:
		reason = "the worker didn't accept the task"
	}
	if reason == "" {
		in.Reply(events.TaskRef{TaskID: id})
		return
	}
	if !t.Status().Terminal() {
		m.fail(ctx, t, reason)
	}
	in.Fail(errors.New(reason))
}

// accept gives a task to the worker that received its run request.
func (m *Manager) accept(ctx context.Context, in bus.Incoming[events.TaskClaim, events.Ack]) {
	worker := in.Under()
	t, err := m.lookup(in.Req.TaskID)
	claim, dispatching := m.claims[in.Req.TaskID]
	switch {
	case err != nil:
		in.Fail(err)
	case !dispatching || subtle.ConstantTimeCompare([]byte(claim), []byte(in.Req.Claim)) != 1:
		in.Fail(fmt.Errorf("task %s: no such run request", t.ID()))
	case t.Status() != events.StatusPending:
		// Cancelled or timed out meanwhile, or taken by another worker.
		in.Fail(fmt.Errorf("task %s is %s", t.ID(), t.Status()))
	default:
		if err := t.Accept(worker, time.Now().UTC()); err != nil {
			in.Fail(err)
			return
		}
		delete(m.claims, t.ID())
		publish(ctx, m, events.TaskRunning, events.TaskRunningData{TaskID: t.ID(), Worker: worker})
		m.alive(t.ID())
		in.Reply(events.Ack{})
	}
}

// reporting returns the task a worker reports on, if it is the one running
// it. A report on a task waiting for an answer that is being delivered means
// the worker got the answer and goes on, though the manager hasn't heard
// back from it yet: the task runs again.
func (m *Manager) reporting(ctx context.Context, worker, id string) (*task.Task, error) {
	t, err := m.lookup(id)
	switch {
	case err != nil:
		return nil, err
	case t.Info().Worker != worker:
		return nil, fmt.Errorf("task %s is not run by %s", id, worker)
	case t.Status().Terminal():
		return nil, fmt.Errorf("task %s is already %s", id, t.Status())
	}
	if t.Status() == events.StatusWaitingInput && m.answering[id] {
		if err := t.Start(time.Now().UTC()); err == nil {
			publish(ctx, m, events.TaskRunning, events.TaskRunningData{TaskID: id})
		}
	}
	m.alive(id)
	return t, nil
}

func (m *Manager) progress(ctx context.Context, in bus.Incoming[events.TaskProgressData, events.Ack]) {
	t, err := m.reporting(ctx, in.Under(), in.Req.TaskID)
	if err == nil {
		err = t.Progress(in.Req.Message, in.Req.Percent, time.Now().UTC())
	}
	if err != nil {
		in.Fail(err)
		return
	}
	info := t.Info()
	publish(ctx, m, events.TaskProgress, events.TaskProgressData{TaskID: info.ID, Message: info.Progress, Percent: info.Percent})
	in.Reply(events.Ack{})
}

func (m *Manager) ask(ctx context.Context, in bus.Incoming[events.TaskNeedsInputData, events.Ack]) {
	t, err := m.reporting(ctx, in.Under(), in.Req.TaskID)
	if err == nil {
		err = t.Ask(in.Req.Question, in.Req.Options, time.Now().UTC())
	}
	if err != nil {
		in.Fail(err)
		return
	}
	publish(ctx, m, events.TaskNeedsInput, events.TaskNeedsInputData{TaskID: t.ID(), Question: in.Req.Question, Options: in.Req.Options})
	in.Reply(events.Ack{})
}

func (m *Manager) done(ctx context.Context, in bus.Incoming[events.TaskDoneData, events.Ack]) {
	if len(in.Req.Result) > maxResult {
		in.Fail(fmt.Errorf("result too large: %d bytes, at most %d", len(in.Req.Result), maxResult))
		return
	}
	t, err := m.reporting(ctx, in.Under(), in.Req.TaskID)
	if err == nil {
		err = t.Finish(in.Req.Result, time.Now().UTC())
	}
	if err != nil {
		in.Fail(err)
		return
	}
	logged := events.TaskDoneData{TaskID: t.ID(), Result: in.Req.Result}
	if t.Info().Sensitive {
		// Kept in memory only, so it is gone after a restart.
		logged = events.TaskDoneData{TaskID: t.ID(), Redacted: true}
	}
	publish(ctx, m, events.TaskDone, logged)
	m.finished(ctx, t, "")
	in.Reply(events.Ack{})
}

func (m *Manager) failed(ctx context.Context, in bus.Incoming[events.TaskFailedData, events.Ack]) {
	t, err := m.reporting(ctx, in.Under(), in.Req.TaskID)
	if err != nil {
		in.Fail(err)
		return
	}
	reason := in.Req.Error
	if reason == "" {
		reason = "failed"
	}
	m.fail(ctx, t, reason)
	in.Reply(events.Ack{})
}

// newClaim returns a secret that can't be guessed.
func newClaim() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// cancel serves a cancellation. channel is where a person asked for it
// directly, if they did.
func (m *Manager) cancel(ctx context.Context, in bus.Incoming[events.TaskCancelRequest, events.Ack], channel string) {
	t, err := m.lookup(in.Req.TaskID)
	if err != nil {
		in.Fail(err)
		return
	}
	if t.Status().Terminal() {
		in.Fail(fmt.Errorf("task %s is already %s", t.ID(), t.Status()))
		return
	}
	reason := in.Req.Reason
	if reason == "" {
		reason = "cancelled by request"
	}
	m.cancelTask(ctx, t, reason, channel)
	in.Reply(events.Ack{})
}

// cancelTask records the cancellation at once and tells the worker to stop,
// best effort: a worker that is gone can't run the task anyway. channel is
// where a person asked for it directly, if they did.
func (m *Manager) cancelTask(ctx context.Context, t *task.Task, reason, channel string) {
	if err := t.Cancel(reason, time.Now().UTC()); err != nil {
		return
	}
	publish(ctx, m, events.TaskCancelled, events.TaskCancelledData{TaskID: t.ID(), Reason: reason, Channel: channel})
	m.controlWorker(ctx, t, events.TaskControl{Action: events.ControlCancel, Reason: reason})
	m.finished(ctx, t, channel)
}

func (m *Manager) answer(ctx context.Context, in bus.Incoming[events.TaskAnswer, events.Ack]) {
	t, err := m.lookup(in.Req.TaskID)
	if err != nil {
		in.Fail(err)
		return
	}
	if t.Status() != events.StatusWaitingInput {
		in.Fail(fmt.Errorf("task %s is not waiting for input (it is %s)", t.ID(), t.Status()))
		return
	}
	id := t.ID()
	// One answer at a time: a second one could otherwise reach the worker as
	// the answer to its next question.
	if m.answering[id] {
		in.Fail(fmt.Errorf("an answer for task %s is already being delivered", id))
		return
	}
	m.answering[id] = true
	askedAt, worker := t.Info().UpdatedAt, t.Info().Worker
	go func() {
		rctx, cancel := context.WithTimeout(ctx, controlTimeout)
		defer cancel()
		_, err := bus.Request(rctx, m.conn, events.ControlTask.With(worker, id), events.TaskControl{
			Action: events.ControlAnswer, Answer: in.Req.Answer,
		})
		m.do(ctx, func() { m.answered(ctx, in, id, askedAt, err) })
	}()
}

// answered handles the worker's reply to a delivered answer.
func (m *Manager) answered(ctx context.Context, in bus.Incoming[events.TaskAnswer, events.Ack], id string, askedAt time.Time, err error) {
	delete(m.answering, id)
	t, ok := m.tasks[id]
	switch {
	case !ok: // a callback from before a restart
		in.Fail(fmt.Errorf("unknown task %q", id))
	case errors.Is(err, bus.ErrNoHandler):
		// Nobody runs the task anymore, so nobody will ever answer it.
		if !t.Status().Terminal() {
			m.fail(ctx, t, "worker is gone: the answer could not be delivered")
		}
		in.Fail(fmt.Errorf("task %s: its worker is gone", id))
	case err != nil:
		in.Fail(fmt.Errorf("deliver answer to %s: %w", id, err))
	case t.Status() == events.StatusFailed || t.Status() == events.StatusCancelled:
		// It ended (e.g. timed out) before the answer could matter. A done
		// task is fine: the answer was delivered and let it finish.
		in.Fail(fmt.Errorf("task %s is already %s", id, t.Status()))
	default:
		// Mark it running only if it still waits on the question we
		// answered: the worker may already have moved on and asked the
		// next one, which must stay visible.
		if t.Status() == events.StatusWaitingInput && t.Info().UpdatedAt.Equal(askedAt) {
			if err := t.Start(time.Now().UTC()); err == nil {
				publish(ctx, m, events.TaskRunning, events.TaskRunningData{TaskID: id})
			}
		}
		in.Reply(events.Ack{})
	}
}

// alive records a sign of life for an active task.
func (m *Manager) alive(id string) {
	if t, ok := m.tasks[id]; ok && !t.Status().Terminal() {
		m.lastBeat[id] = time.Now()
	}
}

// checkLiveness fails the active tasks whose worker stopped beating: it died
// without being able to report (crash, power loss, network cut).
func (m *Manager) checkLiveness(ctx context.Context) {
	limit := heartbeatTolerance * m.opts.HeartbeatInterval
	for _, t := range m.sorted() {
		last, ok := m.lastBeat[t.ID()]
		if !ok || t.Status().Terminal() || time.Since(last) <= limit {
			continue
		}
		m.fail(ctx, t, fmt.Sprintf("worker stopped responding: no heartbeat for %s", time.Since(last).Round(time.Millisecond)))
		m.controlWorker(ctx, t, events.TaskControl{Action: events.ControlCancel, Reason: "no heartbeat"})
	}
}

func (m *Manager) timeout(ctx context.Context, id string) {
	t, ok := m.tasks[id]
	if !ok || t.Status().Terminal() {
		return
	}
	info := t.Info()
	m.fail(ctx, t, fmt.Sprintf("timed out after %s", info.Deadline.Sub(info.CreatedAt).Round(time.Second)))
	m.controlWorker(ctx, t, events.TaskControl{Action: events.ControlCancel, Reason: "timed out"})
}

func (m *Manager) fail(ctx context.Context, t *task.Task, reason string) {
	if err := t.Fail(reason, time.Now().UTC()); err != nil {
		return
	}
	publish(ctx, m, events.TaskFailed, events.TaskFailedData{TaskID: t.ID(), Error: reason})
	m.finished(ctx, t, "")
}

// finished cleans up after a task ended and cancels its active subtasks if
// it did not succeed. channel is where a person cancelled it directly, if
// they did.
func (m *Manager) finished(ctx context.Context, t *task.Task, channel string) {
	if timer, ok := m.timers[t.ID()]; ok {
		timer.Stop()
		delete(m.timers, t.ID())
	}
	delete(m.lastBeat, t.ID())
	if t.Status() == events.StatusDone {
		return
	}
	for _, child := range m.sorted() {
		if child.Info().ParentID == t.ID() && !child.Status().Terminal() {
			m.cancelTask(ctx, child, fmt.Sprintf("parent task %s %s", t.ID(), t.Status()), channel)
		}
	}
}

// controlWorker sends a control command to the worker running t without
// blocking the loop. Missing workers are expected (e.g. after a restart). A
// task no worker accepted has nobody to tell.
func (m *Manager) controlWorker(ctx context.Context, t *task.Task, c events.TaskControl) {
	id, worker := t.ID(), t.Info().Worker
	if worker == "" {
		return
	}
	go func() {
		rctx, cancel := context.WithTimeout(ctx, controlTimeout)
		defer cancel()
		_, err := bus.Request(rctx, m.conn, events.ControlTask.With(worker, id), c)
		if err != nil && !errors.Is(err, bus.ErrNoHandler) && ctx.Err() == nil {
			m.log.Debug("control worker", "task", id, "action", c.Action, "err", err)
		}
	}()
}

func (m *Manager) armTimer(ctx context.Context, id string, d time.Duration) {
	m.timers[id] = time.AfterFunc(d, func() {
		select {
		case m.timeouts <- id:
		case <-ctx.Done():
		}
	})
}

func (m *Manager) stopTimers() {
	for id, timer := range m.timers {
		timer.Stop()
		delete(m.timers, id)
	}
}

// do runs fn in the actor loop.
func (m *Manager) do(ctx context.Context, fn func()) {
	select {
	case m.results <- fn:
	case <-ctx.Done():
	}
}

// publish logs failures instead of returning them: the state change already
// happened, and an unlogged event only costs observers a notification.
func publish[T any](ctx context.Context, m *Manager, s bus.Subject[T], data T) {
	if _, err := bus.Publish(ctx, m.conn, s, data); err != nil && ctx.Err() == nil {
		m.log.Warn("publish task event", "type", s.Type(), "err", err)
	}
}

func (m *Manager) lookup(id string) (*task.Task, error) {
	t, ok := m.tasks[id]
	if !ok {
		return nil, fmt.Errorf("unknown task %q", id)
	}
	return t, nil
}

// list returns the most recent matching tasks, oldest first.
func (m *Manager) list(req events.TaskListRequest) events.TaskList {
	limit := req.Limit
	if limit <= 0 {
		limit = defaultListLimit
	}
	limit = min(limit, maxListLimit)
	out := events.TaskList{Tasks: []events.TaskInfo{}}
	for _, t := range m.sorted() {
		if req.ActiveOnly && t.Status().Terminal() {
			continue
		}
		out.Tasks = append(out.Tasks, t.Info())
	}
	out.Total = len(out.Tasks)
	if len(out.Tasks) > limit {
		out.Tasks = out.Tasks[len(out.Tasks)-limit:]
	}
	return out
}

// prune forgets finished tasks older than the retention period.
func (m *Manager) prune() {
	cutoff := time.Now().Add(-m.opts.Retention)
	for id, t := range m.tasks {
		if t.Status().Terminal() && t.Info().UpdatedAt.Before(cutoff) {
			delete(m.tasks, id)
		}
	}
}

// sorted returns the tasks in creation order.
func (m *Manager) sorted() []*task.Task {
	out := make([]*task.Task, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b *task.Task) int {
		na, _ := idNumber(a.ID())
		nb, _ := idNumber(b.ID())
		return na - nb
	})
	return out
}

// idNumber parses the number of a task ID such as "t7".
func idNumber(id string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "t"))
	return n, err == nil && strings.HasPrefix(id, "t")
}
