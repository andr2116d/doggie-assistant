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
	ui.waitCalls(t, `notice  0 unknown command /nope; /help lists them, and //text says text starting with "/"`)
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
