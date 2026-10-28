package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/console"
)

// fakeScreen is a full screen the test types into, keeping each call as a
// line, e.g. "stream id text".
type fakeScreen struct {
	*fakeUI
	cmu   sync.Mutex
	calls []string
}

func newFakeScreen() *fakeScreen { return &fakeScreen{fakeUI: newFakeUI(true)} }

func (s *fakeScreen) call(f string, args ...any) {
	s.cmu.Lock()
	defer s.cmu.Unlock()
	s.calls = append(s.calls, fmt.Sprintf(f, args...))
}

func (s *fakeScreen) Stream(id, text string)       { s.call("stream %s %s", id, text) }
func (s *fakeScreen) Reply(id, text string)        { s.call("reply %s %s", id, text) }
func (s *fakeScreen) Said(text string)             { s.call("said %s", text) }
func (s *fakeScreen) SaidElsewhere(l, text string) { s.call("elsewhere %s: %s", l, text) }
func (s *fakeScreen) SetActivity(text string)      { s.call("activity %s", text) }
func (s *fakeScreen) SetConnection(up bool, status string) {
	s.call("online %v %s", up, status)
}
func (s *fakeScreen) Notice(id string, kind console.NoticeKind, text string) {
	s.call("notice %s %d %s", id, kind, text)
}

