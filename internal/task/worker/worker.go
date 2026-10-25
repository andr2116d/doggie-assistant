// Package worker lets a component run tasks for the task manager.
//
// A component registers a Func per task kind and calls Serve from its Run.
// The package takes care of the protocol: accepting run requests
// (load-balanced across workers of the same kind), reporting progress,
// asking questions, honoring cancellation and publishing the result.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

// queueGroup load-balances run requests across workers of the same kind.
const queueGroup = "task-workers"

// publishTimeout bounds the final done/failed publish, which must happen even
// when the task's own context is already cancelled.
const publishTimeout = 5 * time.Second

// Func runs one task. It must return soon after ctx is cancelled (the task
// was cancelled, timed out, or DOGGIE is stopping). Its result is encoded as
// JSON. Returning an error fails the task.
type Func func(ctx context.Context, t *Task) (any, error)

// Task is the task being run, with the means to report on it.
type Task struct {
	events.TaskInfo
	conn    *bus.Conn
	answers chan string
	asking  atomic.Bool
}

// Input decodes the task's input into v. An empty input leaves v unchanged.
func (t *Task) Input(v any) error {
	if len(t.TaskInfo.Input) == 0 {
		return nil
	}
	if err := json.Unmarshal(t.TaskInfo.Input, v); err != nil {
		return fmt.Errorf("invalid input for %s: %w", t.Kind, err)
	}
	return nil
}

// Progress reports how the task is doing. percent is 0-100, or 0 if unknown.
func (t *Task) Progress(ctx context.Context, message string, percent int) error {
	_, err := bus.Publish(ctx, t.conn, events.TaskProgress, events.TaskProgressData{
		TaskID: t.ID, Message: message, Percent: percent,
	})
	return err
}

