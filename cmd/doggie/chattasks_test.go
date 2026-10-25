package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

// env builds a task event as the chat receives it.
func env[T any](s bus.Subject[T], data T) bus.Envelope {
	raw, _ := json.Marshal(data)
	return bus.Envelope{Meta: bus.Meta{Type: s.Type()}, Data: raw}
}

func created(id, thread, desc string) bus.Envelope {
	return env(events.TaskCreated, events.TaskInfo{ID: id, TaskSpec: events.TaskSpec{Kind: "demo.sleep", ThreadID: thread, Description: desc}, Status: events.StatusPending})
}

func progress(id, msg string, pct int) bus.Envelope {
	return env(events.TaskProgress, events.TaskProgressData{TaskID: id, Message: msg, Percent: pct})
}

func TestBoardFollowsATask(t *testing.T) {
	b := newTaskBoard("terminal")
	b.event(created("t3", "terminal", "Esperar un rato"))
	if got := b.status(); got != "· t3 Esperar un rato" {
		t.Errorf("status = %q", got)
	}
	b.event(env(events.TaskRunning, events.TaskRef{TaskID: "t3"}))
	b.event(progress("t3", "3/30 s", 10))
	if got := b.status(); got != "· t3 Esperar un rato: 3/30 s (10%)" {
		t.Errorf("status = %q", got)
	}
	b.event(progress("t3", "al 40%", 40))
	if got := b.status(); got != "· t3 Esperar un rato: al 40%" {
		t.Errorf("status = %q", got)
	}
	b.event(env(events.TaskDone, events.TaskDoneData{TaskID: "t3"}))
	if got := b.status(); got != "" {
		t.Errorf("status after the end = %q", got)
	}
	// Late events don't bring it back.
	for _, late := range []bus.Envelope{env(events.TaskRunning, events.TaskRef{TaskID: "t3"}), progress("t3", "4/30 s", 13), created("t3", "terminal", "x")} {
		if q, lookup := b.event(late); q != "" || lookup != "" {
			t.Errorf("late %s: question %q, lookup %q", late.Type, q, lookup)
		}
	}
	if got := b.status(); got != "" {
		t.Errorf("status after late events = %q", got)
	}
}

func TestBoardStatusLine(t *testing.T) {
	b := newTaskBoard("terminal")
	if got := b.status(); got != "" {
		t.Errorf("empty status = %q", got)
	}
	b.event(created("t20", "voice", ""))
	b.event(created("t21", "", ""))
	if got := b.status(); got != "· 2 tasks in other conversations" {
		t.Errorf("status = %q", got)
	}
	b.event(created("t10", "terminal", strings.Repeat("larga ", 20)))
	b.event(created("t9", "terminal", "Una\ttarea\ncon saltos"))
	b.event(env(events.TaskNeedsInput, events.TaskNeedsInputData{TaskID: "t9", Question: "¿Sigo?"}))
	want := "· t9 Una tarea con saltos: asks (/answer t9 …) · t10 larga larga larga larga larga… · +2 elsewhere"
	if got := b.status(); got != want {
		t.Errorf("status = %q\nwant     %q", got, want)
	}
	b.event(progress("t9", "1/3", 0))
	if got := b.status(); strings.Contains(got, "asks") {
		t.Errorf("status still asks after the task went on: %q", got)
	}
}

