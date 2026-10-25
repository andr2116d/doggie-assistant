package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

// fakeUI is a chat UI the test types into.
type fakeUI struct {
	lines       chan string
	interactive bool
	mu          sync.Mutex
	out         strings.Builder
}

func newFakeUI(interactive bool) *fakeUI {
	return &fakeUI{lines: make(chan string, 100), interactive: interactive}
}

func (u *fakeUI) Write(p []byte) (int, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.out.Write(p)
}
func (u *fakeUI) Lines() <-chan string { return u.lines }
func (u *fakeUI) IsInteractive() bool  { return u.interactive }

func (u *fakeUI) printed() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.out.String()
}

// waitPrinted waits until the UI shows want.
func (u *fakeUI) waitPrinted(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(u.printed(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("printed %q, want it to contain %q", u.printed(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func testChatOptions() chatOptions {
	return chatOptions{channel: "terminal", thread: "terminal", wait: 300 * time.Millisecond,
		replyTimeout: 3 * time.Second, slowReply: time.Hour, sendTimeout: 2 * time.Second}
}

// runChat runs chat in the background and returns its result channel.
func runChat(t *testing.T, client *bus.Conn, ui chatUI, states chan connState, o chatOptions) (<-chan error, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- chat(ctx, client, ui, states, o) }()
	return done, cancel
}

func result(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("chat did not end")
		return nil
	}
}

// streamReply says texts as the brain does: pieces, then the whole block,
// then the turn.
func streamReply(ctx context.Context, c *bus.Conn, thread, cause string, texts ...string) {
	for i, s := range texts {
		for _, piece := range strings.SplitAfter(s, " ") {
			bus.Emit(c, events.OutSpeakDelta, events.SpeakDeltaData{ThreadID: thread, Channel: "terminal", Text: piece, Block: i, CausedBy: cause})
		}
		bus.Publish(ctx, c, events.OutSpeak, events.SpeakData{ThreadID: thread, Channel: "terminal", Text: s, Block: i}, bus.CausedBy(cause))
	}
	var inputs []events.TurnInput
	if cause != "" {
		inputs = []events.TurnInput{{EventID: cause}}
	}
	bus.Publish(ctx, c, events.BrainTurn, events.BrainTurnData{ThreadID: thread, Inputs: inputs, Replies: texts}, bus.CausedBy(cause))
}

func TestChatTalks(t *testing.T) {
	var got []events.UtteranceData
	var mu sync.Mutex
	client := sayEnv(t, func(ctx context.Context, c *bus.Conn, u bus.Event[events.UtteranceData]) {
		mu.Lock()
		got = append(got, u.Data)
		mu.Unlock()
		streamReply(ctx, c, "other", "", "not for you")
		streamReply(ctx, c, u.Data.ThreadID, u.ID, "Voy.", "Un chiste corto.")
	})
	ui := newFakeUI(true)
	done, _ := runChat(t, client, ui, make(chan connState), testChatOptions())
	ui.lines <- "  recuérdame comprar pan  "
	ui.waitPrinted(t, "doggie> Voy.\ndoggie> Un chiste corto.\n")
	ui.lines <- "   "
	ui.lines <- "¿y la hora?"
	ui.waitPrinted(t, "corto.\ndoggie> Voy.\n")
	close(ui.lines)
	if err := result(t, done); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0].Text != "recuérdame comprar pan" || got[0].ThreadID != "terminal" || got[0].Channel != "terminal" || got[1].Text != "¿y la hora?" {
		t.Errorf("sent %+v", got)
	}
	if strings.Contains(ui.printed(), "not for you") {
		t.Error("printed another thread's reply")
	}
}

// Notices arrive whenever they happen, between the user's sentences.
func TestChatShowsLaterNotices(t *testing.T) {
	brains := make(chan *bus.Conn, 1)
	client := sayEnv(t, func(ctx context.Context, c *bus.Conn, u bus.Event[events.UtteranceData]) {
		streamReply(ctx, c, "terminal", u.ID, "En eso.")
		brains <- c
	})
	ui := newFakeUI(true)
	runChat(t, client, ui, make(chan connState), testChatOptions())
	ui.lines <- "pon un temporizador"
	ui.waitPrinted(t, "doggie> En eso.\n")
	streamReply(context.Background(), <-brains, "terminal", "", "Listo, te lo recuerdo.")
	ui.waitPrinted(t, "doggie> Listo, te lo recuerdo.\n")
}

func TestChatTellsAFailedTurn(t *testing.T) {
	client := sayEnv(t, func(ctx context.Context, c *bus.Conn, u bus.Event[events.UtteranceData]) {
		bus.Publish(ctx, c, events.BrainTurn, events.BrainTurnData{ThreadID: "terminal", Inputs: []events.TurnInput{{EventID: u.ID}}, Error: "the CLI is not logged in"})
	})
	ui := newFakeUI(true)
	runChat(t, client, ui, make(chan connState), testChatOptions())
	ui.lines <- "hola"
	ui.waitPrinted(t, "· the turn failed: the CLI is not logged in\n")
}

func TestChatTellsASlowReplyOnce(t *testing.T) {
	client := sayEnv(t, nil)
	ui := newFakeUI(true)
	o := testChatOptions()
	o.slowReply = 100 * time.Millisecond
	runChat(t, client, ui, make(chan connState), o)
	ui.lines <- "hola"
	ui.lines <- "¿estás?"
	ui.waitPrinted(t, "· no reply yet")
	time.Sleep(2500 * time.Millisecond)
	if n := strings.Count(ui.printed(), "no reply yet"); n != 1 {
		t.Errorf("told %d times", n)
	}
}

func TestChatRefusesTooLongSentences(t *testing.T) {
	sentences := make(chan string, 10)
	client := sayEnv(t, func(ctx context.Context, c *bus.Conn, u bus.Event[events.UtteranceData]) {
		sentences <- u.Data.Text
	})
	ui := newFakeUI(false)
	done, _ := runChat(t, client, ui, make(chan connState), testChatOptions())
	ui.lines <- strings.Repeat("ñ", events.MaxUtteranceRunes+1)
	ui.lines <- "bad \xff utf8"
	ui.lines <- strings.Repeat("ñ", events.MaxUtteranceRunes)
	close(ui.lines)
	result(t, done) // no brain answers: it ends on the reply timeout
	ui.waitPrinted(t, fmt.Sprintf("· not sent: %d characters, the limit is %d", events.MaxUtteranceRunes+1, events.MaxUtteranceRunes))
	ui.waitPrinted(t, "· not sent: the text is not valid UTF-8")
	if s := <-sentences; len([]rune(s)) != events.MaxUtteranceRunes {
		t.Errorf("sent a %d-rune sentence", len([]rune(s)))
	}
	select {
	case s := <-sentences:
		t.Errorf("also sent %q", s[:min(len(s), 20)])
	default:
	}
}

// Piped in, the chat sends every line, waits for the replies and the
// notices after them, then ends.
func TestChatPiped(t *testing.T) {
	client := sayEnv(t, func(ctx context.Context, c *bus.Conn, u bus.Event[events.UtteranceData]) {
		streamReply(ctx, c, "terminal", u.ID, "re: "+u.Data.Text)
		if u.Data.Text == "two" {
			time.Sleep(100 * time.Millisecond)
			streamReply(ctx, c, "terminal", "", "notice")
		}
	})
	ui := newFakeUI(false)
	for _, l := range []string{"one", "two"} {
		ui.lines <- l
	}
	close(ui.lines)
	start := time.Now()
	done, _ := runChat(t, client, ui, make(chan connState), testChatOptions())
	if err := result(t, done); err != nil {
		t.Fatal(err)
	}
	if want := "doggie> re: one\ndoggie> re: two\ndoggie> notice\n"; ui.printed() != want {
		t.Errorf("printed %q, want %q", ui.printed(), want)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("took %v", took)
	}
}

func TestChatPipedWithoutABrain(t *testing.T) {
	client := sayEnv(t, nil)
	ui := newFakeUI(false)
	ui.lines <- "hola"
	close(ui.lines)
	o := testChatOptions()
	o.replyTimeout = time.Second
	done, _ := runChat(t, client, ui, make(chan connState), o)
	if err := result(t, done); err == nil || !strings.Contains(err.Error(), "no reply") {
		t.Errorf("err = %v", err)
	}
}

func TestChatPipedNothing(t *testing.T) {
	ui := newFakeUI(false)
	close(ui.lines)
	done, _ := runChat(t, sayEnv(t, nil), ui, make(chan connState), testChatOptions())
	if err := result(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestChatStopsOnCancel(t *testing.T) {
	ui := newFakeUI(true)
	done, cancel := runChat(t, sayEnv(t, nil), ui, make(chan connState), testChatOptions())
	cancel()
	if err := result(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestChatTellsAboutTheConnection(t *testing.T) {
	ui := newFakeUI(true)
	states := make(chan connState, 10)
	done, _ := runChat(t, sayEnv(t, nil), ui, states, testChatOptions())
	states <- connState{kind: connDown, err: errors.New("EOF")}
	states <- connState{kind: connDown, err: errors.New("EOF")}
	states <- connState{kind: connUp}
	ui.waitPrinted(t, "· lost the connection to DOGGIE; reconnecting...\n· connected again\n")
	states <- connState{kind: connClosed, err: errors.New("nats: authorization violation")}
	if err := result(t, done); err == nil || !strings.Contains(err.Error(), "authorization violation") {
		t.Errorf("err = %v", err)
	}
	ui.waitPrinted(t, "· DOGGIE ended the connection: nats: authorization violation\n")
	if n := strings.Count(ui.printed(), "lost the connection"); n != 1 {
		t.Errorf("told the loss %d times", n)
	}
}

// After a reconnection, what was said meanwhile shows once, in order, and
// what was seen live doesn't show again.
func TestChatCatchesUp(t *testing.T) {
	client := sayEnv(t, nil)
	ui := newFakeUI(true)
	c := &chatState{ui: ui, view: newReplyView(ui), o: testChatOptions(), unanswered: map[string]time.Time{}, started: time.Now().Add(-time.Second)}
	ctx := context.Background()
	live, _ := bus.Publish(ctx, client, events.OutSpeak, events.SpeakData{ThreadID: "terminal", Text: "seen live"}, bus.CausedBy("a"))
	ev := bus.Event[events.SpeakData]{Meta: live, Data: events.SpeakData{ThreadID: "terminal", Text: "seen live"}}
	c.speak(ev)
	c.unanswered["q1"] = time.Now()
	bus.Publish(ctx, client, events.OutSpeak, events.SpeakData{ThreadID: "terminal", Text: "missed one"}, bus.CausedBy("b"))
	bus.Publish(ctx, client, events.OutSpeak, events.SpeakData{ThreadID: "elsewhere", Text: "not ours"}, bus.CausedBy("c"))
	bus.Publish(ctx, client, events.BrainTurn, events.BrainTurnData{ThreadID: "terminal", Inputs: []events.TurnInput{{EventID: "q1"}}, Error: "boom"})
	bus.Publish(ctx, client, events.OutSpeak, events.SpeakData{ThreadID: "terminal", Text: "missed two"}, bus.CausedBy("d"))
	c.catchUp(ctx, client)
	c.catchUp(ctx, client)
	want := "doggie> seen live\ndoggie> missed one\n· the turn failed: boom\ndoggie> missed two\n"
	if got := ui.printed(); got != want {
		t.Errorf("printed %q, want %q", got, want)
	}
	if len(c.unanswered) != 0 {
		t.Error("a turn read from the log didn't answer its sentence")
	}
}

// A turn may arrive before the chat learns its sentence was published: the
// sentence is answered all the same.
func TestChatTurnBeforeItsPublishConfirmed(t *testing.T) {
	ui := newFakeUI(true)
	c := &chatState{ui: ui, view: newReplyView(ui), o: testChatOptions(), unanswered: map[string]time.Time{}}
	c.turn(bus.Event[events.BrainTurnData]{Meta: bus.Meta{ID: "turn1"}, Data: events.BrainTurnData{ThreadID: "terminal", Inputs: []events.TurnInput{{EventID: "q1"}}}})
	if !c.early.has("q1") {
		t.Fatal("early answer not kept")
	}
}

// A piped chat waits for a sentence still being sent, however long the
// confirmation takes, and fails when one could not be sent.
func TestChatPipedWaitsForSlowSends(t *testing.T) {
	client := sayEnv(t, func(ctx context.Context, c *bus.Conn, u bus.Event[events.UtteranceData]) {
		streamReply(ctx, c, "terminal", u.ID, "re: "+u.Data.Text)
	})
	ui := newFakeUI(false)
	ui.lines <- "lost"
	ui.lines <- "slow" // last, so nothing else holds the chat meanwhile
	close(ui.lines)
	o := testChatOptions()
	o.wait = 0
	o.publish = func(ctx context.Context, conn *bus.Conn, s bus.Subject[events.UtteranceData], d events.UtteranceData) (bus.Meta, error) {
		if d.Text == "lost" {
			return bus.Meta{}, errors.New("refused")
		}
		time.Sleep(2500 * time.Millisecond) // longer than wait and a tick
		return bus.Publish(ctx, conn, s, d)
	}
	o.sendTimeout = 10 * time.Second
	done, _ := runChat(t, client, ui, make(chan connState), o)
	err := result(t, done)
	if !strings.Contains(ui.printed(), "doggie> re: slow") {
		t.Errorf("ended before the reply: %q", ui.printed())
	}
	if !errors.Is(err, errShown) || !strings.Contains(ui.printed(), `could not send "lost"`) {
		t.Errorf("err = %v, printed %q", err, ui.printed())
	}
}

// Sentences said in the thread by others show, so their replies make sense;
// the chat's own don't show twice, and other threads' don't show.
func TestChatShowsSentencesSaidElsewhere(t *testing.T) {
	brains := make(chan *bus.Conn, 1)
	client := sayEnv(t, func(ctx context.Context, c *bus.Conn, u bus.Event[events.UtteranceData]) {
		brains <- c
	})
	ui := newFakeUI(true)
	runChat(t, client, ui, make(chan connState), testChatOptions())
	ui.lines <- "mine"
	brain := <-brains
	ctx := context.Background()
	other, _ := events.UtteranceOn("voice:kitchen")
	bus.Publish(ctx, brain, events.UserUtterance.With("terminal"), events.UtteranceData{ThreadID: "terminal", Channel: "terminal", Text: "from  another\nchat"})
	bus.Publish(ctx, brain, other, events.UtteranceData{ThreadID: "terminal", Channel: "voice:kitchen", Text: "from the kitchen"})
	bus.Publish(ctx, brain, other, events.UtteranceData{Channel: "voice:kitchen", Text: "kitchen's own thread"})
	ui.waitPrinted(t, "you (elsewhere)> from another chat\nvoice:kitchen> from the kitchen\n")
	time.Sleep(200 * time.Millisecond)
	if p := ui.printed(); strings.Contains(p, "mine") || strings.Contains(p, "own thread") {
		t.Errorf("printed %q", p)
	}
}

func TestChatCatchesUpSentencesSaidElsewhere(t *testing.T) {
	client := sayEnv(t, nil)
	ui := newFakeUI(true)
	c := &chatState{ui: ui, view: newReplyView(ui), o: testChatOptions(), self: client.Name(),
		unanswered: map[string]time.Time{}, started: time.Now().Add(-time.Second)}
	ctx := context.Background()
	bus.Publish(ctx, client, events.UserUtterance.With("terminal"), events.UtteranceData{ThreadID: "terminal", Text: "my own"})
	bus.Publish(ctx, client, events.OutSpeak, events.SpeakData{ThreadID: "terminal", Text: "reply"})
	c.catchUp(ctx, client)
	if got := ui.printed(); got != "doggie> reply\n" {
		t.Errorf("printed %q", got)
	}
	// As seen by another chat: the sentence is someone else's.
	c.self = "someone-else"
	c.seen = recentIDs{}
	ui.out.Reset()
	c.catchUp(ctx, client)
	if got := ui.printed(); got != "you (elsewhere)> my own\ndoggie> reply\n" {
		t.Errorf("printed %q", got)
	}
}
