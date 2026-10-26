package events

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
)

// Tasks are DOGGIE's units of background work. The task manager owns the
// state of every task; workers (the components that know how to do things)
// run them. Requesters get a task ID at once and follow the task through
// its events.
//
// The manager is the only one that publishes task events, so a task's
// events are in the order it applied them. Workers report to it with
// commands under their own name (WorkerAccept and the rest), which the bus
// only lets each worker use for itself: the manager takes a task's reports
// only from the worker that accepted it.

// TaskStatus is where a task is in its lifecycle.
type TaskStatus string

const (
	StatusPending      TaskStatus = "pending"
	StatusRunning      TaskStatus = "running"
	StatusWaitingInput TaskStatus = "waiting_input"
	StatusDone         TaskStatus = "done"
	StatusFailed       TaskStatus = "failed"
	StatusCancelled    TaskStatus = "cancelled"
)

// Terminal reports whether the task is over.
func (s TaskStatus) Terminal() bool {
	return s == StatusDone || s == StatusFailed || s == StatusCancelled
}

// TaskSpec is what a requester asks for.
type TaskSpec struct {
	// Kind selects the worker, e.g. "demo.sleep".
	Kind  string          `json:"kind"`
	Input json.RawMessage `json:"input,omitempty"`
	// Description is human-readable, e.g. "Wait 30 seconds".
	Description string `json:"description,omitempty"`
	// ParentID makes this a subtask, cancelled with its parent.
	ParentID string `json:"parent_id,omitempty"`
	// ThreadID is the conversation thread that asked, so notices about the
	// task reach whoever requested it.
	ThreadID string `json:"thread_id,omitempty"`
	// TimeoutS overrides the manager's default timeout, in seconds.
	TimeoutS int `json:"timeout_s,omitempty"`
	// CausedBy are the events that led to the task, e.g. the sentences a
	// turn answered when the LLM started it: at most MaxCausedBy IDs. Its
	// task.created, logged before the task runs, records which sentence
	// asked for what.
	CausedBy []string `json:"caused_by,omitempty"`
	// Sensitive keeps Input, the result, progress messages and questions
	// out of the event log: they only travel in commands and live in the
	// manager's memory (task.get tells them), so they are gone after a
	// restart. The description is still logged, so keep it free of
	// sensitive data.
	Sensitive bool `json:"sensitive,omitempty"`
}

// MaxCausedBy bounds TaskSpec.CausedBy.
const MaxCausedBy = 16

// TaskInfo is a snapshot of a task.
type TaskInfo struct {
	ID string `json:"id"`
	TaskSpec
	Status    TaskStatus      `json:"status"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Deadline  time.Time       `json:"deadline"`
	Progress  string          `json:"progress,omitempty"`
	Percent   int             `json:"percent,omitempty"`
	Question  string          `json:"question,omitempty"`
	Options   []string        `json:"options,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	// Worker is who accepted the task, as the bus knows it.
	Worker string `json:"worker,omitempty"`
	// HeartbeatMS tells the worker how often to send TaskAlive, and Claim is
	// what it accepts the task with (see WorkerAccept). They are set on run
	// requests only.
	HeartbeatMS int    `json:"heartbeat_ms,omitempty"`
	Claim       string `json:"claim,omitempty"`
}

// TaskRef identifies a task.
type TaskRef struct {
	TaskID string `json:"task_id"`
}

// TaskProgressData reports how a running task is doing.
type TaskProgressData struct {
	TaskID  string `json:"task_id"`
	Message string `json:"message"`
	// Percent is 0-100, or 0 when unknown.
	Percent int `json:"percent,omitempty"`
	// Redacted means the message of a sensitive task was left out of the
	// event; task.get tells it.
	Redacted bool `json:"redacted,omitempty"`
}

// TaskNeedsInputData is a question a task needs answered to continue.
type TaskNeedsInputData struct {
	TaskID   string   `json:"task_id"`
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
	// Redacted means the question and options of a sensitive task were
	// left out of the event; task.get tells them while the task waits.
	Redacted bool `json:"redacted,omitempty"`
}

// TaskDoneData is a task's result.
type TaskDoneData struct {
	TaskID string          `json:"task_id"`
	Result json.RawMessage `json:"result,omitempty"`
	// Redacted means the result of a sensitive task was left out of the
	// event; only the manager keeps it.
	Redacted bool `json:"redacted,omitempty"`
}

// TaskAliveData says a worker is still running a task.
type TaskAliveData struct {
	TaskID string `json:"task_id"`
}

// TaskAlive is sent by workers for every task they run, every HeartbeatMS,
// under their name: TaskAlive.With(worker). The manager fails tasks whose
// heartbeat stops. It is a signal, so beats don't flood the event log.
var TaskAlive = bus.NewSignal[TaskAliveData]("task.alive")

// TaskRunningData says a task runs: its worker accepted it, or its question
// was answered.
type TaskRunningData struct {
	TaskID string `json:"task_id"`
	// Worker is who runs it, when it was just accepted.
	Worker string `json:"worker,omitempty"`
}

// TaskFailedData explains why a task failed.
type TaskFailedData struct {
	TaskID string `json:"task_id"`
	Error  string `json:"error"`
}

