package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

func logTurn(t *testing.T, c *bus.Conn, d events.BrainTurnData) {
	t.Helper()
	if _, err := bus.Publish(context.Background(), c, events.BrainTurn, d); err != nil {
		t.Fatal(err)
	}
}

func historyOptions(n int) chatOptions {
	o := testChatOptions()
	o.history = n
	return o
}

func TestChatShowsTheLastTurnsOnOpening(t *testing.T) {
	client := sayEnv(t, nil)
	for i := 1; i <= 6; i++ {
		logTurn(t, client, events.BrainTurnData{ThreadID: "terminal", Inputs: []events.TurnInput{{Said: fmt.Sprintf("frase %d", i), Channel: "terminal"}}, Replies: []string{fmt.Sprintf("respuesta %d", i)}})
	}
	logTurn(t, client, events.BrainTurnData{ThreadID: "voice", Inputs: []events.TurnInput{{Said: "de otra conversación"}}, Replies: []string{"no para ti"}})
	logTurn(t, client, events.BrainTurnData{ThreadID: "terminal"}) // nothing to show
	logTurn(t, client, events.BrainTurnData{ThreadID: "terminal", Inputs: []events.TurnInput{
		{Said: "  desde la\ncocina ", Channel: "voice:kitchen"},
		{Task: &events.TurnTask{Type: "task.done", TaskID: "t3", Description: "Esperar un rato"}},
		{Task: &events.TurnTask{Type: "task.failed", TaskID: "t4", Reason: "timed out"}},
	}, Replies: []string{"Listo.\nY otra cosa."}, Error: "the LLM stopped"})

	ui := newFakeUI(true)
	runChat(t, client, ui, make(chan connState), historyOptions(5))
	ui.waitPrinted(t, "· ───\n")
	want := "· earlier in this conversation, from 0s ago:\n" +
		"you> frase 3\ndoggie> respuesta 3\n" +
		"you> frase 4\ndoggie> respuesta 4\n" +
		"you> frase 5\ndoggie> respuesta 5\n" +
		"you> frase 6\ndoggie> respuesta 6\n" +
		"voice:kitchen> desde la cocina\n· t3 (Esperar un rato) finished\n· t4 failed: timed out\ndoggie> Listo.\nY otra cosa.\n· the turn failed: the LLM stopped\n" +
		"· ───\n"
	if got := ui.printed(); got != want {
		t.Errorf("printed:\n%s\nwant:\n%s", got, want)
	}

	// A turn after opening arrives live, once.
	streamReply(context.Background(), client, "terminal", "", "en vivo")
	ui.waitPrinted(t, "· ───\ndoggie> en vivo\n")
	ui.lines <- "/help"
	ui.waitPrinted(t, chatHelp)
	if n := strings.Count(ui.printed(), "en vivo"); n != 1 {
		t.Errorf("the live reply shows %d times:\n%s", n, ui.printed())
	}
}

func TestChatHistoryIsOptional(t *testing.T) {
	for name, tt := range map[string]struct {
		interactive bool
		history     int
		logged      bool
	}{
		"turned off":     {true, 0, true},
		"piped":          {false, 5, true},
		"nothing so far": {true, 5, false},
	} {
		t.Run(name, func(t *testing.T) {
			client := sayEnv(t, nil)
			if tt.logged {
				logTurn(t, client, events.BrainTurnData{ThreadID: "terminal", Replies: []string{"de antes"}})
			}
			ui := newFakeUI(tt.interactive)
			runChat(t, client, ui, make(chan connState), historyOptions(tt.history))
			ui.lines <- "/help"
			ui.waitPrinted(t, chatHelp)
			if got := ui.printed(); got != chatHelp+"\n" {
				t.Errorf("printed %q", got)
			}
		})
	}
}

