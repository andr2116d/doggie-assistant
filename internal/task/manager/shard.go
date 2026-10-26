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
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/task"
)

// shard is an actor that owns some of the tasks: a task and its subtasks
// always belong to the same shard, so a cancellation cascades within it, and
// every event of a task is published in the order the shard applied it.
// Shards run apart, so a busy one doesn't hold the others up.
type shard struct {
	m    *Manager
	conn *bus.Conn
	log  *slog.Logger
	opts Options

	tasks     map[string]*task.Task
	timers    map[string]*time.Timer
	timeouts  chan string
	inbox     chan func()          // work for the shard: requests and outcomes
	answering map[string]bool      // tasks with an answer being delivered
	lastBeat  map[string]time.Time // last sign of life of each active task
	claims    map[string]string    // what each task being dispatched is accepted with
}

// inboxSize lets the manager hand a shard work without waiting for it.
const inboxSize = 256

func newShard(m *Manager) *shard {
	return &shard{
		m: m, conn: m.conn, log: m.log, opts: m.opts,
		tasks:     make(map[string]*task.Task),
		timers:    make(map[string]*time.Timer),
		timeouts:  make(chan string),
		inbox:     make(chan func(), inboxSize),
		answering: make(map[string]bool),
		lastBeat:  make(map[string]time.Time),
		claims:    make(map[string]string),
	}
}

// run rebuilds the shard's tasks from their logged events, fails the ones a
// restart interrupted, tells ready how many, and then serves until ctx is
// cancelled.
func (s *shard) run(ctx context.Context, logged []bus.Envelope, ready func(tasks, interrupted int)) {
	defer s.stopTimers()
	for _, env := range logged {
		s.apply(env)
	}
	interrupted := 0
	for _, t := range s.sorted() {
		if t.Status().Terminal() {
			continue
		}
		s.fail(ctx, t, "interrupted: DOGGIE restarted")
		// The worker may still be running it (e.g. only the manager
		// restarted): ask it to stop.
		s.controlWorker(ctx, t, events.TaskControl{Action: events.ControlCancel, Reason: "interrupted"})
		interrupted++
	}
	ready(len(s.tasks), interrupted)

	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	liveness := time.NewTicker(s.opts.HeartbeatInterval)
	defer liveness.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case fn := <-s.inbox:
			fn()
		case <-liveness.C:
			s.checkLiveness(ctx)
		case id := <-s.timeouts:
			s.timeout(ctx, id)
		case <-prune.C:
			s.prune()
		}
	}
}

// beat records a heartbeat, if it comes from the task's worker.
func (s *shard) beat(id, worker string) {
	if t, ok := s.tasks[id]; ok && t.Info().Worker == worker {
		s.alive(id)
	}
}

// get answers task.get.
func (s *shard) get(in bus.Incoming[events.TaskRef, events.TaskInfo]) {
	if t, err := s.lookup(in.Req.TaskID); err != nil {
		in.Fail(err)
	} else {
		in.Reply(t.Info())
	}
}

// infos returns a snapshot of the shard's tasks.
func (s *shard) infos(activeOnly bool) []events.TaskInfo {
	var out []events.TaskInfo
	for _, t := range s.tasks {
		if !activeOnly || !t.Status().Terminal() {
			out = append(out, t.Info())
		}
	}
	return out
}

// apply folds one logged event into the state.
func (s *shard) apply(env bus.Envelope) {
	if env.Type == events.TaskCreated.Type() {
		ev, err := bus.Decode(env, events.TaskCreated)
		if err != nil {
			s.log.Warn("ignoring task event", "type", env.Type, "err", err)
			return
		}
		if _, exists := s.tasks[ev.Data.ID]; exists {
			return
		}
		t := task.FromInfo(ev.Data)
		s.tasks[t.ID()] = t
		return
	}

	id, err := task.EventTaskID(env)
	if err != nil {
		s.log.Warn("ignoring task event", "type", env.Type, "err", err)
		return
	}
	t, ok := s.tasks[id]
	if !ok {
		s.log.Debug("ignoring event for unknown task", "type", env.Type, "task", id)
		return
	}
	if err := t.Apply(env); err != nil {
		// An event that doesn't fit the task's state changes nothing.
		s.log.Debug("ignoring task event", "type", env.Type, "task", id, "err", err)
	}
}

