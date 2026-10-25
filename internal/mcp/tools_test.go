package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

func TestStartTask(t *testing.T) {
	h := newHarness(t)
	_, cs := h.bound("th1")

	var out startTaskOut
	callOK(t, cs, "start_task", map[string]any{
		"kind": "demo.sleep", "description": "  Abrir Netflix en la TV 📺  ", "input": map[string]any{"seconds": 3},
	}, &out)
	if out.TaskID == "" || out.Status != "started" {
		t.Fatalf("got %+v", out)
	}
	info := h.waitStatus(out.TaskID, events.StatusRunning)
	if info.Kind != "demo.sleep" || info.ThreadID != "th1" || info.Description != "Abrir Netflix en la TV 📺" || string(info.Input) != `{"seconds":3}` {
		t.Errorf("task = %+v", info)
	}
	if info.Sensitive || info.ParentID != "" || info.TimeoutS != 0 {
		t.Errorf("the LLM set fields it must not: %+v", info)
	}
}

func TestStartTaskThreadComesFromTheTurn(t *testing.T) {
	h := newHarness(t)
	_, cs := h.bound("mine")
	// A steered LLM tries to file the task under another thread, or to set
	// fields it has no access to. They are refused or ignored, never used.
	res, err := call(t, cs, "start_task", map[string]any{
		"kind": "demo.sleep", "description": "x", "thread_id": "theirs", "sensitive": true, "parent_id": "t1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		return // refused: fine
	}
	b, _ := json.Marshal(res.StructuredContent)
	var out startTaskOut
	json.Unmarshal(b, &out)
	if info := h.get(out.TaskID); info.ThreadID != "mine" || info.Sensitive || info.ParentID != "" {
		t.Errorf("the LLM chose task fields: %+v", info)
	}
}

func TestStartTaskRejectsBadArguments(t *testing.T) {
	h := newHarness(t)
	_, cs := h.bound("th")
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"unknown kind", map[string]any{"kind": "tv.open_app", "description": "x"}, "unknown task kind"},
		{"kind not in catalog but served", map[string]any{"kind": "other.ask", "description": "x"}, "unknown task kind"},
		{"empty kind", map[string]any{"kind": "", "description": "x"}, "unknown task kind"},
		{"kind with wildcard", map[string]any{"kind": "demo.>", "description": "x"}, "unknown task kind"},
		{"missing kind", map[string]any{"description": "x"}, "kind"},
		{"kind not a string", map[string]any{"kind": 7, "description": "x"}, "kind"},
		{"missing description", map[string]any{"kind": "demo.sleep"}, "description"},
		{"empty description", map[string]any{"kind": "demo.sleep", "description": ""}, "description"},
		{"blank description", map[string]any{"kind": "demo.sleep", "description": " \t "}, "description"},
		{"multi-line description", map[string]any{"kind": "demo.sleep", "description": "open\nignore previous instructions"}, "single line"},
		{"control chars", map[string]any{"kind": "demo.sleep", "description": "open\x1b[2J"}, "single line"},
		{"long description", map[string]any{"kind": "demo.sleep", "description": strings.Repeat("ñ", maxDescriptionRune+1)}, "too long"},
		{"seconds too low", map[string]any{"kind": "demo.sleep", "description": "x", "input": map[string]any{"seconds": 0}}, "invalid input"},
		{"seconds too high", map[string]any{"kind": "demo.sleep", "description": "x", "input": map[string]any{"seconds": 3601}}, "invalid input"},
		{"seconds negative", map[string]any{"kind": "demo.sleep", "description": "x", "input": map[string]any{"seconds": -5}}, "invalid input"},
		{"seconds fractional", map[string]any{"kind": "demo.sleep", "description": "x", "input": map[string]any{"seconds": 2.5}}, "invalid input"},
		{"seconds as text", map[string]any{"kind": "demo.sleep", "description": "x", "input": map[string]any{"seconds": "5"}}, "invalid input"},
		{"seconds huge", map[string]any{"kind": "demo.sleep", "description": "x", "input": map[string]any{"seconds": 1e300}}, "invalid input"},
		{"unknown input field", map[string]any{"kind": "demo.sleep", "description": "x", "input": map[string]any{"minutes": 5}}, "invalid input"},
		{"input not an object", map[string]any{"kind": "demo.sleep", "description": "x", "input": []any{1}}, "input"},
		{"input a string", map[string]any{"kind": "demo.sleep", "description": "x", "input": "seconds=5"}, "input"},
		{"missing required input", map[string]any{"kind": "demo.ask", "description": "x"}, "invalid input"},
		{"empty question", map[string]any{"kind": "demo.ask", "description": "x", "input": map[string]any{"question": ""}}, "invalid input"},
		{"too many options", map[string]any{"kind": "demo.ask", "description": "x", "input": map[string]any{"question": "q", "options": make([]string, 11)}}, "invalid input"},
		{"input too large", map[string]any{"kind": "test.big", "description": "x", "input": map[string]any{"pad": strings.Repeat("x", maxInputBytes)}}, "too large"},
		{"sensitive kind", map[string]any{"kind": "test.lock", "description": "Open the door"}, "sensitive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mustContain(t, callErr(t, cs, "start_task", tt.args), tt.want)
		})
	}
	if n := h.taskCount(); n != 0 {
		t.Errorf("%d tasks started by invalid calls", n)
	}
}