// TaskCancelledData explains why a task was cancelled.
type TaskCancelledData struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason,omitempty"`
	// Channel is where a person asked for it directly, e.g. from a chat,
	// not through the brain; for a subtask, where its parent's was asked
	// for. Empty when DOGGIE did it. The bus vouches for it.
	Channel string `json:"channel,omitempty"`
}

// Task events, all published by the manager.
var (
	TaskCreated    = bus.NewSubject[TaskInfo]("task.created")
	TaskRunning    = bus.NewSubject[TaskRunningData]("task.running")
	TaskProgress   = bus.NewSubject[TaskProgressData]("task.progress")
	TaskNeedsInput = bus.NewSubject[TaskNeedsInputData]("task.needs_input")
	TaskDone       = bus.NewSubject[TaskDoneData]("task.done")
	TaskFailed     = bus.NewSubject[TaskFailedData]("task.failed")
	TaskCancelled  = bus.NewSubject[TaskCancelledData]("task.cancelled")
)

// Ack is the empty reply of commands that only need confirmation.
type Ack struct{}

// TaskCancelRequest asks to cancel a task and its subtasks.
type TaskCancelRequest struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason,omitempty"`
}

// TaskAnswer answers the question of a task waiting for input.
type TaskAnswer struct {
	TaskID string `json:"task_id"`
	Answer string `json:"answer"`
}

// TaskListRequest filters the task list.
type TaskListRequest struct {
	ActiveOnly bool `json:"active_only,omitempty"`
	// Limit keeps the most recent matching tasks. 0 means the manager's
	// default; the manager also caps it so the reply fits in one message.
	Limit int `json:"limit,omitempty"`
}

// TaskList is a list of tasks, oldest first.
type TaskList struct {
	Tasks []TaskInfo `json:"tasks"`
	// Total is how many tasks matched before the limit.
	Total int `json:"total"`
}

// Commands served by the task manager.
var (
	StartTask  = bus.NewCommand[TaskSpec, TaskRef]("task.start")
	CancelTask = bus.NewCommand[TaskCancelRequest, Ack]("task.cancel")
	AnswerTask = bus.NewCommand[TaskAnswer, Ack]("task.answer")
	ListTasks  = bus.NewCommand[TaskListRequest, TaskList]("task.list")
	GetTask    = bus.NewCommand[TaskRef, TaskInfo]("task.get")
	// CancelTaskFrom is CancelTask asked for by a person directly, on a
	// channel: CancelTaskFrom.With(channel), see CancelTaskOn. The bus only
	// lets each satellite use its own channels, and CancelTask is for
	// DOGGIE itself, so task.cancelled tells truly who asked.
	CancelTaskFrom = bus.NewCommand[TaskCancelRequest, Ack]("task.cancel.from")
)

// CancelTaskOn is the command a channel cancels tasks with.
func CancelTaskOn(channel string) (bus.Command[TaskCancelRequest, Ack], error) {
	if !ValidChannel(channel) {
		return bus.Command[TaskCancelRequest, Ack]{}, fmt.Errorf("invalid channel %q", channel)
	}
	return CancelTaskFrom.With(channel), nil
}

// TaskControlAction is what the manager asks a worker to do with a task.
type TaskControlAction string

const (
	ControlCancel TaskControlAction = "cancel"
	ControlAnswer TaskControlAction = "answer"
)

// TaskControl is sent by the manager to the worker running a task.
type TaskControl struct {
	Action TaskControlAction `json:"action"`
	Answer string            `json:"answer,omitempty"`
	Reason string            `json:"reason,omitempty"`
}

// Commands served by workers. RunTask is served per kind
// (RunTask.With(kind)), load-balanced across workers. ControlTask is served
// per task by the worker running it: ControlTask.With(worker, taskID).
var (
	RunTask     = bus.NewCommand[TaskInfo, Ack]("task.run")
	ControlTask = bus.NewCommand[TaskControl, Ack]("task.control")
)

// TaskClaim accepts a task: Claim is the one its run request carried, which
// only the workers of its kind receive.
type TaskClaim struct {
	TaskID string `json:"task_id"`
	Claim  string `json:"claim"`
}

// Commands a worker sends the manager about the tasks it runs, under its
// own name: WorkerAccept.With(worker), and so on. The manager answers with
// an error when the report no longer fits the task, e.g. it was cancelled:
// the worker should stop.
var (
	// WorkerAccept takes a task the worker received, before it runs it.
	WorkerAccept = bus.NewCommand[TaskClaim, Ack]("task.accept")
	// WorkerProgress reports how a task is doing.
	WorkerProgress = bus.NewCommand[TaskProgressData, Ack]("task.report.progress")
	// WorkerAsk pauses a task with a question; the answer comes as a
	// ControlTask.
	WorkerAsk = bus.NewCommand[TaskNeedsInputData, Ack]("task.report.ask")
	// WorkerDone finishes a task with its result. The result of a sensitive
	// task stays out of the event log, in the manager's memory.
	WorkerDone = bus.NewCommand[TaskDoneData, Ack]("task.report.done")
	// WorkerFailed fails a task.
	WorkerFailed = bus.NewCommand[TaskFailedData, Ack]("task.report.failed")
)