// task.running follows an answer, but also the start of a task, which may
// arrive after its first question: the manager tells which.
func TestBoardAsksTheManagerWhatRunningMeans(t *testing.T) {
	b := newTaskBoard("terminal")
	b.event(created("t1", "terminal", ""))
	b.event(env(events.TaskNeedsInput, events.TaskNeedsInputData{TaskID: "t1", Question: "¿Sigo?"}))
	_, lookup := b.event(env(events.TaskRunning, events.TaskRef{TaskID: "t1"}))
	if lookup != "t1" {
		t.Fatalf("lookup = %q, want t1", lookup)
	}
	if _, again := b.event(env(events.TaskRunning, events.TaskRef{TaskID: "t1"})); again != "" {
		t.Errorf("looked up twice")
	}
	// It was the start: the question still waits.
	b.looked(boardLookup{id: "t1", info: events.TaskInfo{ID: "t1", Status: events.StatusWaitingInput, Question: "¿Sigo?"}})
	if got := b.status(); !strings.Contains(got, "t1 demo.sleep: asks") {
		t.Errorf("status = %q", got)
	}
	// The manager hadn't heard of the question yet: look again, which tells.
	_, lookup = b.event(env(events.TaskRunning, events.TaskRef{TaskID: "t1"}))
	running := boardLookup{id: lookup, info: events.TaskInfo{ID: "t1", Status: events.StatusRunning, Progress: "2/3"}}
	if _, again := b.looked(running); again != "t1" {
		t.Fatalf("again = %q, want t1", again)
	}
	if got := b.status(); !strings.Contains(got, "asks") {
		t.Errorf("a first look dropped the question: %q", got)
	}
	b.looked(boardLookup{id: "t1", info: events.TaskInfo{ID: "t1", Status: events.StatusWaitingInput, Question: "¿Sigo?"}})
	if got := b.status(); !strings.Contains(got, "asks") {
		t.Errorf("status = %q", got)
	}
	// It was the answer: the second look says so too.
	b.event(env(events.TaskRunning, events.TaskRef{TaskID: "t1"}))
	if _, again := b.looked(running); again != "t1" {
		t.Fatalf("again = %q, want t1", again)
	}
	if _, again := b.looked(running); again != "" {
		t.Errorf("looked a third time")
	}
	if got := b.status(); got != "· t1 demo.sleep: 2/3" {
		t.Errorf("status = %q", got)
	}
	// It ended meanwhile.
	b.event(env(events.TaskNeedsInput, events.TaskNeedsInputData{TaskID: "t1", Question: "¿Y ahora?"}))
	_, lookup = b.event(env(events.TaskRunning, events.TaskRef{TaskID: "t1"}))
	b.looked(boardLookup{id: lookup, info: events.TaskInfo{ID: "t1", Status: events.StatusDone}})
	if got := b.status(); got != "" {
		t.Errorf("status = %q", got)
	}
	if _, lookup := b.event(progress("t1", "late", 0)); lookup != "" {
		t.Errorf("an ended task is looked up again")
	}
}

// A question only the user may answer, asked before the chat opened, is
// told once the chat lists the tasks.
func TestBoardTellsQuestionsFoundInALists(t *testing.T) {
	b := newTaskBoard("terminal")
	waiting := events.TaskInfo{ID: "t5", TaskSpec: events.TaskSpec{Kind: "demo.ask", ThreadID: "terminal", Sensitive: true}, Status: events.StatusWaitingInput, Question: "¿Pago?"}
	other := events.TaskInfo{ID: "t6", TaskSpec: events.TaskSpec{Kind: "demo.ask", ThreadID: "terminal"}, Status: events.StatusWaitingInput, Question: "¿Cuál?"}
	qs := b.merge(boardList{asked: time.Now(), tasks: []events.TaskInfo{waiting, other}})
	if len(qs) != 1 || !strings.HasPrefix(qs[0], `· t5 asks: "¿Pago?"`) {
		t.Errorf("questions = %q", qs)
	}
	// Told already: not again, e.g. after a reconnection.
	b.clear()
	time.Sleep(time.Millisecond)
	if qs := b.merge(boardList{asked: time.Now(), tasks: []events.TaskInfo{waiting}}); len(qs) != 0 {
		t.Errorf("told again: %q", qs)
	}
	if q, _ := b.event(env(events.TaskNeedsInput, events.TaskNeedsInputData{TaskID: "t5", Question: "¿Pago?"})); q != "" {
		t.Errorf("told again: %q", q)
	}
	// A new question is told.
	if q, _ := b.event(env(events.TaskNeedsInput, events.TaskNeedsInputData{TaskID: "t5", Question: "¿Seguro?"})); q == "" {
		t.Error("a second question wasn't told")
	}
}

func TestBoardTellsQuestionsOnlyTheUserMayAnswer(t *testing.T) {
	b := newTaskBoard("terminal")
	sensitive := events.TaskInfo{ID: "t1", TaskSpec: events.TaskSpec{Kind: "demo.ask", ThreadID: "terminal", Sensitive: true}}
	b.event(env(events.TaskCreated, sensitive))
	q, _ := b.event(env(events.TaskNeedsInput, events.TaskNeedsInputData{TaskID: "t1", Question: "¿Confirmas\nel pago?", Options: []string{"sí", "no"}}))
	if want := `· t1 asks: "¿Confirmas el pago?" [sí | no]. Only you may answer it: /answer t1 <answer>`; q != want {
		t.Errorf("question = %q\nwant       %q", q, want)
	}

	// The LLM may answer this one, and tells the user itself.
	b.event(env(events.TaskCreated, events.TaskInfo{ID: "t2", TaskSpec: events.TaskSpec{Kind: "demo.ask", ThreadID: "terminal"}}))
	if q, _ := b.event(env(events.TaskNeedsInput, events.TaskNeedsInputData{TaskID: "t2", Question: "¿Cuál?"})); q != "" {
		t.Errorf("told a question the LLM may answer: %q", q)
	}
	// A kind the LLM may not start, e.g. from doggie task start.
	b.event(env(events.TaskCreated, events.TaskInfo{ID: "t3", TaskSpec: events.TaskSpec{Kind: "other.kind", ThreadID: "terminal"}}))
	if q, _ := b.event(env(events.TaskNeedsInput, events.TaskNeedsInputData{TaskID: "t3", Question: "¿Seguro?"})); !strings.HasPrefix(q, "· t3 asks") {
		t.Errorf("question of an unknown kind = %q", q)
	}
	// Another conversation's question is told there.
	b.event(env(events.TaskCreated, events.TaskInfo{ID: "t4", TaskSpec: events.TaskSpec{Kind: "demo.ask", ThreadID: "voice", Sensitive: true}}))
	if q, _ := b.event(env(events.TaskNeedsInput, events.TaskNeedsInputData{TaskID: "t4", Question: "¿Abro?"})); q != "" {
		t.Errorf("told another conversation's question: %q", q)
	}
}

