package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

func TestParseLine(t *testing.T) {
	for _, tt := range []struct {
		line, sentence string
		cmd            *command
	}{
		{"  recuérdame comprar pan ", "recuérdame comprar pan", nil},
		{"//etc/hosts tiene un error", "/etc/hosts tiene un error", nil},
		{"//", "/", nil},
		{"/tasks", "", &command{name: "/tasks"}},
		{"  /tasks   all ", "", &command{name: "/tasks", text: "all"}},
		{"/answer t3   sí,  claro ", "", &command{name: "/answer", id: "t3", text: "sí,  claro"}},
		{"/answer\tt3\tsí", "", &command{name: "/answer", id: "t3", text: "sí"}},
		{"/answer t3", "", &command{name: "/answer", id: "t3"}},
		{"/cancel t3 ya no hace falta", "", &command{name: "/cancel", id: "t3", text: "ya no hace falta"}},
		{"/", "", &command{name: "/"}},
		{"/Tasks", "", &command{name: "/Tasks"}},
	} {
		sentence, cmd := parseLine(tt.line)
		if sentence != tt.sentence || (cmd == nil) != (tt.cmd == nil) || (cmd != nil && (cmd.name != tt.cmd.name || cmd.id != tt.cmd.id || cmd.text != tt.cmd.text)) {
			t.Errorf("parseLine(%q) = %q, %+v; want %q, %+v", tt.line, sentence, cmd, tt.sentence, tt.cmd)
		}
	}
}

func TestCommandCheck(t *testing.T) {
	for line, want := range map[string]string{
		"/help":         "",
		"/tasks":        "",
		"/tasks all":    "",
		"/tasks t3":     "usage: /tasks [all]",
		"/answer":       "usage: /answer",
		"/answer t3":    "usage: /answer",
		"/answer t3 sí": "",
		"/cancel":       "usage: /cancel",
		"/cancel t3":    "",
		"/":             "unknown command /;",
		"/borrar t3":    "unknown command /borrar;",
		"/Tasks":        "unknown command /Tasks;",
	} {
		_, cmd := parseLine(line)
		got := cmd.check()
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("check(%q) = %q, want %q", line, got, want)
		}
	}
}

// chatOnTasks runs an interactive chat with the task manager and the demo
// worker.
func chatOnTasks(t *testing.T) (*bus.Conn, *fakeUI) {
	t.Helper()
	c := taskEnv(t)
	waitForWorker(t, c)
	waitNoActiveTasks(t, c)
	ui := newFakeUI(true)
	runChat(t, c, ui, make(chan connState), testChatOptions())
	return c, ui
}

func startTask(t *testing.T, c *bus.Conn, spec events.TaskSpec) string {
	t.Helper()
	ref, err := bus.Request(context.Background(), c, events.StartTask, spec)
	if err != nil {
		t.Fatal(err)
	}
	return ref.TaskID
}

func TestChatAnswersTasksDirectly(t *testing.T) {
	c, ui := chatOnTasks(t)
	id := startTask(t, c, events.TaskSpec{Kind: "demo.ask", Input: json.RawMessage(`{"question":"¿Confirmas el pago?","options":["sí","no"]}`), ThreadID: "terminal", Sensitive: true})
	waitStatus(t, c, id, events.StatusWaitingInput)

	ui.lines <- "/answer " + id + " Sí"
	ui.waitPrinted(t, "· answered "+id+"\n")
	waitStatus(t, c, id, events.StatusDone)
	ui.lines <- "/answer " + id + " otra vez"
	ui.waitPrinted(t, "· could not answer "+id+": it isn't waiting for an answer (it is done)\n")

	// An answer that isn't one of the options is delivered, with a warning:
	// the task decides what it means.
	other := startTask(t, c, events.TaskSpec{Kind: "demo.ask", Input: json.RawMessage(`{"question":"¿Cuál?","options":["rojo","azul"]}`)})
	waitStatus(t, c, other, events.StatusWaitingInput)
	ui.lines <- "/answer " + other + " verde, mejor"
	ui.waitPrinted(t, "· answered "+other+", though not with one of its options (rojo | azul)\n")
	waitStatus(t, c, other, events.StatusDone)
	info, _ := bus.Request(context.Background(), c, events.GetTask, events.TaskRef{TaskID: other})
	if string(info.Result) != `{"answer":"verde, mejor"}` {
		t.Errorf("result = %s", info.Result)
	}

	ui.lines <- "/answer t999 sí"
	ui.waitPrinted(t, "· could not answer t999: unknown task")
	ui.lines <- "/answer " + id
	ui.waitPrinted(t, "· usage: /answer <id> <text>")
}