func TestAgo(t *testing.T) {
	for d, want := range map[time.Duration]string{-time.Second: "0s", 90 * time.Second: "1m", 47 * time.Hour: "47h", 72 * time.Hour: "3d"} {
		if got := ago(d); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
}

// The history doesn't make a catch-up after a reconnection read the log from
// the oldest turn shown.
func TestChatHistoryLeavesCatchUpWhereItWas(t *testing.T) {
	client := sayEnv(t, nil)
	logTurn(t, client, events.BrainTurnData{ThreadID: "terminal", Replies: []string{"de antes"}})
	ui := newFakeUI(true)
	c := &chatState{ui: ui, view: newReplyView(ui), o: historyOptions(5), started: time.Now()}
	c.showHistory(context.Background(), client)
	if !strings.Contains(ui.printed(), "doggie> de antes") {
		t.Fatalf("printed %q", ui.printed())
	}
	if !c.lastSeen.IsZero() {
		t.Errorf("lastSeen = %v, want it untouched", c.lastSeen)
	}
}

// serveHistory answers brain.history as the brain would.
func serveHistory(t *testing.T, c *bus.Conn, reply func(events.HistoryRequest) events.HistoryReply) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	in, err := bus.Serve(ctx, c, events.BrainHistory)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for req := range in {
			req.Reply(reply(req.Req))
		}
	}()
}

// The chat takes the history from the brain when it can, leaving out what
// was logged after it opened.
func TestChatTakesTheHistoryFromTheBrain(t *testing.T) {
	client := sayEnv(t, nil)
	logTurn(t, client, events.BrainTurnData{ThreadID: "terminal", Replies: []string{"solo en el log"}})
	asked := make(chan events.HistoryRequest, 1)
	serveHistory(t, client, func(r events.HistoryRequest) events.HistoryReply {
		asked <- r
		turn := func(seq uint64, reply string) events.PastTurn {
			return events.PastTurn{ID: reply, At: time.Now(), Seq: seq, Turn: events.BrainTurnData{ThreadID: "terminal", Replies: []string{reply}}}
		}
		return events.HistoryReply{Known: true, Turns: []events.PastTurn{turn(1, "del cerebro"), turn(1<<40, "después de abrir")}}
	})
	ui := newFakeUI(true)
	runChat(t, client, ui, make(chan connState), historyOptions(3))
	ui.waitPrinted(t, "· ───\n")
	out := ui.printed()
	if !strings.Contains(out, "doggie> del cerebro\n") || strings.Contains(out, "solo en el log") || strings.Contains(out, "después de abrir") {
		t.Errorf("printed:\n%s", out)
	}
	if r := <-asked; r.ThreadID != "terminal" || r.Limit != 3 {
		t.Errorf("asked %+v", r)
	}
}

// When the brain doesn't know the conversation, the chat reads the log,
// looking further back while it finds too few turns.
func TestChatReadsTheHistoryFromTheLog(t *testing.T) {
	saved := historyWindows
	historyWindows = []time.Duration{time.Nanosecond, 0}
	t.Cleanup(func() { historyWindows = saved })
	client := sayEnv(t, nil)
	for _, s := range []string{"uno", "dos", "tres"} {
		logTurn(t, client, events.BrainTurnData{ThreadID: "terminal", Replies: []string{s}})
	}
	serveHistory(t, client, func(events.HistoryRequest) events.HistoryReply { return events.HistoryReply{} })
	ui := newFakeUI(true)
	runChat(t, client, ui, make(chan connState), historyOptions(2))
	ui.waitPrinted(t, "· ───\n")
	if out := ui.printed(); !strings.Contains(out, "doggie> dos\ndoggie> tres\n") || strings.Contains(out, "uno") {
		t.Errorf("printed:\n%s", out)
	}
}

// A failure DOGGIE told itself needs no note; the others are told in a few
// words, or with the error when it says something only it can.
func TestFailureNotes(t *testing.T) {
	for _, tc := range []struct {
		turn events.BrainTurnData
		want string
	}{
		{events.BrainTurnData{}, ""},
		{events.BrainTurnData{Failure: events.FailureOther}, ""},
		{events.BrainTurnData{Error: "llm: usage limit reached: You've hit your session limit", Failure: events.FailureUsageLimit}, ""},
		{events.BrainTurnData{Error: "llm: turn timed out", Failure: events.FailureTimeout}, "it took too long"},
		{events.BrainTurnData{Error: "llm: tools", Failure: events.FailureNoTools}, "DOGGIE's tools didn't answer"},
		{events.BrainTurnData{Error: "claudecode: Please run /login", Failure: events.FailureOther}, "claudecode: Please run /login"},
		{events.BrainTurnData{Error: "logged before failures had kinds"}, "logged before failures had kinds"},
	} {
		if got := failure(tc.turn); got != tc.want {
			t.Errorf("failure(%+v) = %q, want %q", tc.turn, got, tc.want)
		}
	}
}
