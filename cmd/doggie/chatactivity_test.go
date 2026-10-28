package main

import (
	"context"
	"testing"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

// The chat shows DOGGIE thinking from a sentence until its reply starts,
// again while it works between blocks, and not once the turn ends.
func TestChatShowsThinking(t *testing.T) {
	brains := make(chan *bus.Conn, 1)
	causes := make(chan string, 1)
	client := sayEnv(t, func(ctx context.Context, c *bus.Conn, u bus.Event[events.UtteranceData]) {
		brains <- c
		causes <- u.ID
	})
	ui := newFakeScreen()
	runChat(t, client, ui, make(chan connState), testChatOptions())
	ui.lines <- "busca algo"
	brain, cause := <-brains, <-causes
	thinking := "activity " + words("").thinking
	ui.waitCalls(t, thinking)

	bus.Emit(brain, events.OutSpeakDelta, events.SpeakDeltaData{ThreadID: "terminal", Channel: "terminal", Text: "Busco ", CausedBy: cause})
	ui.waitCalls(t, thinking, "activity ")
	bus.Publish(context.Background(), brain, events.OutSpeak, events.SpeakData{ThreadID: "terminal", Channel: "terminal", Text: "Busco ya."}, bus.CausedBy(cause))
	ui.waitCalls(t, thinking, "activity ", thinking)
	bus.Publish(context.Background(), brain, events.BrainTurn, events.BrainTurnData{ThreadID: "terminal", Inputs: []events.TurnInput{{EventID: cause}}, Replies: []string{"Busco ya."}}, bus.CausedBy(cause))
	ui.waitCalls(t, thinking, "activity ", thinking, "activity ")
	ui.waitCalls(t, "reply "+cause+"#0 Busco ya.")
}