func TestBoardLooksUpUnknownTasks(t *testing.T) {
	b := newTaskBoard("terminal")
	q, lookup := b.event(progress("t7", "1/5 s", 20))
	if q != "" || lookup != "t7" {
		t.Fatalf("question %q, lookup %q; want a lookup of t7", q, lookup)
	}
	// Events while it is looked up wait for it, without a second lookup.
	if _, lookup := b.event(env(events.TaskNeedsInput, events.TaskNeedsInputData{TaskID: "t7", Question: "¿Sigo?"})); lookup != "" {
		t.Errorf("second lookup %q", lookup)
	}
	info := events.TaskInfo{ID: "t7", TaskSpec: events.TaskSpec{Kind: "demo.ask", ThreadID: "terminal", Sensitive: true}, Status: events.StatusRunning}
	questions, _ := b.looked(boardLookup{id: "t7", info: info})
	if len(questions) != 1 || !strings.HasPrefix(questions[0], "· t7 asks") {
		t.Errorf("questions = %q", questions)
	}
	if got := b.status(); !strings.Contains(got, "t7 demo.ask: asks") {
		t.Errorf("status = %q", got)
	}

	// A task that ended meanwhile, or can't be looked up, isn't shown.
	b.event(progress("t8", "x", 0))
	b.looked(boardLookup{id: "t8", info: events.TaskInfo{ID: "t8", Status: events.StatusDone}})
	b.event(progress("t9", "x", 0))
	b.looked(boardLookup{id: "t9", err: errors.New("timeout")})
	if got := b.status(); strings.Contains(got, "t8") || strings.Contains(got, "t9") {
		t.Errorf("status = %q", got)
	}
	if _, lookup := b.event(progress("t8", "y", 0)); lookup != "" {
		t.Errorf("a task seen ended is looked up again")
	}
	// An end of a task never seen needs no lookup.
	if _, lookup := b.event(env(events.TaskFailed, events.TaskFailedData{TaskID: "t11", Error: "x"})); lookup != "" {
		t.Errorf("looked up a task that ended")
	}
	if _, lookup := b.event(progress("t11", "late", 0)); lookup != "" {
		t.Errorf("looked up a task that ended")
	}
	// Garbage is ignored.
	for _, bad := range []bus.Envelope{{Meta: bus.Meta{Type: "task.progress"}, Data: json.RawMessage(`{`)}, env(events.TaskProgress, events.TaskProgressData{})} {
		if q, lookup := b.event(bad); q != "" || lookup != "" {
			t.Errorf("garbage gave %q, %q", q, lookup)
		}
	}
}

func TestBoardMergesLists(t *testing.T) {
	b := newTaskBoard("terminal")
	b.event(created("t1", "terminal", "vieja"))
	b.event(created("t2", "terminal", "terminada"))
	b.event(env(events.TaskDone, events.TaskDoneData{TaskID: "t2"}))
	asked := time.Now()
	time.Sleep(time.Millisecond)
	b.event(created("t3", "terminal", "nueva"))
	b.event(progress("t3", "1/5 s", 0))
	b.merge(boardList{asked: asked, tasks: []events.TaskInfo{
		{ID: "t2", TaskSpec: events.TaskSpec{ThreadID: "terminal"}, Status: events.StatusRunning},
		{ID: "t3", TaskSpec: events.TaskSpec{ThreadID: "terminal", Description: "nueva"}, Status: events.StatusPending},
		{ID: "t4", TaskSpec: events.TaskSpec{ThreadID: "terminal", Description: "otra"}, Status: events.StatusRunning, Progress: "2/9"},
	}})
	// t1 ended unseen; t2 was seen ending; t3 is newer than the list.
	if got, want := b.status(), "· t3 nueva: 1/5 s · t4 otra: 2/9"; got != want {
		t.Errorf("status = %q, want %q", got, want)
	}
	b.merge(boardList{err: errors.New("no manager")})
	if got := b.status(); !strings.Contains(got, "t4") {
		t.Errorf("a failed list changed the board: %q", got)
	}
	b.clear()
	if got := b.status(); got != "" {
		t.Errorf("status after clear = %q", got)
	}
}