// waitCalls waits until the calls made include each of want, in order.
func (s *fakeScreen) waitCalls(t *testing.T, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.cmu.Lock()
		got := slices.Clone(s.calls)
		s.cmu.Unlock()
		i := 0
		for _, c := range got {
			if i < len(want) && c == want[i] {
				i++
			}
		}
		if i == len(want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("calls:\n%s\nwant in order:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestChatOnAScreen(t *testing.T) {
	brains := make(chan *bus.Conn, 1)
	client := sayEnv(t, func(ctx context.Context, c *bus.Conn, u bus.Event[events.UtteranceData]) {
		streamReply(ctx, c, u.Data.ThreadID, u.ID, "Voy.", "Listo, te aviso.")
		select {
		case brains <- c:
		default:
		}
	})
	ui := newFakeScreen()
	o := testChatOptions()
	o.words = words("es")
	runChat(t, client, ui, make(chan connState), o)
	ui.waitCalls(t, "online true ")
	ui.lines <- "recuérdame sacar la ropa"
	ui.waitCalls(t, "activity pensando")
	brain := <-brains
	ui.cmu.Lock()
	var cause string
	for _, c := range ui.calls {
		if id, ok := strings.CutPrefix(c, "reply "); ok {
			cause, _, _ = strings.Cut(id, "#")
			break
		}
	}
	ui.cmu.Unlock()
	ui.waitCalls(t, "reply "+cause+"#0 Voy.", "reply "+cause+"#1 Listo, te aviso.")
	if strings.Contains(ui.printed(), "doggie>") {
		t.Errorf("printed plain lines: %q", ui.printed())
	}

	bus.Publish(context.Background(), brain, events.UserUtterance.With("terminal"), events.UtteranceData{ThreadID: "terminal", Channel: "terminal", Text: "desde otra ventana"})
	ui.waitCalls(t, "elsewhere tú, en otra ventana: desde otra ventana")
	ui.lines <- "/nope"
	ui.waitCalls(t, `notice  3 comando desconocido /nope; /help los muestra, y //texto dice un texto que empieza con "/"`)
}

func TestChatHistoryOnAScreen(t *testing.T) {
	client := sayEnv(t, nil)
	logTurn(t, client, events.BrainTurnData{ThreadID: "terminal", Inputs: []events.TurnInput{
		{Said: "frase vieja", Channel: "terminal"},
		{Said: "desde la cocina", Channel: "voice:kitchen"},
		{Task: &events.TurnTask{Type: "task.done", TaskID: "t3", Description: "Esperar un rato"}},
	}, Replies: []string{"respuesta vieja"}, Error: "the LLM stopped"})
	ui := newFakeScreen()
	runChat(t, client, ui, make(chan connState), historyOptions(5))
	ui.waitCalls(t,
		"said frase vieja",
		"elsewhere voice:kitchen: desde la cocina",
		"notice  0 t3 (Esperar un rato) finished",
		"reply past:0#0 respuesta vieja",
		"notice  3 the turn failed: the LLM stopped",
	)
	if strings.Contains(ui.printed(), "earlier in this conversation") {
		t.Errorf("printed plain history: %q", ui.printed())
	}
}

func TestChatConnectionOnAScreen(t *testing.T) {
	client := sayEnv(t, nil)
	ui := newFakeScreen()
	states := make(chan connState, 4)
	runChat(t, client, ui, states, testChatOptions())
	ui.waitCalls(t, "online true ")
	states <- connState{kind: connDown}
	ui.waitCalls(t, "online true ", "online false reconnecting…")
	states <- connState{kind: connUp}
	ui.waitCalls(t, "online false reconnecting…", "online true ")
}

func TestWords(t *testing.T) {
	es, en := words("es-PE"), words("fr")
	if es.thinking != "pensando" || en.thinking != "thinking" || es.screen.Newer(1) != "↓ 1 nuevo" || es.screen.Newer(3) != "↓ 3 nuevos" {
		t.Errorf("%+v %+v", es.thinking, en.thinking)
	}
	if got := spanishClock(time.Date(2026, 10, 28, 21, 40, 0, 0, time.UTC)); got != "mié 28 oct · 21:40" {
		t.Errorf("clock %q", got)
	}
}

func (s *fakeScreen) SetTasks(tasks []console.TaskView, more string) {
	var parts []string
	for _, t := range tasks {
		parts = append(parts, fmt.Sprintf("%s|%s|%s|%d|%v", t.ID, t.Title, t.Detail, t.Percent, t.Waiting))
	}
	s.call("tasks %s %s", strings.Join(parts, ","), more)
}

func TestBoardViews(t *testing.T) {
	b := newTaskBoard("terminal")
	b.event(created("t10", "terminal", "Esperar  un\nrato"))
	b.event(created("t2", "terminal", ""))
	b.event(created("t7", "voice", "de otra"))
	b.event(created("t8", "voice", "y otra"))
	b.event(env(events.TaskRunning, events.TaskRunningData{TaskID: "t10"}))
	b.event(progress("t10", "3/30 s", 10))
	b.event(env(events.TaskNeedsInput, events.TaskNeedsInputData{TaskID: "t2", Question: "¿Pago la luz?"}))
	views, more := b.views(words("es"))
	want := []console.TaskView{
		{ID: "t2", Title: "demo.sleep", Detail: "espera tu respuesta · /answer t2", Waiting: true},
		{ID: "t10", Title: "Esperar un rato", Detail: "3/30 s", Percent: 10},
	}
	if !slices.Equal(views, want) || more != "+2 en otras conversaciones" {
		t.Errorf("%+v %q", views, more)
	}
	if _, more := newTaskBoard("terminal").views(words("en")); more != "" {
		t.Errorf("more %q without tasks", more)
	}
}

// The chat shows its tasks in the screen's panel as they go.
func TestChatTasksOnAScreen(t *testing.T) {
	client := sayEnv(t, nil)
	ui := newFakeScreen()
	runChat(t, client, ui, make(chan connState), testChatOptions())
	ui.waitCalls(t, "online true ")
	ctx := context.Background()
	bus.Publish(ctx, client, events.TaskCreated, events.TaskInfo{ID: "t1", TaskSpec: events.TaskSpec{Kind: "demo.sleep", ThreadID: "terminal", Description: "Recordar estirar"}})
	bus.Publish(ctx, client, events.TaskProgress, events.TaskProgressData{TaskID: "t1", Message: "5/10 s", Percent: 50})
	ui.waitCalls(t, "tasks t1|Recordar estirar|5/10 s|50|false ")
	bus.Publish(ctx, client, events.TaskDone, events.TaskDoneData{TaskID: "t1"})
	ui.waitCalls(t, "tasks t1|Recordar estirar|5/10 s|50|false ", "tasks  ")
}

func TestBoardDoingFor(t *testing.T) {
	b := newTaskBoard("terminal")
	start := func(id, desc string, causes ...string) {
		b.event(env(events.TaskCreated, events.TaskInfo{ID: id, TaskSpec: events.TaskSpec{Kind: "demo.sleep", ThreadID: "terminal", Description: desc, CausedBy: causes}, Status: events.StatusPending}))
	}
	start("t1", "Esperar un rato", "s1")
	sentences := map[string]time.Time{"s1": time.Now()}
	if got := b.doingFor(sentences); got != "" {
		t.Errorf("a pending task: %q", got)
	}
	b.event(env(events.TaskRunning, events.TaskRunningData{TaskID: "t1"}))
	start("t2", "", "s1")
	b.event(env(events.TaskRunning, events.TaskRunningData{TaskID: "t2"}))
	start("t3", "De otra frase", "s2")
	b.event(env(events.TaskRunning, events.TaskRunningData{TaskID: "t3"}))
	if got := b.doingFor(sentences); got != "demo.sleep" {
		t.Errorf("the newest task: %q", got)
	}
	if got := b.doingFor(map[string]time.Time{"s9": time.Now()}); got != "" {
		t.Errorf("another sentence: %q", got)
	}
}

// Every Spanish text takes the same arguments, in the same order, as the
// English one it translates.
func TestSpanishVerbs(t *testing.T) {
	verbs := func(s string) []string {
		var vs []string
		for i := 0; i < len(s)-1; i++ {
			if s[i] == '%' {
				vs = append(vs, s[i:i+2])
				i++
			}
		}
		return vs
	}
	for en, es := range spanish {
		if !slices.Equal(verbs(en), verbs(es)) {
			t.Errorf("%q takes %v, its translation %q %v", en, verbs(en), es, verbs(es))
		}
	}
	if got := spanish.f("%d tasks", 3); got != "3 tareas" {
		t.Errorf("%q", got)
	}
	if got := translator(nil).f("100%% done"); got != "100%% done" {
		t.Errorf("a text without arguments is kept as is: %q", got)
	}
}

// The chat's notes on a Spanish screen are in Spanish, with their kind.
func TestChatSaysInSpanish(t *testing.T) {
	client := sayEnv(t, nil)
	ui := newFakeScreen()
	o := testChatOptions()
	o.words = words("es")
	states := make(chan connState, 4)
	runChat(t, client, ui, states, o)
	ui.waitCalls(t, "online true ")
	states <- connState{kind: connDown}
	ui.waitCalls(t, "notice  2 se perdió la conexión con DOGGIE; reconectando…")
	states <- connState{kind: connUp}
	ui.waitCalls(t, "notice  1 conectado otra vez")
	ui.lines <- "/tasks"
	ui.waitCalls(t, "notice  3 no pude listar las tareas: el gestor de tareas de DOGGIE no está corriendo")
}