// Ask pauses the task until its question is answered, and returns the
// answer. It fails when ctx is cancelled first.
func (t *Task) Ask(ctx context.Context, question string, options ...string) (string, error) {
	t.asking.Store(true)
	defer t.asking.Store(false)
	if _, err := bus.Publish(ctx, t.conn, events.TaskNeedsInput, events.TaskNeedsInputData{
		TaskID: t.ID, Question: question, Options: options,
	}); err != nil {
		return "", err
	}
	select {
	case a := <-t.answers:
		return a, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Spawn starts a subtask, which is cancelled if this task fails or is
// cancelled. It returns the subtask's ID.
func (t *Task) Spawn(ctx context.Context, spec events.TaskSpec) (string, error) {
	spec.ParentID = t.ID
	if spec.ThreadID == "" {
		spec.ThreadID = t.ThreadID
	}
	ref, err := bus.Request(ctx, t.conn, events.StartTask, spec)
	return ref.TaskID, err
}

// deliver hands an answer to a pending Ask.
func (t *Task) deliver(answer string) error {
	if !t.asking.Load() {
		return errors.New("task is not waiting for input")
	}
	select {
	case t.answers <- answer:
		return nil
	default:
		return errors.New("an answer is already pending")
	}
}

// Serve runs tasks of the given kinds until ctx is cancelled, then waits for
// the running tasks to stop. Tasks interrupted that way fail with "worker
// stopped".
func Serve(ctx context.Context, conn *bus.Conn, log *slog.Logger, funcs map[string]Func) error {
	wait, err := Start(ctx, conn, log, funcs)
	if err != nil {
		return err
	}
	<-ctx.Done()
	wait()
	return nil
}

// Start is Serve without blocking: it returns once every kind is being
// served, with a func that waits for the worker to stop after ctx is
// cancelled.
func Start(ctx context.Context, conn *bus.Conn, log *slog.Logger, funcs map[string]Func) (wait func(), err error) {
	for kind := range funcs {
		if !bus.ValidName(kind) {
			return nil, fmt.Errorf("worker: invalid task kind %q", kind)
		}
	}
	var wg sync.WaitGroup
	for kind, fn := range funcs {
		runs, err := bus.Serve(ctx, conn, events.RunTask.With(kind), bus.Queue(queueGroup))
		if err != nil {
			return nil, err
		}
		wg.Go(func() {
			for in := range runs {
				wg.Go(func() { run(ctx, conn, log, in, fn) })
			}
		})
	}
	return wg.Wait, nil
}

// run executes one task: it serves the task's control command (cancel,
// answer), accepts the run request, calls fn and publishes the outcome.
func run(ctx context.Context, conn *bus.Conn, log *slog.Logger, in bus.Incoming[events.TaskInfo, events.Ack], fn Func) {
	info := in.Req
	log = log.With("task", info.ID, "kind", info.Kind)

	taskCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !info.Deadline.IsZero() {
		var cancelDeadline context.CancelFunc
		taskCtx, cancelDeadline = context.WithDeadline(taskCtx, info.Deadline)
		defer cancelDeadline()
	}

	t := &Task{TaskInfo: info, conn: conn, answers: make(chan string, 1)}
	controls, err := bus.Serve(taskCtx, conn, events.ControlTask.With(info.ID))
	if err != nil {
		in.Fail(err)
		return
	}
	var cancelled atomic.Bool
	go func() {
		for c := range controls {
			switch c.Req.Action {
			case events.ControlCancel:
				cancelled.Store(true)
				cancel()
				c.Reply(events.Ack{})
			case events.ControlAnswer:
				if err := t.deliver(c.Req.Answer); err != nil {
					c.Fail(err)
					continue
				}
				c.Reply(events.Ack{})
			default:
				c.Fail(fmt.Errorf("unknown control action %q", c.Req.Action))
			}
		}
	}()

	if err := in.Reply(events.Ack{}); err != nil {
		log.Warn("accept task", "err", err)
		return
	}
	log.Debug("task accepted")
	if info.HeartbeatMS > 0 {
		go heartbeat(taskCtx, conn, info.ID, time.Duration(info.HeartbeatMS)*time.Millisecond)
	}

	result, err := safeCall(taskCtx, t, fn)
	cancel() // stops the heartbeat before the outcome is published

	pubCtx, cancelPub := context.WithTimeout(context.Background(), publishTimeout)
	defer cancelPub()
	switch {
	case cancelled.Load():
		// The manager already recorded the cancellation.
		log.Debug("task cancelled")
	case err != nil:
		reason := err.Error()
		switch {
		case ctx.Err() != nil:
			reason = "worker stopped"
		case errors.Is(taskCtx.Err(), context.DeadlineExceeded):
			reason = "timed out"
		}
		publishFailed(pubCtx, conn, log, info.ID, reason)
	default:
		raw, err := json.Marshal(result)
		if err != nil {
			publishFailed(pubCtx, conn, log, info.ID, fmt.Sprintf("encode result: %v", err))
			return
		}
		done := events.TaskDoneData{TaskID: info.ID, Result: raw}
		if info.Sensitive {
			// The result goes to the manager in a command, outside the event
			// log; the logged event only says the task is done.
			if _, err := bus.Request(pubCtx, conn, events.ReportResult, done); err != nil {
				publishFailed(pubCtx, conn, log, info.ID, fmt.Sprintf("deliver result: %v", err))
				return
			}
			done = events.TaskDoneData{TaskID: info.ID, Redacted: true}
		}
		if _, err := bus.Publish(pubCtx, conn, events.TaskDone, done); err != nil {
			// E.g. a result above the bus's max message size: fail the task
			// instead of leaving it running until its timeout.
			log.Warn("publish task done", "err", err)
			publishFailed(pubCtx, conn, log, info.ID, fmt.Sprintf("publish result (%d bytes): %v", len(raw), err))
		}
	}
}

// heartbeat tells the manager the task is still being worked on, until ctx
// is cancelled.
func heartbeat(ctx context.Context, conn *bus.Conn, id string, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			bus.Emit(conn, events.TaskAlive, events.TaskAliveData{TaskID: id})
		}
	}
}

// safeCall runs fn, turning a panic into an error so one buggy task cannot
// crash the component.
func safeCall(ctx context.Context, t *Task, fn Func) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn(ctx, t)
}

func publishFailed(ctx context.Context, conn *bus.Conn, log *slog.Logger, id, reason string) {
	if _, err := bus.Publish(ctx, conn, events.TaskFailed, events.TaskFailedData{TaskID: id, Error: reason}); err != nil {
		log.Warn("publish task failed", "err", err)
	}
}