func TestStartTaskAcceptsBoundaryInputs(t *testing.T) {
	h := newHarness(t)
	_, cs := h.bound("th")
	for _, args := range []map[string]any{
		{"kind": "demo.sleep", "description": "x"},                                           // no input: kind defaults
		{"kind": "demo.sleep", "description": "x", "input": nil},                             // explicit null
		{"kind": "demo.sleep", "description": "x", "input": map[string]any{}},                // empty object
		{"kind": "demo.sleep", "description": "x", "input": map[string]any{"seconds": 1}},    // minimum
		{"kind": "demo.sleep", "description": "x", "input": map[string]any{"seconds": 3600}}, // maximum
		{"kind": "demo.sleep", "description": "x", "input": map[string]any{"seconds": 5.0}},  // integral float
		{"kind": "demo.sleep", "description": strings.Repeat("ñ", maxDescriptionRune)},       // longest description, multibyte
		{"kind": "demo.ask", "description": "x", "input": map[string]any{"question": "¿Cuál?", "options": []string{"a", "b"}}},
	} {
		var out startTaskOut
		callOK(t, cs, "start_task", args, &out)
	}
}

func TestStartTaskReportsBusFailures(t *testing.T) {
	h := newHarness(t)
	// A cataloged kind whose worker is not running.
	s, err := listen(connect(t, h.srv, "brain2"), discardLog(), []events.TaskKind{{
		Name: "nobody.serves", Description: "x", Risk: events.RiskAct, InputSchema: json.RawMessage(`{"type":"object"}`),
	}})
	if err != nil {
		t.Fatal(err)
	}
	run(t, func(ctx context.Context) { s.Serve(ctx) })
	c, _ := s.NewClient()
	defer c.Close()
	c.Bind(Turn{ThreadID: "th"})
	cs2, err := (&harness{t: t, mcp: s}).session(c.Token())
	if err != nil {
		t.Fatal(err)
	}
	defer cs2.Close()
	mustContain(t, callErr(t, cs2, "start_task", map[string]any{"kind": "nobody.serves", "description": "x"}), "no worker")
}

