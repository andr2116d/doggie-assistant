package events

import (
	"encoding/json"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
)

// Tasks are DOGGIE's units of background work. The task manager owns the
// state of every task; workers (the components that know how to do things)
// run them. Requesters get a task ID at once and follow the task through
// its events.

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
	// Kind selects the worker, e.g. "tv.open_app".
	Kind  string          `json:"kind"`
	Input json.RawMessage `json:"input,omitempty"`
	// Description is human-readable, e.g. "Open Netflix on the TV".
	Description string `json:"description,omitempty"`
	// ParentID makes this a subtask, cancelled with its parent.
	ParentID string `json:"parent_id,omitempty"`
	// ThreadID is the conversation thread that asked, so notices about the
	// task reach whoever requested it.
	ThreadID string `json:"thread_id,omitempty"`
	// TimeoutS overrides the manager's default timeout, in seconds.
	TimeoutS int `json:"timeout_s,omitempty"`
	// Sensitive keeps Input and the result out of the event log: they only
	// travel in commands and live in the manager's memory, so they are gone
	// after a restart. Description, progress messages and questions are still
	// logged, so keep them free of sensitive data.
	Sensitive bool `json:"sensitive,omitempty"`
}

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
	// HeartbeatMS tells the worker how often to send TaskAlive. It is set on
	// run requests only.
	HeartbeatMS int `json:"heartbeat_ms,omitempty"`
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
}

// TaskNeedsInputData is a question a task needs answered to continue.
type TaskNeedsInputData struct {
	TaskID   string   `json:"task_id"`
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

// TaskDoneData is a task's result.
type TaskDoneData struct {
	TaskID string          `json:"task_id"`
	Result json.RawMessage `json:"result,omitempty"`
	// Redacted means the result of a sensitive task was left out of the
	// event; the manager received it with ReportResult.
	Redacted bool `json:"redacted,omitempty"`
}

// TaskAliveData says a worker is still running a task.
type TaskAliveData struct {
	TaskID string `json:"task_id"`
}

// TaskAlive is sent by workers for every task they run, every HeartbeatMS.
// The manager fails tasks whose heartbeat stops. It is a signal, so beats
// don't flood the event log.
var TaskAlive = bus.NewSignal[TaskAliveData]("task.alive")

// TaskFailedData explains why a task failed.
type TaskFailedData struct {
	TaskID string `json:"task_id"`
	Error  string `json:"error"`
}

// TaskCancelledData explains why a task was cancelled.
type TaskCancelledData struct {
	TaskID string `json:"task_id"`
	Reason string `json:"reason,omitempty"`
}

// Task events. The manager publishes created, running, cancelled and failed
// (timeouts, missing workers, restarts). Workers publish progress,
// needs_input, done and failed.
var (
	TaskCreated    = bus.NewSubject[TaskInfo]("task.created")
	TaskRunning    = bus.NewSubject[TaskRef]("task.running")
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
	// ReportResult hands the manager the result of a sensitive task, outside
	// the event log, before the worker publishes a redacted task.done.
	ReportResult = bus.NewCommand[TaskDoneData, Ack]("task.result")
)

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
// per task (ControlTask.With(taskID)) by the worker running it.
var (
	RunTask     = bus.NewCommand[TaskInfo, Ack]("task.run")
	ControlTask = bus.NewCommand[TaskControl, Ack]("task.control")
)
