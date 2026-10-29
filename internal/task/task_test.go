package task

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

var t0 = time.Date(2026, 10, 24, 20, 0, 0, 0, time.UTC)

// taskIn returns a task driven to status through valid transitions.
func taskIn(t *testing.T, status events.TaskStatus) *Task {
	t.Helper()
	tk := New("t1", events.TaskSpec{Kind: "demo.sleep"}, t0, t0.Add(time.Minute))
	steps := map[events.TaskStatus][]func() error{
		events.StatusPending:      nil,
		events.StatusRunning:      {func() error { return tk.Start(t0) }},
		events.StatusWaitingInput: {func() error { return tk.Start(t0) }, func() error { return tk.Ask("q?", nil, t0) }},
		events.StatusDone:         {func() error { return tk.Start(t0) }, func() error { return tk.Finish(nil, t0) }},
		events.StatusFailed:       {func() error { return tk.Fail("boom", t0) }},
		events.StatusCancelled:    {func() error { return tk.Cancel("stop", t0) }},
	}
	for _, step := range steps[status] {
		if err := step(); err != nil {
			t.Fatalf("driving to %s: %v", status, err)
		}
	}
	return tk
}

func TestTransitions(t *testing.T) {
	type action struct {
		name string
		do   func(*Task, time.Time) error
	}
	actions := []action{
		{"start", func(tk *Task, now time.Time) error { return tk.Start(now) }},
		{"progress", func(tk *Task, now time.Time) error { return tk.Progress("1/3", 33, now) }},
		{"ask", func(tk *Task, now time.Time) error { return tk.Ask("which?", []string{"a"}, now) }},
		{"finish", func(tk *Task, now time.Time) error { return tk.Finish(json.RawMessage(`{}`), now) }},
		{"fail", func(tk *Task, now time.Time) error { return tk.Fail("boom", now) }},
		{"cancel", func(tk *Task, now time.Time) error { return tk.Cancel("stop", now) }},
	}
	S := struct{ P, R, W, D, F, C events.TaskStatus }{
		events.StatusPending, events.StatusRunning, events.StatusWaitingInput,
		events.StatusDone, events.StatusFailed, events.StatusCancelled,
	}
	// want[from][action] is the resulting status, or "" when the transition
	// must be rejected.
	want := map[events.TaskStatus]map[string]events.TaskStatus{
		S.P: {"start": S.R, "progress": "", "ask": "", "finish": "", "fail": S.F, "cancel": S.C},
		S.R: {"start": "", "progress": S.R, "ask": S.W, "finish": S.D, "fail": S.F, "cancel": S.C},
		S.W: {"start": S.R, "progress": "", "ask": "", "finish": "", "fail": S.F, "cancel": S.C},
		S.D: {"start": "", "progress": "", "ask": "", "finish": "", "fail": "", "cancel": ""},
		S.F: {"start": "", "progress": "", "ask": "", "finish": "", "fail": "", "cancel": ""},
		S.C: {"start": "", "progress": "", "ask": "", "finish": "", "fail": "", "cancel": ""},
	}
	later := t0.Add(time.Second)
	for from, row := range want {
		for _, a := range actions {
			t.Run(fmt.Sprintf("%s/%s", from, a.name), func(t *testing.T) {
				tk := taskIn(t, from)
				err := a.do(tk, later)
				to := row[a.name]
				if to == "" {
					if !errors.Is(err, ErrInvalidTransition) {
						t.Fatalf("err = %v, want ErrInvalidTransition", err)
					}
					if tk.Status() != from {
						t.Errorf("status changed to %s on a rejected transition", tk.Status())
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if tk.Status() != to {
					t.Errorf("status = %s, want %s", tk.Status(), to)
				}
				if !tk.Info().UpdatedAt.Equal(later) {
					t.Error("UpdatedAt not set")
				}
			})
		}
	}
}

func TestAnswerClearsQuestion(t *testing.T) {
	tk := taskIn(t, events.StatusWaitingInput)
	if err := tk.Start(t0); err != nil {
		t.Fatal(err)
	}
	if info := tk.Info(); info.Question != "" || info.Options != nil {
		t.Errorf("question not cleared: %+v", info)
	}
}

func TestProgressClampsPercent(t *testing.T) {
	tk := taskIn(t, events.StatusRunning)
	tk.Progress("x", 250, t0)
	if p := tk.Info().Percent; p != 100 {
		t.Errorf("Percent = %d, want 100", p)
	}
	tk.Progress("x", -5, t0)
	if p := tk.Info().Percent; p != 0 {
		t.Errorf("Percent = %d, want 0", p)
	}
}

func TestInfoIsACopy(t *testing.T) {
	tk := taskIn(t, events.StatusRunning)
	tk.Ask("which?", []string{"a", "b"}, t0)
	info := tk.Info()
	info.Options[0] = "changed"
	if tk.Info().Options[0] != "a" {
		t.Error("mutating a snapshot changed the task")
	}
}

func envelope(t *testing.T, typ string, data any) bus.Envelope {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	return bus.Envelope{Meta: bus.Meta{Type: typ, Time: t0.Add(time.Hour)}, Data: raw}
}

func TestApplyFoldsEvents(t *testing.T) {
	tk := New("t1", events.TaskSpec{Kind: "demo.ask"}, t0, t0.Add(time.Minute))
	for _, env := range []bus.Envelope{
		envelope(t, events.TaskRunning.Type(), events.TaskRef{TaskID: "t1"}),
		envelope(t, events.TaskProgress.Type(), events.TaskProgressData{TaskID: "t1", Message: "half", Percent: 50}),
		envelope(t, events.TaskNeedsInput.Type(), events.TaskNeedsInputData{TaskID: "t1", Question: "list?", Options: []string{"Groceries"}}),
		envelope(t, events.TaskRunning.Type(), events.TaskRef{TaskID: "t1"}),
		envelope(t, events.TaskDone.Type(), events.TaskDoneData{TaskID: "t1", Result: json.RawMessage(`{"answer":"Compras"}`)}),
	} {
		if err := tk.Apply(env); err != nil {
			t.Fatalf("apply %s: %v", env.Type, err)
		}
	}
	info := tk.Info()
	if info.Status != events.StatusDone || string(info.Result) != `{"answer":"Compras"}` || info.Progress != "half" {
		t.Errorf("unexpected state: %+v", info)
	}
	if !info.UpdatedAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("UpdatedAt = %s, want the event time", info.UpdatedAt)
	}

	late := envelope(t, events.TaskFailed.Type(), events.TaskFailedData{TaskID: "t1", Error: "late"})
	if err := tk.Apply(late); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("late failure: err = %v, want ErrInvalidTransition", err)
	}
	if err := tk.Apply(envelope(t, "task.unknown", events.TaskRef{TaskID: "t1"})); err == nil {
		t.Error("unknown event type accepted")
	}
}