// start creates task id. The manager routed it here, with the parent if it
// has one; a task that can't start is forgotten.
func (s *shard) start(ctx context.Context, in bus.Incoming[events.TaskSpec, events.TaskRef], id string) {
	spec := in.Req
	refuse := func(err error) {
		s.m.forget(id)
		in.Fail(err)
	}
	if !bus.ValidName(spec.Kind) {
		refuse(fmt.Errorf("invalid task kind %q", spec.Kind))
		return
	}
	if spec.ParentID != "" {
		parent, err := s.lookup(spec.ParentID)
		if err != nil {
			refuse(err)
			return
		}
		if parent.Status().Terminal() {
			refuse(fmt.Errorf("parent task %s is already %s", parent.ID(), parent.Status()))
			return
		}
	}
	timeout := s.opts.DefaultTimeout
	switch maxS := int64(s.opts.MaxTimeout / time.Second); {
	case spec.TimeoutS < 0:
		refuse(errors.New("timeout_s must not be negative"))
		return
	case int64(spec.TimeoutS) > maxS:
		refuse(fmt.Errorf("timeout_s must be at most %d", maxS))
		return
	case spec.TimeoutS > 0:
		timeout = time.Duration(spec.TimeoutS) * time.Second
	}

	now := time.Now().UTC()
	t := task.New(id, spec, now, now.Add(timeout))
	s.tasks[id] = t
	logged := t.Info()
	if logged.Sensitive {
		logged.Input = nil // the worker gets it in the run request, never logged
	}
	publish(ctx, s, events.TaskCreated, logged)
	s.armTimer(ctx, id, timeout)

	info := t.Info()
	info.HeartbeatMS = int(s.opts.HeartbeatInterval / time.Millisecond)
	// Only a worker that got the run request can accept the task: task IDs
	// are easy to guess.
	info.Claim = newClaim()
	s.claims[id] = info.Claim
	go func() {
		rctx, cancel := context.WithTimeout(ctx, s.opts.DispatchTimeout)
		defer cancel()
		_, err := bus.Request(rctx, s.conn, events.RunTask.With(spec.Kind), info)
		s.do(ctx, func() { s.dispatched(ctx, in, id, err) })
	}()
}

// dispatched handles the worker's answer to a run request. A worker
// accepts the task (WorkerAccept) before it answers, so by now the task
// runs, unless it ended meanwhile.
func (s *shard) dispatched(ctx context.Context, in bus.Incoming[events.TaskSpec, events.TaskRef], id string, err error) {
	delete(s.claims, id)
	t, ok := s.tasks[id]
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
		s.controlWorker(ctx, t, events.TaskControl{Action: events.ControlCancel, Reason: reason})
	case t.Status() == events.StatusPending:
		reason = "the worker didn't accept the task"
	}
	if reason == "" {
		in.Reply(events.TaskRef{TaskID: id})
		return
	}
	if !t.Status().Terminal() {
		s.fail(ctx, t, reason)
	}
	in.Fail(errors.New(reason))
}

// accept gives a task to the worker that received its run request.
func (s *shard) accept(ctx context.Context, in bus.Incoming[events.TaskClaim, events.Ack]) {
	worker := in.Under()
	t, err := s.lookup(in.Req.TaskID)
	claim, dispatching := s.claims[in.Req.TaskID]
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
		delete(s.claims, t.ID())
		publish(ctx, s, events.TaskRunning, events.TaskRunningData{TaskID: t.ID(), Worker: worker})
		s.alive(t.ID())
		in.Reply(events.Ack{})
	}
}

// reporting returns the task a worker reports on, if it is the one running
// it. A report on a task waiting for an answer that is being delivered means
// the worker got the answer and goes on, though the manager hasn't heard
// back from it yet: the task runs again.
func (s *shard) reporting(ctx context.Context, worker, id string) (*task.Task, error) {
	t, err := s.lookup(id)
	switch {
	case err != nil:
		return nil, err
	case t.Info().Worker != worker:
		return nil, fmt.Errorf("task %s is not run by %s", id, worker)
	case t.Status().Terminal():
		return nil, fmt.Errorf("task %s is already %s", id, t.Status())
	}
	if t.Status() == events.StatusWaitingInput && s.answering[id] {
		if err := t.Start(time.Now().UTC()); err == nil {
			publish(ctx, s, events.TaskRunning, events.TaskRunningData{TaskID: id})
		}
	}
	s.alive(id)
	return t, nil
}

func (s *shard) progress(ctx context.Context, in bus.Incoming[events.TaskProgressData, events.Ack]) {
	t, err := s.reporting(ctx, in.Under(), in.Req.TaskID)
	if err == nil {
		err = t.Progress(in.Req.Message, in.Req.Percent, time.Now().UTC())
	}
	if err != nil {
		in.Fail(err)
		return
	}
	info := t.Info()
	logged := events.TaskProgressData{TaskID: info.ID, Message: info.Progress, Percent: info.Percent}
	if info.Sensitive {
		logged = events.TaskProgressData{TaskID: info.ID, Percent: info.Percent, Redacted: true}
	}
	publish(ctx, s, events.TaskProgress, logged)
	in.Reply(events.Ack{})
}

