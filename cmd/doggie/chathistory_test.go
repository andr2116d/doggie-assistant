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
