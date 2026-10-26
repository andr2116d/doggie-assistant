// Package task models a task's lifecycle as a state machine.
//
// The task manager applies every task event through it, both live and when
// rebuilding its state from the event log after a restart, so a task's state
// is always the fold of its events:
//
//	pending ──► running ──► done
//	              │  ▲  ├──► failed
//	              ▼  │  └──► cancelled
//	        waiting_input
//
// Any non-terminal task can fail or be cancelled. Terminal tasks never change.
package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

// ErrInvalidTransition is returned when an event does not fit the task's
// current status, e.g. a late "done" for a cancelled task.
var ErrInvalidTransition = errors.New("task: invalid transition")

var nonTerminal = []events.TaskStatus{events.StatusPending, events.StatusRunning, events.StatusWaitingInput}

// Task is a task and its current state.
type Task struct {
	info events.TaskInfo
}

// New returns a pending task.
func New(id string, spec events.TaskSpec, now, deadline time.Time) *Task {
	return &Task{info: events.TaskInfo{
		ID:        id,
		TaskSpec:  spec,
		Status:    events.StatusPending,
		CreatedAt: now,
		UpdatedAt: now,
		Deadline:  deadline,
	}}
}

// FromInfo restores a task from a snapshot, e.g. a task.created event.
func FromInfo(info events.TaskInfo) *Task { return &Task{info: info} }

// Info returns a snapshot of the task.
func (t *Task) Info() events.TaskInfo {
	info := t.info
	info.Options = slices.Clone(t.info.Options)
	return info
}

// ID returns the task ID.
func (t *Task) ID() string { return t.info.ID }

// Status returns the task's current status.
func (t *Task) Status() events.TaskStatus { return t.info.Status }

func (t *Task) transition(to events.TaskStatus, now time.Time, from ...events.TaskStatus) error {
	if !slices.Contains(from, t.info.Status) {
		return fmt.Errorf("%w: %s is %s, cannot become %s", ErrInvalidTransition, t.info.ID, t.info.Status, to)
	}
	t.info.Status = to
	t.info.UpdatedAt = now
	return nil
}

// Accept marks a pending task running on worker.
func (t *Task) Accept(worker string, now time.Time) error {
	if err := t.transition(events.StatusRunning, now, events.StatusPending); err != nil {
		return err
	}
	t.info.Worker = worker
	return nil
}

// Start marks the task running: a worker accepted it, or its question was
// answered.
func (t *Task) Start(now time.Time) error {
	if err := t.transition(events.StatusRunning, now, events.StatusPending, events.StatusWaitingInput); err != nil {
		return err
	}
	t.info.Question, t.info.Options = "", nil
	return nil
}

// Progress records how a running task is doing. The status does not change.
func (t *Task) Progress(message string, percent int, now time.Time) error {
	if err := t.transition(events.StatusRunning, now, events.StatusRunning); err != nil {
		return err
	}
	t.info.Progress, t.info.Percent = message, max(0, min(percent, 100))
	return nil
}

// Ask pauses the task until its question is answered.
func (t *Task) Ask(question string, options []string, now time.Time) error {
	if err := t.transition(events.StatusWaitingInput, now, events.StatusRunning); err != nil {
		return err
	}
	t.info.Question, t.info.Options = question, slices.Clone(options)
	return nil
}

// Finish marks the task done with its result.
func (t *Task) Finish(result json.RawMessage, now time.Time) error {
	if err := t.transition(events.StatusDone, now, events.StatusRunning); err != nil {
		return err
	}
	t.info.Result = result
	t.info.Question, t.info.Options = "", nil
	return nil
}

// Fail marks a non-terminal task failed.
func (t *Task) Fail(reason string, now time.Time) error {
	if err := t.transition(events.StatusFailed, now, nonTerminal...); err != nil {
		return err
	}
	t.info.Error = reason
	return nil
}

// Cancel marks a non-terminal task cancelled.
func (t *Task) Cancel(reason string, now time.Time) error {
	if err := t.transition(events.StatusCancelled, now, nonTerminal...); err != nil {
		return err
	}
	t.info.Error = reason
	return nil
}

// Apply applies a task event other than task.created, using the event's time.
// EventTaskID tells which task an event is about.
//
// A worker event (progress, needs_input, done) on a pending or waiting task
// implies the task (re)started: the worker can report before the manager has
// recorded that it accepted the task or received the answer.
func (t *Task) Apply(env bus.Envelope) error {
	now := env.Time
	switch env.Type {
	case events.TaskProgress.Type(), events.TaskNeedsInput.Type(), events.TaskDone.Type():
		if t.info.Status == events.StatusPending || t.info.Status == events.StatusWaitingInput {
			if err := t.Start(now); err != nil {
				return err
			}
		}
	}
	switch env.Type {
	case events.TaskRunning.Type():
		ev, err := bus.Decode(env, events.TaskRunning)
		if err != nil {
			return err
		}
		if ev.Data.Worker != "" && t.info.Status == events.StatusPending {
			return t.Accept(ev.Data.Worker, now)
		}
		return t.Start(now)
	case events.TaskProgress.Type():
		ev, err := bus.Decode(env, events.TaskProgress)
		if err != nil {
			return err
		}
		return t.Progress(ev.Data.Message, ev.Data.Percent, now)
	case events.TaskNeedsInput.Type():
		ev, err := bus.Decode(env, events.TaskNeedsInput)
		if err != nil {
			return err
		}
		return t.Ask(ev.Data.Question, ev.Data.Options, now)
	case events.TaskDone.Type():
		ev, err := bus.Decode(env, events.TaskDone)
		if err != nil {
			return err
		}
		return t.Finish(ev.Data.Result, now)
	case events.TaskFailed.Type():
		ev, err := bus.Decode(env, events.TaskFailed)
		if err != nil {
			return err
		}
		return t.Fail(ev.Data.Error, now)
	case events.TaskCancelled.Type():
		ev, err := bus.Decode(env, events.TaskCancelled)
		if err != nil {
			return err
		}
		return t.Cancel(ev.Data.Reason, now)
	}
	return fmt.Errorf("task: unexpected event type %s", env.Type)
}

// EventTaskID returns the ID of the task a task event is about.
func EventTaskID(env bus.Envelope) (string, error) {
	var ref struct {
		TaskID string `json:"task_id"`
		ID     string `json:"id"` // task.created carries a full TaskInfo
	}
	if err := json.Unmarshal(env.Data, &ref); err != nil {
		return "", fmt.Errorf("task: decode %s: %w", env.Type, err)
	}
	if ref.TaskID != "" {
		return ref.TaskID, nil
	}
	if ref.ID != "" {
		return ref.ID, nil
	}
	return "", fmt.Errorf("task: %s event has no task ID", env.Type)
}