func TestWorkerEventsImplyStart(t *testing.T) {
	tests := []struct {
		from events.TaskStatus
		env  bus.Envelope
		want events.TaskStatus
	}{
		// A fast worker's result can arrive before the manager records that
		// the worker accepted the task.
		{events.StatusPending, envelope(t, events.TaskDone.Type(), events.TaskDoneData{TaskID: "t1"}), events.StatusDone},
		{events.StatusPending, envelope(t, events.TaskProgress.Type(), events.TaskProgressData{TaskID: "t1"}), events.StatusRunning},
		{events.StatusPending, envelope(t, events.TaskNeedsInput.Type(), events.TaskNeedsInputData{TaskID: "t1"}), events.StatusWaitingInput},
		// ...or before the manager records that the answer was delivered.
		{events.StatusWaitingInput, envelope(t, events.TaskDone.Type(), events.TaskDoneData{TaskID: "t1"}), events.StatusDone},
		{events.StatusWaitingInput, envelope(t, events.TaskNeedsInput.Type(), events.TaskNeedsInputData{TaskID: "t1", Question: "next?"}), events.StatusWaitingInput},
	}
	for _, tt := range tests {
		tk := taskIn(t, tt.from)
		if err := tk.Apply(tt.env); err != nil {
			t.Errorf("%s + %s: %v", tt.from, tt.env.Type, err)
			continue
		}
		if tk.Status() != tt.want {
			t.Errorf("%s + %s = %s, want %s", tt.from, tt.env.Type, tk.Status(), tt.want)
		}
	}

	// The manager's own late "running" is then rejected, not re-applied.
	tk := taskIn(t, events.StatusPending)
	tk.Apply(envelope(t, events.TaskDone.Type(), events.TaskDoneData{TaskID: "t1"}))
	if err := tk.Apply(envelope(t, events.TaskRunning.Type(), events.TaskRef{TaskID: "t1"})); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("late running: err = %v, want ErrInvalidTransition", err)
	}
}

func TestEventTaskID(t *testing.T) {
	for _, tt := range []struct {
		env  bus.Envelope
		want string
	}{
		{envelope(t, events.TaskDone.Type(), events.TaskDoneData{TaskID: "t7"}), "t7"},
		{envelope(t, events.TaskCreated.Type(), events.TaskInfo{ID: "t8"}), "t8"},
	} {
		if got, err := EventTaskID(tt.env); err != nil || got != tt.want {
			t.Errorf("EventTaskID(%s) = %q, %v; want %q", tt.env.Type, got, err, tt.want)
		}
	}
	if _, err := EventTaskID(envelope(t, events.TaskDone.Type(), struct{}{})); err == nil {
		t.Error("event without a task ID accepted")
	}
}