func TestChatListsAndCancelsTasks(t *testing.T) {
	c, ui := chatOnTasks(t)
	ui.lines <- "/tasks"
	ui.waitPrinted(t, "· no tasks in progress\n")

	mine := startTask(t, c, events.TaskSpec{Kind: "demo.sleep", Input: json.RawMessage(`{"seconds":60}`), Description: "Esperar\tun minuto", ThreadID: "terminal"})
	theirs := startTask(t, c, events.TaskSpec{Kind: "demo.sleep", Input: json.RawMessage(`{"seconds":60}`), ThreadID: "voice"})
	nobody := startTask(t, c, events.TaskSpec{Kind: "demo.sleep", Input: json.RawMessage(`{"seconds":60}`)})
	for _, id := range []string{mine, theirs, nobody} {
		waitStatus(t, c, id, events.StatusRunning)
	}
	ui.lines <- "/tasks"
	ui.waitPrinted(t, "· 3 tasks in progress:\n")
	out := ui.printed()
	for _, want := range []string{"Esperar un minuto", "demo.sleep (conversation voice)", "demo.sleep (no conversation)"} {
		if !strings.Contains(out, want) {
			t.Errorf("task list lacks %q:\n%s", want, out)
		}
	}

	// Commands run in order: the list after a cancellation doesn't show it.
	ui.lines <- "/cancel " + mine + " ya no"
	ui.lines <- "/tasks"
	ui.waitPrinted(t, "· cancelled "+mine+"\n· 2 tasks in progress:\n")
	ev, _, _ := bus.LastEvent(context.Background(), c, events.TaskCancelled)
	if ev.Data.TaskID != mine || ev.Data.Reason != "ya no" || ev.Data.Channel != "terminal" {
		t.Errorf("task.cancelled = %+v, want %s from the terminal", ev.Data, mine)
	}
	ui.lines <- "/cancel " + mine
	ui.waitPrinted(t, "· could not cancel "+mine+": task "+mine+" is already cancelled\n")

	ui.lines <- "/tasks all"
	ui.waitPrinted(t, "· the last ")
	if !strings.Contains(ui.printed(), "cancelled") {
		t.Errorf("/tasks all doesn't show the cancelled task:\n%s", ui.printed())
	}
}

func TestChatCommandsWithoutDOGGIE(t *testing.T) {
	client := sayEnv(t, nil) // a bus without the task manager
	ui := newFakeUI(true)
	runChat(t, client, ui, make(chan connState), testChatOptions())
	ui.lines <- "/tasks"
	ui.waitPrinted(t, "· could not list the tasks: DOGGIE's task manager isn't running\n")
	ui.lines <- "/help"
	ui.waitPrinted(t, chatHelp+"\n")
	ui.lines <- "/borrar todo"
	ui.waitPrinted(t, "· unknown command /borrar;")
}

// Commands never reach the brain; "//" says a sentence starting with "/".
func TestChatCommandsAreNotSaid(t *testing.T) {
	said := make(chan string, 10)
	client := sayEnv(t, func(ctx context.Context, c *bus.Conn, u bus.Event[events.UtteranceData]) {
		said <- u.Data.Text
		streamReply(ctx, c, u.Data.ThreadID, u.ID, "ok")
	})
	ui := newFakeUI(true)
	runChat(t, client, ui, make(chan connState), testChatOptions())
	ui.lines <- "/help"
	ui.lines <- "/answer t1 sí"
	ui.lines <- "//help no es un comando"
	select {
	case s := <-said:
		if s != "/help no es un comando" {
			t.Errorf("said %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the // sentence wasn't said")
	}
	select {
	case s := <-said:
		t.Errorf("also said %q", s)
	case <-time.After(200 * time.Millisecond):
	}
}

// Piped, commands print their output and a failed one fails the chat.
func TestPipedChatRunsCommands(t *testing.T) {
	c := taskEnv(t)
	waitForWorker(t, c)
	id := startTask(t, c, events.TaskSpec{Kind: "demo.ask", Input: json.RawMessage(`{"question":"¿Sigo?"}`)})
	waitStatus(t, c, id, events.StatusWaitingInput)

	ui := newFakeUI(false)
	done, _ := runChat(t, c, ui, make(chan connState), testChatOptions())
	ui.lines <- "/answer " + id + " sí"
	close(ui.lines)
	if err := result(t, done); err != nil {
		t.Fatalf("chat: %v\n%s", err, ui.printed())
	}
	if !strings.Contains(ui.printed(), "· answered "+id+"\n") {
		t.Errorf("printed %q", ui.printed())
	}

	ui = newFakeUI(false)
	done, _ = runChat(t, c, ui, make(chan connState), testChatOptions())
	ui.lines <- "/cancel " + id
	close(ui.lines)
	if err := result(t, done); err == nil {
		t.Errorf("a failed command didn't fail the piped chat:\n%s", ui.printed())
	}
	ui = newFakeUI(false)
	done, _ = runChat(t, c, ui, make(chan connState), testChatOptions())
	ui.lines <- "/answer " + id
	close(ui.lines)
	if err := result(t, done); err == nil {
		t.Errorf("a wrong command didn't fail the piped chat:\n%s", ui.printed())
	}
}

// waitNoActiveTasks waits until every task, e.g. waitForWorker's, ended.
func waitNoActiveTasks(t *testing.T, c *bus.Conn) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		list, err := bus.Request(context.Background(), c, events.ListTasks, events.TaskListRequest{ActiveOnly: true})
		if err == nil && len(list.Tasks) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("tasks still active: %+v, %v", list.Tasks, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestTaskTableHasNoTrailingSpaces(t *testing.T) {
	out := taskTable(events.TaskList{Tasks: []events.TaskInfo{{ID: "t1", TaskSpec: events.TaskSpec{Kind: "demo.sleep"}, Status: events.StatusPending}}, Total: 1}, true, "terminal", time.Now())
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimRight(line, " ") != line {
			t.Errorf("trailing spaces in %q", line)
		}
	}
}