func (s *shard) ask(ctx context.Context, in bus.Incoming[events.TaskNeedsInputData, events.Ack]) {
	t, err := s.reporting(ctx, in.Under(), in.Req.TaskID)
	if err == nil {
		err = t.Ask(in.Req.Question, in.Req.Options, time.Now().UTC())
	}
	if err != nil {
		in.Fail(err)
		return
	}
	logged := events.TaskNeedsInputData{TaskID: t.ID(), Question: in.Req.Question, Options: in.Req.Options}
	if t.Info().Sensitive {
		logged = events.TaskNeedsInputData{TaskID: t.ID(), Redacted: true}
	}
	publish(ctx, s, events.TaskNeedsInput, logged)
	in.Reply(events.Ack{})
}

func (s *shard) done(ctx context.Context, in bus.Incoming[events.TaskDoneData, events.Ack]) {
	if len(in.Req.Result) > maxResult {
		in.Fail(fmt.Errorf("result too large: %d bytes, at most %d", len(in.Req.Result), maxResult))
		return
	}
	t, err := s.reporting(ctx, in.Under(), in.Req.TaskID)
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
	publish(ctx, s, events.TaskDone, logged)
	s.finished(ctx, t, "")
	in.Reply(events.Ack{})
}

func (s *shard) failed(ctx context.Context, in bus.Incoming[events.TaskFailedData, events.Ack]) {
	t, err := s.reporting(ctx, in.Under(), in.Req.TaskID)
	if err != nil {
		in.Fail(err)
		return
	}
	reason := in.Req.Error
	if reason == "" {
		reason = "failed"
	}
	s.fail(ctx, t, reason)
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
func (s *shard) cancel(ctx context.Context, in bus.Incoming[events.TaskCancelRequest, events.Ack], channel string) {
	t, err := s.lookup(in.Req.TaskID)
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
	s.cancelTask(ctx, t, reason, channel)
	in.Reply(events.Ack{})
}

// cancelTask records the cancellation at once and tells the worker to stop,
// best effort: a worker that is gone can't run the task anyway. channel is
// where a person asked for it directly, if they did.
func (s *shard) cancelTask(ctx context.Context, t *task.Task, reason, channel string) {
	if err := t.Cancel(reason, time.Now().UTC()); err != nil {
		return
	}
	publish(ctx, s, events.TaskCancelled, events.TaskCancelledData{TaskID: t.ID(), Reason: reason, Channel: channel})
	s.controlWorker(ctx, t, events.TaskControl{Action: events.ControlCancel, Reason: reason})
	s.finished(ctx, t, channel)
}

func (s *shard) answer(ctx context.Context, in bus.Incoming[events.TaskAnswer, events.Ack]) {
	t, err := s.lookup(in.Req.TaskID)
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
	if s.answering[id] {
		in.Fail(fmt.Errorf("an answer for task %s is already being delivered", id))
		return
	}
	s.answering[id] = true
	askedAt, worker := t.Info().UpdatedAt, t.Info().Worker
	go func() {
		rctx, cancel := context.WithTimeout(ctx, controlTimeout)
		defer cancel()
		_, err := bus.Request(rctx, s.conn, events.ControlTask.With(worker, id), events.TaskControl{
			Action: events.ControlAnswer, Answer: in.Req.Answer,
		})
		s.do(ctx, func() { s.answered(ctx, in, id, askedAt, err) })
	}()
}

// answered handles the worker's reply to a delivered answer.
func (s *shard) answered(ctx context.Context, in bus.Incoming[events.TaskAnswer, events.Ack], id string, askedAt time.Time, err error) {
	delete(s.answering, id)
	t, ok := s.tasks[id]
	switch {
	case !ok: // a callback from before a restart
		in.Fail(fmt.Errorf("unknown task %q", id))
	case errors.Is(err, bus.ErrNoHandler):
		// Nobody runs the task anymore, so nobody will ever answer it.
		if !t.Status().Terminal() {
			s.fail(ctx, t, "worker is gone: the answer could not be delivered")
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
				publish(ctx, s, events.TaskRunning, events.TaskRunningData{TaskID: id})
			}
		}
		in.Reply(events.Ack{})
	}
}

// alive records a sign of life for an active task.
func (s *shard) alive(id string) {
	if t, ok := s.tasks[id]; ok && !t.Status().Terminal() {
		s.lastBeat[id] = time.Now()
	}
}