func TestBoardIsBounded(t *testing.T) {
	b := newTaskBoard("terminal")
	for i := range 2 * maxBoardTasks {
		b.event(created(fmt.Sprintf("t%d", i), "voice", ""))
		b.event(progress(fmt.Sprintf("u%d", i), "x", 0))
	}
	if len(b.tasks) > maxBoardTasks || len(b.looking) > maxBoardTasks {
		t.Errorf("%d tasks and %d lookups", len(b.tasks), len(b.looking))
	}
}

func TestCompareIDs(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{{"t2", "t10", -1}, {"t10", "t2", 1}, {"t3", "t3", 0}, {"x", "t1", 1}, {"t9223372036854775807", "t1", 1}} {
		if got := compareIDs(tt.a, tt.b); (got > 0) != (tt.want > 0) || (got < 0) != (tt.want < 0) {
			t.Errorf("compareIDs(%s, %s) = %d", tt.a, tt.b, got)
		}
	}
}

// The chat shows a task's progress on its status line, tells a question
// only the user may answer, and clears the line when the task ends.
func TestChatShowsTasksLive(t *testing.T) {
	c, ui := chatOnTasks(t)
	id := startTask(t, c, events.TaskSpec{Kind: "demo.sleep", Input: json.RawMessage(`{"seconds":2}`), Description: "Esperar", ThreadID: "terminal"})
	ui.waitStatus(t, "· "+id+" Esperar: 1/2 s (50%)")
	ui.waitStatus(t, "")

	ask := startTask(t, c, events.TaskSpec{Kind: "demo.ask", Input: json.RawMessage(`{"question":"¿Confirmas?"}`), ThreadID: "terminal", Sensitive: true})
	ui.waitPrinted(t, "· "+ask+` asks: "¿Confirmas?". Only you may answer it: /answer `+ask+" <answer>\n")
	ui.waitStatus(t, "· "+ask+" demo.ask: asks (/answer "+ask+" …)")
	ui.lines <- "/answer " + ask + " sí"
	ui.waitStatus(t, "")
}

// Tasks already running when the chat opens are on the status line.
func TestChatListsTasksOnOpening(t *testing.T) {
	c := taskEnv(t)
	waitForWorker(t, c)
	waitNoActiveTasks(t, c)
	id := startTask(t, c, events.TaskSpec{Kind: "demo.sleep", Input: json.RawMessage(`{"seconds":60}`), Description: "Larga", ThreadID: "terminal"})
	waitStatus(t, c, id, events.StatusRunning)
	ui := newFakeUI(true)
	states := make(chan connState, 4)
	runChat(t, c, ui, states, testChatOptions())
	ui.waitStatusPrefix(t, "· "+id+" Larga")

	// What is known may be stale while the connection is down.
	states <- connState{kind: connDown}
	ui.waitStatus(t, "")
	states <- connState{kind: connUp}
	ui.waitStatusPrefix(t, "· "+id+" Larga")
}

// Many progress reports in a row don't redraw the line each time, and the
// last one shows.
func TestChatStatusIsThrottled(t *testing.T) {
	client := sayEnv(t, nil)
	ui := newFakeUI(true)
	runChat(t, client, ui, make(chan connState), testChatOptions())
	ui.lines <- "/help" // once shown, the chat listens
	ui.waitPrinted(t, chatHelp)
	ctx := context.Background()
	bus.Publish(ctx, client, events.TaskCreated, events.TaskInfo{ID: "t1", TaskSpec: events.TaskSpec{Kind: "demo.sleep", ThreadID: "terminal"}})
	for i := 1; i <= 50; i++ {
		bus.Publish(ctx, client, events.TaskProgress, events.TaskProgressData{TaskID: "t1", Message: fmt.Sprintf("%d/50", i)})
	}
	ui.waitStatus(t, "· t1 demo.sleep: 50/50")
	ui.mu.Lock()
	n := len(ui.statuses)
	ui.mu.Unlock()
	if n > 5 {
		t.Errorf("the status line changed %d times", n)
	}
}

// waitStatusPrefix waits until the status line starts with prefix.
func (u *fakeUI) waitStatusPrefix(t *testing.T, prefix string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.HasPrefix(u.status(), prefix) {
		if time.Now().After(deadline) {
			t.Fatalf("status %q, want it to start with %q", u.status(), prefix)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