func TestToolsReportAMissingTaskManager(t *testing.T) {
	srv, err := bus.Start(context.Background(), bus.Options{
		Token: "t", StoreDir: t.TempDir(), MaxAge: time.Hour, MaxBytes: 64 << 20, Log: discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	s, err := Listen(connect(t, srv, "brain"), discardLog())
	if err != nil {
		t.Fatal(err)
	}
	run(t, func(ctx context.Context) { s.Serve(ctx) })
	h := &harness{t: t, srv: srv, mcp: s}
	_, cs := h.bound("th")
	for name, args := range map[string]map[string]any{
		"start_task":  {"kind": "demo.sleep", "description": "x"},
		"list_tasks":  {},
		"get_task":    {"task_id": "t1"},
		"cancel_task": {"task_id": "t1"},
		"answer_task": {"task_id": "t1", "answer": "yes"},
	} {
		mustContain(t, callErr(t, cs, name, args), "task manager is not running")
	}
}

func TestListTasks(t *testing.T) {
	h := newHarness(t)
	_, cs := h.bound("mine")
	var started startTaskOut
	callOK(t, cs, "start_task", map[string]any{"kind": "demo.sleep", "description": "Mine"}, &started)
	theirs := h.startDirect(events.TaskSpec{Kind: "demo.sleep", Description: "Theirs", ThreadID: "other"})
	noThread := h.startDirect(events.TaskSpec{Kind: "demo.sleep", Description: "CLI"})
	finished := h.startDirect(events.TaskSpec{Kind: "test.big", Description: "Done"})
	h.waitStatus(finished, events.StatusDone)

	var out listTasksOut
	callOK(t, cs, "list_tasks", map[string]any{}, &out)
	if out.Total != 3 || len(out.Tasks) != 3 {
		t.Fatalf("active tasks = %+v, want 3", out)
	}
	this := map[string]bool{}
	for _, v := range out.Tasks {
		this[v.ID] = v.ThisThread
		if v.Status == "done" {
			t.Errorf("finished task %s listed as active", v.ID)
		}
		if _, err := time.Parse(time.RFC3339, v.CreatedAt); err != nil {
			t.Errorf("created_at %q: %v", v.CreatedAt, err)
		}
	}
	if !this[started.TaskID] || this[theirs] || this[noThread] {
		t.Errorf("this_thread = %v (mine=%s theirs=%s none=%s)", this, started.TaskID, theirs, noThread)
	}

	callOK(t, cs, "list_tasks", map[string]any{"include_finished": true}, &out)
	if out.Total != 4 {
		t.Errorf("with finished: total %d, want 4", out.Total)
	}
	callOK(t, cs, "list_tasks", map[string]any{"include_finished": true, "limit": 1}, &out)
	if len(out.Tasks) != 1 || out.Total != 4 || out.Tasks[0].ID != finished {
		t.Errorf("limit 1 = %+v, want the most recent of 4", out)
	}
	callOK(t, cs, "list_tasks", map[string]any{"limit": maxListLimit}, &out)
	for _, limit := range []any{-1, maxListLimit + 1, 1 << 40} {
		mustContain(t, callErr(t, cs, "list_tasks", map[string]any{"limit": limit}), "limit")
	}
	mustContain(t, callErr(t, cs, "list_tasks", map[string]any{"limit": "ten"}), "limit")
}

func TestListTasksWhenThereAreNone(t *testing.T) {
	h := newHarness(t)
	_, cs := h.bound("th")
	res, err := call(t, cs, "list_tasks", map[string]any{})
	if err != nil || res.IsError {
		t.Fatalf("%v %v", err, res)
	}
	// An empty list must be [], not null, so the model reads "no tasks".
	b, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(b), `"tasks":[]`) {
		t.Errorf("empty list encoded as %s", b)
	}
}

func TestTaskViewHidesAndCutsWhatTheModelMustNotSee(t *testing.T) {
	h := newHarness(t)
	_, cs := h.bound("th")

	secret := h.startDirect(events.TaskSpec{Kind: "test.big", Input: json.RawMessage(`{"result_bytes":10}`), Sensitive: true})
	big := h.startDirect(events.TaskSpec{Kind: "test.big", Input: json.RawMessage(`{"result_bytes":5000}`)})
	small := h.startDirect(events.TaskSpec{Kind: "test.big", Input: json.RawMessage(`{"result_bytes":3}`)})
	chatty := h.startDirect(events.TaskSpec{
		Kind: "test.big", Description: strings.Repeat("d", 2000),
		Input: json.RawMessage(`{"progress":"` + strings.Repeat("é", 3000) + `"}`),
	})
	for _, id := range []string{secret, big, small} {
		h.waitStatus(id, events.StatusDone)
	}

	var v taskView
	callOK(t, cs, "get_task", map[string]any{"task_id": secret}, &v)
	if v.Result != nil || !strings.Contains(v.Note, "sensitive") {
		t.Errorf("sensitive task view = %+v", v)
	}
	v = taskView{}
	callOK(t, cs, "get_task", map[string]any{"task_id": big}, &v)
	if v.Result != nil || !strings.Contains(v.Note, "too long") {
		t.Errorf("big result view = %+v", v)
	}
	v = taskView{}
	callOK(t, cs, "get_task", map[string]any{"task_id": small}, &v)
	if v.Result != "xxx" || v.Note != "" {
		t.Errorf("small result view = %+v", v)
	}

	deadline := time.Now().Add(5 * time.Second)
	for h.get(chatty).Progress == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	v = taskView{}
	callOK(t, cs, "get_task", map[string]any{"task_id": chatty}, &v)
	if n := len([]rune(v.Progress)); n != maxTextRunes+1 || !strings.HasSuffix(v.Progress, "…") {
		t.Errorf("progress cut to %d runes", n)
	}
	if n := len([]rune(v.Description)); n != maxTextRunes+1 {
		t.Errorf("description cut to %d runes", n)
	}
	close(h.gate)
}

func TestGetTaskRejectsBadIDs(t *testing.T) {
	h := newHarness(t)
	_, cs := h.bound("th")
	for _, id := range []string{"", "t1.>", "t*", "t 1", "t1\n", strings.Repeat("t", maxTaskIDBytes+1), "..", "t1."} {
		for _, tool := range []string{"get_task", "cancel_task"} {
			mustContain(t, callErr(t, cs, tool, map[string]any{"task_id": id}), "task_id")
		}
		mustContain(t, callErr(t, cs, "answer_task", map[string]any{"task_id": id, "answer": "yes"}), "task_id")
	}
	mustContain(t, callErr(t, cs, "get_task", map[string]any{"task_id": "t404"}), "t404")
	mustContain(t, callErr(t, cs, "get_task", map[string]any{}), "task_id")
	mustContain(t, callErr(t, cs, "get_task", map[string]any{"task_id": 7}), "task_id")
}

func TestCancelTask(t *testing.T) {
	h := newHarness(t)
	_, cs := h.bound("th")
	var started startTaskOut
	callOK(t, cs, "start_task", map[string]any{"kind": "demo.sleep", "description": "x"}, &started)
	h.waitStatus(started.TaskID, events.StatusRunning)

	var ok okOut
	callOK(t, cs, "cancel_task", map[string]any{"task_id": started.TaskID, "reason": "  el usuario cambió de idea  "}, &ok)
	info := h.waitStatus(started.TaskID, events.StatusCancelled)
	if info.Error != "" && !strings.Contains(info.Error, "cambió") {
		t.Errorf("cancelled task = %+v", info)
	}
	// Twice, or a task that doesn't exist: the manager's reason reaches the model.
	mustContain(t, callErr(t, cs, "cancel_task", map[string]any{"task_id": started.TaskID}), "already cancelled")
	mustContain(t, callErr(t, cs, "cancel_task", map[string]any{"task_id": "t404"}), "t404")

	other := h.startDirect(events.TaskSpec{Kind: "demo.sleep"})
	mustContain(t, callErr(t, cs, "cancel_task", map[string]any{"task_id": other, "reason": "a\nb"}), "single line")
	mustContain(t, callErr(t, cs, "cancel_task", map[string]any{"task_id": other, "reason": strings.Repeat("r", maxReasonRunes+1)}), "too long")
	if h.get(other).Status.Terminal() {
		t.Error("an invalid cancel stopped the task")
	}
	// Without a reason, the manager's default one is used.
	callOK(t, cs, "cancel_task", map[string]any{"task_id": other}, &ok)
	h.waitStatus(other, events.StatusCancelled)
}

func TestAnswerTask(t *testing.T) {
	h := newHarness(t)
	_, cs := h.bound("th")
	var started startTaskOut
	callOK(t, cs, "start_task", map[string]any{
		"kind": "demo.ask", "description": "Elegir perfil", "input": map[string]any{"question": "¿Qué perfil?", "options": []string{"Ana", "Luis"}},
	}, &started)
	h.waitStatus(started.TaskID, events.StatusWaitingInput)

	var v taskView
	callOK(t, cs, "get_task", map[string]any{"task_id": started.TaskID}, &v)
	if v.Question != "¿Qué perfil?" || len(v.Options) != 2 || v.Status != "waiting_input" {
		t.Errorf("waiting task view = %+v", v)
	}

	var ok okOut
	callOK(t, cs, "answer_task", map[string]any{"task_id": started.TaskID, "answer": "  Luis  "}, &ok)
	info := h.waitStatus(started.TaskID, events.StatusDone)
	if string(info.Result) != `{"answer":"Luis"}` {
		t.Errorf("result = %s, want the trimmed answer", info.Result)
	}
	// It no longer waits for an answer.
	mustContain(t, callErr(t, cs, "answer_task", map[string]any{"task_id": started.TaskID, "answer": "Ana"}), "not waiting")
}

func TestAnswerTaskRejectsBadAnswers(t *testing.T) {
	h := newHarness(t)
	_, cs := h.bound("th")
	id := h.startDirect(events.TaskSpec{Kind: "demo.ask", Input: json.RawMessage(`{"question":"q"}`)})
	h.waitStatus(id, events.StatusWaitingInput)
	for _, args := range []map[string]any{
		{"task_id": id},
		{"task_id": id, "answer": ""},
		{"task_id": id, "answer": " \n\t "},
		{"task_id": id, "answer": strings.Repeat("a", maxAnswerRunes+1)},
		{"task_id": id, "answer": 42},
	} {
		callErr(t, cs, "answer_task", args)
	}
	if h.get(id).Status != events.StatusWaitingInput {
		t.Error("an invalid answer reached the task")
	}
	// The longest answer, multi-line, is fine: people say long things.
	var ok okOut
	callOK(t, cs, "answer_task", map[string]any{"task_id": id, "answer": strings.Repeat("ñ", maxAnswerRunes-1) + "\n"}, &ok)
	h.waitStatus(id, events.StatusDone)
}

// The LLM must never confirm a sensitive action by itself: it may only answer
// tasks it knows are not sensitive.
func TestAnswerTaskRefusesWhatOnlyAPersonMayAnswer(t *testing.T) {
	h := newHarness(t)
	_, cs := h.bound("th")
	q := json.RawMessage(`{"question":"Confirm?"}`)
	cases := map[string]string{
		"sensitive kind":    h.startDirect(events.TaskSpec{Kind: "test.lock", Input: q, ThreadID: "th"}),
		"sensitive task":    h.startDirect(events.TaskSpec{Kind: "demo.ask", Input: q, Sensitive: true, ThreadID: "th"}),
		"unknown kind":      h.startDirect(events.TaskSpec{Kind: "other.ask", Input: q, ThreadID: "th"}),
		"unknown, no input": h.startDirect(events.TaskSpec{Kind: "other.ask"}),
	}
	for name, id := range cases {
		h.waitStatus(id, events.StatusWaitingInput)
		mustContain(t, callErr(t, cs, "answer_task", map[string]any{"task_id": id, "answer": "yes"}), "answered by the user directly")
		if got := h.get(id).Status; got != events.StatusWaitingInput {
			t.Errorf("%s: task is %s after a refused answer", name, got)
		}
	}
	// A person, through the task command, still can.
	id := cases["sensitive kind"]
	if _, err := bus.Request(context.Background(), h.direct, events.AnswerTask, events.TaskAnswer{TaskID: id, Answer: "yes"}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(id, events.StatusDone)
}

func TestCleanLineAndTruncate(t *testing.T) {
	for in, want := range map[string]string{"": "", "  a b  ": "a b", "ñandú": "ñandú"} {
		if got, err := cleanLine(in, "f", 10); err != nil || got != want {
			t.Errorf("cleanLine(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"a\nb", "a\rb", "a\x00b", "a\u0085b", "\xff\xfe", strings.Repeat("ñ", 11)} {
		if _, err := cleanLine(in, "f", 10); err == nil {
			t.Errorf("cleanLine(%q) accepted", in)
		}
	}
	if got := truncate("ñandú", 3); got != "ñan…" {
		t.Errorf("truncate = %q", got)
	}
	if got := truncate("abc", 3); got != "abc" {
		t.Errorf("truncate at the limit = %q", got)
	}
	if got := truncate("", 0); got != "" {
		t.Errorf("truncate empty = %q", got)
	}
}
