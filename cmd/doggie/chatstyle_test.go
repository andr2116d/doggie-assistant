package main

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/console"
)

// styledUI is a fake console with looks: it keeps every text printed and
// every activity shown.
type styledUI struct {
	*fakeUI
	smu        sync.Mutex
	texts      []console.Text
	activities []string
}

func newStyledUI() *styledUI { return &styledUI{fakeUI: newFakeUI(true)} }

func (u *styledUI) Print(texts ...console.Text) error {
	u.smu.Lock()
	u.texts = append(u.texts, texts...)
	u.smu.Unlock()
	for _, t := range texts {
		u.fakeUI.Write([]byte(t.Text))
	}
	return nil
}

func (u *styledUI) SetActivity(text string) {
	u.smu.Lock()
	defer u.smu.Unlock()
	u.activities = append(u.activities, text)
}

func (u *styledUI) shown() ([]console.Text, []string) {
	u.smu.Lock()
	defer u.smu.Unlock()
	return slices.Clone(u.texts), slices.Clone(u.activities)
}

func TestStyledView(t *testing.T) {
	ui := newStyledUI()
	v := newStyledView(ui, "DOGGIE")
	k := blockKey{"u", 0}
	v.delta(k, "Ya ")
	v.delta(k, "voy.")
	v.block(k, "Ya voy.")
	v.note("· t3 finished")
	v.note("t3  running  Esperar")
	v.block(blockKey{"u", 1}, "Otro bloque.")
	texts, _ := ui.shown()
	body := func(s string, paced bool) console.Text {
		return console.Text{Text: s, Wrap: true, Indent: 8, Paced: paced}
	}
	label := console.Text{Text: "DOGGIE", Role: console.Assistant, Gap: true}
	want := []console.Text{
		label, {Text: "  "}, body("Ya ", true),
		body("voy.", true),
		body("\n", true),
		{Text: "· t3 finished\n", Role: console.Muted, Wrap: true},
		{Text: "t3  running  Esperar\n", Wrap: true},
		label, {Text: "  "}, body("Otro bloque.\n", false),
	}
	if !slices.Equal(texts, want) {
		t.Fatalf("\n%+v\nwant\n%+v", texts, want)
	}
	if got := ui.printed(); got != "DOGGIE  Ya voy.\n· t3 finished\nt3  running  Esperar\nDOGGIE  Otro bloque.\n" {
		t.Errorf("text: %q", got)
	}
}

// The chat shows DOGGIE thinking from a sentence until its reply starts,
// again while it works between blocks, and not once the turn ends.
func TestChatShowsThinking(t *testing.T) {
	brains := make(chan *bus.Conn, 1)
	causes := make(chan string, 1)
	client := sayEnv(t, func(ctx context.Context, c *bus.Conn, u bus.Event[events.UtteranceData]) {
		brains <- c
		causes <- u.ID
	})
	ui := newStyledUI()
	runChat(t, client, ui, make(chan connState), testChatOptions())
	ui.lines <- "busca algo"
	brain, cause := <-brains, <-causes
	waitActivities(t, ui, words("").thinking)

	bus.Emit(brain, events.OutSpeakDelta, events.SpeakDeltaData{ThreadID: "terminal", Channel: "terminal", Text: "Busco ", CausedBy: cause})
	waitActivities(t, ui, words("").thinking, "")
	bus.Publish(context.Background(), brain, events.OutSpeak, events.SpeakData{ThreadID: "terminal", Channel: "terminal", Text: "Busco ya."}, bus.CausedBy(cause))
	waitActivities(t, ui, words("").thinking, "", words("").thinking)
	bus.Publish(context.Background(), brain, events.BrainTurn, events.BrainTurnData{ThreadID: "terminal", Inputs: []events.TurnInput{{EventID: cause}}, Replies: []string{"Busco ya."}}, bus.CausedBy(cause))
	waitActivities(t, ui, words("").thinking, "", words("").thinking, "")
	ui.waitPrinted(t, "DOGGIE  Busco ya.\n")
}

func waitActivities(t *testing.T, ui *styledUI, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, got := ui.shown()
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("activities %q, want %q", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestNoticeIsMutedOnAConsole(t *testing.T) {
	ui := newStyledUI()
	notice(ui, "· DOGGIE is running")
	texts, _ := ui.shown()
	if len(texts) != 1 || texts[0].Role != console.Muted || texts[0].Text != "· DOGGIE is running\n" {
		t.Errorf("%+v", texts)
	}
	var plain strings.Builder
	notice(&plain, "· DOGGIE is running")
	if plain.String() != "· DOGGIE is running\n" {
		t.Errorf("plain: %q", plain.String())
	}
	piped := newStyledUI()
	piped.interactive = false
	notice(piped, "· x")
	if texts, _ := piped.shown(); len(texts) != 0 || piped.printed() != "· x\n" {
		t.Errorf("piped: %+v %q", texts, piped.printed())
	}
}