// checkLiveness fails the active tasks whose worker stopped beating: it died
// without being able to report (crash, power loss, network cut).
func (s *shard) checkLiveness(ctx context.Context) {
	limit := heartbeatTolerance * s.opts.HeartbeatInterval
	for _, t := range s.sorted() {
		last, ok := s.lastBeat[t.ID()]
		if !ok || t.Status().Terminal() || time.Since(last) <= limit {
			continue
		}
		s.fail(ctx, t, fmt.Sprintf("worker stopped responding: no heartbeat for %s", time.Since(last).Round(time.Millisecond)))
		s.controlWorker(ctx, t, events.TaskControl{Action: events.ControlCancel, Reason: "no heartbeat"})
	}
}

func (s *shard) timeout(ctx context.Context, id string) {
	t, ok := s.tasks[id]
	if !ok || t.Status().Terminal() {
		return
	}
	info := t.Info()
	s.fail(ctx, t, fmt.Sprintf("timed out after %s", info.Deadline.Sub(info.CreatedAt).Round(time.Second)))
	s.controlWorker(ctx, t, events.TaskControl{Action: events.ControlCancel, Reason: "timed out"})
}

func (s *shard) fail(ctx context.Context, t *task.Task, reason string) {
	if err := t.Fail(reason, time.Now().UTC()); err != nil {
		return
	}
	publish(ctx, s, events.TaskFailed, events.TaskFailedData{TaskID: t.ID(), Error: reason})
	s.finished(ctx, t, "")
}

// finished cleans up after a task ended and cancels its active subtasks if
// it did not succeed. channel is where a person cancelled it directly, if
// they did.
func (s *shard) finished(ctx context.Context, t *task.Task, channel string) {
	if timer, ok := s.timers[t.ID()]; ok {
		timer.Stop()
		delete(s.timers, t.ID())
	}
	delete(s.lastBeat, t.ID())
	if t.Status() == events.StatusDone {
		return
	}
	for _, child := range s.sorted() {
		if child.Info().ParentID == t.ID() && !child.Status().Terminal() {
			s.cancelTask(ctx, child, fmt.Sprintf("parent task %s %s", t.ID(), t.Status()), channel)
		}
	}
}

// controlWorker sends a control command to the worker running t without
// blocking the loop. Missing workers are expected (e.g. after a restart). A
// task no worker accepted has nobody to tell.
func (s *shard) controlWorker(ctx context.Context, t *task.Task, c events.TaskControl) {
	id, worker := t.ID(), t.Info().Worker
	if worker == "" {
		return
	}
	go func() {
		rctx, cancel := context.WithTimeout(ctx, controlTimeout)
		defer cancel()
		_, err := bus.Request(rctx, s.conn, events.ControlTask.With(worker, id), c)
		if err != nil && !errors.Is(err, bus.ErrNoHandler) && ctx.Err() == nil {
			s.log.Debug("control worker", "task", id, "action", c.Action, "err", err)
		}
	}()
}

func (s *shard) armTimer(ctx context.Context, id string, d time.Duration) {
	s.timers[id] = time.AfterFunc(d, func() {
		select {
		case s.timeouts <- id:
		case <-ctx.Done():
		}
	})
}

func (s *shard) stopTimers() {
	for id, timer := range s.timers {
		timer.Stop()
		delete(s.timers, id)
	}
}

// tryDo runs fn in the shard if it has room for it now, and reports
// whether it will.
func (s *shard) tryDo(fn func()) bool {
	select {
	case s.inbox <- fn:
		return true
	default:
		return false
	}
}

// do runs fn in the shard, unless the run is over.
func (s *shard) do(ctx context.Context, fn func()) {
	select {
	case s.inbox <- fn:
	case <-ctx.Done():
	}
}

// publish logs failures instead of returning them: the state change already
// happened, and an unlogged event only costs observers a notification.
func publish[T any](ctx context.Context, s *shard, subj bus.Subject[T], data T) {
	if _, err := bus.Publish(ctx, s.conn, subj, data); err != nil && ctx.Err() == nil {
		s.log.Warn("publish task event", "type", subj.Type(), "err", err)
	}
}

func (s *shard) lookup(id string) (*task.Task, error) {
	t, ok := s.tasks[id]
	if !ok {
		return nil, fmt.Errorf("unknown task %q", id)
	}
	return t, nil
}

// prune forgets finished tasks older than the retention period.
func (s *shard) prune() {
	cutoff := time.Now().Add(-s.opts.Retention)
	for id, t := range s.tasks {
		if t.Status().Terminal() && t.Info().UpdatedAt.Before(cutoff) {
			delete(s.tasks, id)
			s.m.forget(id)
		}
	}
}

// sorted returns the tasks in creation order.
func (s *shard) sorted() []*task.Task {
	out := make([]*task.Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b *task.Task) int {
		na, _ := idNumber(a.ID())
		nb, _ := idNumber(b.ID())
		return na - nb
	})
	return out
}
