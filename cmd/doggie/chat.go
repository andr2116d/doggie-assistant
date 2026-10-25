package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/console"
)

const chatUsage = `Usage:
  doggie chat [-channel terminal] [-thread id] [-as satellite] [-wait 20s]

Talks with DOGGIE: type a sentence and press Enter, and keep typing while
DOGGIE answers or works. Replies show as they are written, and notices
about tasks arrive when they happen. Ctrl+C or Ctrl+D on an empty line
quits.

Without a terminal (e.g. sentences piped in), it sends each line, then
keeps listening for -wait after the last reply.

It also takes -config, -server, -ca and -as (see "doggie events -h").
`

// chatOptions are the flags of doggie chat.
type chatOptions struct {
	channel, thread string
	// wait is how long a piped chat keeps listening after the last reply.
	wait time.Duration
	// replyTimeout bounds how long a piped chat waits for its answers.
	replyTimeout time.Duration
	// slowReply is when to tell the user a sentence got no answer yet.
	slowReply time.Duration
	// sendTimeout bounds publishing one sentence.
	sendTimeout time.Duration
	// publish sends a sentence; nil means bus.Publish.
	publish func(context.Context, *bus.Conn, bus.Subject[events.UtteranceData], events.UtteranceData) (bus.Meta, error)
}

func chatCmd(args []string) error {
	flags := flag.NewFlagSet("chat", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprint(flags.Output(), chatUsage) }
	client := addClientFlags(flags)
	o := chatOptions{replyTimeout: 3 * time.Minute, slowReply: 20 * time.Second, sendTimeout: 5 * time.Second}
	flags.StringVar(&o.channel, "channel", "terminal", "channel to speak on")
	flags.StringVar(&o.thread, "thread", "", "conversation thread (default: the channel's)")
	flags.DurationVar(&o.wait, "wait", 20*time.Second, "without a terminal, keep listening this long after the last reply")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q (see \"doggie chat -h\")", flags.Args())
	}
	if o.thread == "" {
		o.thread = o.channel
	}

	ui, err := console.Open(os.Stdin, os.Stdout, console.Options{Prompt: "you> ", MaxRunes: events.MaxUtteranceRunes})
	if err != nil {
		return err
	}
	defer ui.Close()
	states := make(chan connState, 64)
	conn, url, err := client.dial("chat", func(d *bus.DialOptions) {
		d.OnDisconnect = func(err error) { sendState(states, connState{kind: connDown, err: err}) }
		d.OnReconnect = func() { sendState(states, connState{kind: connUp}) }
		d.OnClosed = func(err error) { sendState(states, connState{kind: connClosed, err: err}) }
		if ui.IsInteractive() {
			// Its warnings would break the screen; the chat tells what
			// happens to the connection itself.
			d.Log = slog.New(slog.DiscardHandler)
		}
	})
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if ui.IsInteractive() {
		fmt.Fprintf(ui, "· talking with DOGGIE at %s on %s; Ctrl+D quits\n", url, o.channel)
	}
	return chat(ctx, conn, ui, states, o)
}

// errShown marks an error the chat already showed, so it isn't printed
// again on exit.
var errShown = errors.New("shown")

// connKind is a change in the connection to the bus.
type connKind int

const (
	connDown connKind = iota
	connUp
	connClosed
)

type connState struct {
	kind connKind
	err  error
}

// sendState passes a connection change on without blocking the bus's
// callbacks; a full queue means the chat is gone or far behind.
func sendState(states chan<- connState, s connState) {
	select {
	case states <- s:
	default:
	}
}

// chatUI is where the chat reads sentences and prints.
type chatUI interface {
	io.Writer
	Lines() <-chan string
	IsInteractive() bool
}

// sent is the outcome of publishing a sentence.
type sent struct {
	text string
	id   string
	at   time.Time
	err  error
}

// chatState is what the chat loop owns.
type chatState struct {
	ui   chatUI
	view *replyView
	o    chatOptions
	// unanswered are the sentences sent and not answered by a turn yet,
	// with when they were sent.
	unanswered map[string]time.Time
	// early are sentences a turn answered before their publish was
	// confirmed here.
	early  recentIDs
	warned bool // the user was told a reply is slow
	seen   recentIDs
	// lastSeen is the log time of the newest event shown; started bounds
	// what a catch-up may show.
	lastSeen, started time.Time
	down              bool
}

// chat runs until the user quits, the input ends and replies are in, or
// ctx is done.
func chat(ctx context.Context, conn *bus.Conn, ui chatUI, states <-chan connState, o chatOptions) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Subscribe first, so no reply is missed.
	deltas, err := bus.SubscribeSignal(sctx, conn, events.OutSpeakDelta)
	if err != nil {
		return err
	}
	speaks, err := bus.Subscribe(sctx, conn, events.OutSpeak)
	if err != nil {
		return err
	}
	turns, err := bus.Subscribe(sctx, conn, events.BrainTurn)
	if err != nil {
		return err
	}
	subject, err := events.UtteranceOn(o.channel)
	if err != nil {
		return err
	}

	c := &chatState{
		ui: ui, view: newReplyView(ui), o: o,
		unanswered: map[string]time.Time{},
		started:    time.Now(),
	}
	toSend := make(chan string, 64)
	results := make(chan sent, 64)
	go sender(sctx, conn, subject, o, toSend, results)

	lines := ui.Lines()
	inputDone := false
	// inFlight are sentences handed to the sender and not confirmed yet;
	// failed tells a piped chat to end with an error.
	inFlight, failed := 0, false
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	// idleSince is the last activity, for a piped chat to know when to stop.
	idleSince := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case line, ok := <-lines:
			if !ok {
				if ui.IsInteractive() {
					return nil
				}
				lines, inputDone = nil, true
				close(toSend)
				continue
			}
			if c.accept(line) {
				inFlight++
				toSend <- line
			}
		case r := <-results:
			inFlight--
			if r.err != nil {
				failed = true
				c.view.note(fmt.Sprintf("· could not send %q: %v", short(r.text), r.err))
				continue
			}
			if c.early.has(r.id) {
				continue
			}
			c.unanswered[r.id] = r.at
		case d, ok := <-deltas:
			if !ok {
				return ended(ctx)
			}
			if d.ThreadID == o.thread {
				c.view.delta(blockKey{d.CausedBy, d.Block}, d.Text)
				idleSince = time.Now()
			}
		case ev, ok := <-speaks:
			if !ok {
				return ended(ctx)
			}
			if c.speak(ev) {
				idleSince = time.Now()
			}
		case ev, ok := <-turns:
			if !ok {
				return ended(ctx)
			}
			if c.turn(ev) {
				idleSince = time.Now()
			}
		case s := <-states:
			if err := c.connection(ctx, conn, s); err != nil {
				return err
			}
		case now := <-tick.C:
			c.checkSlow(now)
			if inputDone && inFlight == 0 {
				if len(c.unanswered) == 0 && now.Sub(idleSince) >= o.wait {
					if failed {
						return fmt.Errorf("%w: some sentences were not sent", errShown)
					}
					return nil
				}
				if len(c.unanswered) > 0 && now.Sub(c.oldest()) >= o.replyTimeout {
					return errors.New("no reply: is DOGGIE running with the brain component?")
				}
			}
		}
	}
}

// accept checks a typed line before sending it.
func (c *chatState) accept(line string) bool {
	line = strings.TrimSpace(line)
	switch {
	case line == "":
		return false
	case !utf8.ValidString(line):
		c.view.note("· not sent: the text is not valid UTF-8")
		return false
	case utf8.RuneCountInString(line) > events.MaxUtteranceRunes:
		c.view.note(fmt.Sprintf("· not sent: %d characters, the limit is %d", utf8.RuneCountInString(line), events.MaxUtteranceRunes))
		return false
	}
	return true
}

// sender publishes sentences in order. A slow or failed publish doesn't
// hold the chat loop, which keeps showing replies meanwhile.
func sender(ctx context.Context, conn *bus.Conn, subject bus.Subject[events.UtteranceData], o chatOptions, lines <-chan string, results chan<- sent) {
	publish := o.publish
	if publish == nil {
		publish = func(ctx context.Context, c *bus.Conn, s bus.Subject[events.UtteranceData], d events.UtteranceData) (bus.Meta, error) {
			return bus.Publish(ctx, c, s, d)
		}
	}
	for line := range lines {
		text := strings.TrimSpace(line)
		pctx, cancel := context.WithTimeout(ctx, o.sendTimeout)
		meta, err := publish(pctx, conn, subject, events.UtteranceData{ThreadID: o.thread, Channel: o.channel, Text: text})
		cancel()
		if err != nil && ctx.Err() == nil {
			err = fmt.Errorf("DOGGIE did not confirm it (is it running, and may this connection speak for %s?)", o.channel)
		}
		select {
		case results <- sent{text: text, id: meta.ID, at: time.Now(), err: err}:
		case <-ctx.Done():
			return
		}
	}
}

// fresh reports whether an event is new to the chat, and remembers it.
func (c *chatState) fresh(env bus.Meta) bool {
	if !c.seen.add(env.ID) {
		return false
	}
	if env.Time.After(c.lastSeen) {
		c.lastSeen = env.Time
	}
	return true
}

// speak shows a whole block of this thread. It reports whether it did.
func (c *chatState) speak(ev bus.Event[events.SpeakData]) bool {
	if ev.Data.ThreadID != c.o.thread || !c.fresh(ev.Meta) {
		return false
	}
	c.view.block(blockKey{ev.CausedBy, ev.Data.Block}, ev.Data.Text)
	return true
}

// turn takes the end of a turn of this thread: its sentences are answered,
// and a failure is told.
func (c *chatState) turn(ev bus.Event[events.BrainTurnData]) bool {
	if ev.Data.ThreadID != c.o.thread || !c.fresh(ev.Meta) {
		return false
	}
	for _, in := range ev.Data.Inputs {
		if _, ok := c.unanswered[in.EventID]; ok {
			delete(c.unanswered, in.EventID)
		} else {
			c.early.add(in.EventID)
		}
	}
	c.warned = false
	if ev.Data.Error != "" {
		c.view.note("· the turn failed: " + ev.Data.Error)
	}
	return true
}

// connection tells the user about the bus connection, and on its return
// shows what was said meanwhile. A connection closed for good ends the chat.
func (c *chatState) connection(ctx context.Context, conn *bus.Conn, s connState) error {
	switch s.kind {
	case connDown:
		if !c.down {
			c.down = true
			c.view.note("· lost the connection to DOGGIE; reconnecting...")
		}
	case connUp:
		c.down = false
		c.view.note("· connected again")
		c.catchUp(ctx, conn)
	case connClosed:
		reason := "closed"
		if s.err != nil {
			reason = s.err.Error()
		}
		c.view.note("· DOGGIE ended the connection: " + reason)
		return fmt.Errorf("%w: connection ended: %s", errShown, reason)
	}
	return nil
}

// catchUp shows this thread's replies and turns logged while the chat was
// disconnected, oldest first. Events seen live are skipped.
func (c *chatState) catchUp(ctx context.Context, conn *bus.Conn) {
	since := c.lastSeen
	if since.IsZero() {
		since = c.started
	}
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var missed []bus.Envelope
	for _, typ := range []string{events.OutSpeak.Type(), events.BrainTurn.Type()} {
		envs, err := bus.History(hctx, conn, since, typ)
		if err != nil {
			c.view.note("· could not read what was said meanwhile: " + err.Error())
			return
		}
		missed = append(missed, envs...)
	}
	slices.SortStableFunc(missed, func(a, b bus.Envelope) int { return a.Time.Compare(b.Time) })
	for _, env := range missed {
		if env.Time.Before(c.started) {
			continue
		}
		switch env.Type {
		case events.OutSpeak.Type():
			if ev, err := bus.Decode(env, events.OutSpeak); err == nil {
				c.speak(ev)
			}
		case events.BrainTurn.Type():
			if ev, err := bus.Decode(env, events.BrainTurn); err == nil {
				c.turn(ev)
			}
		}
	}
}

// checkSlow tells the user once when a sentence waits too long for an
// answer.
func (c *chatState) checkSlow(now time.Time) {
	if c.warned || c.down || len(c.unanswered) == 0 || now.Sub(c.oldest()) < c.o.slowReply {
		return
	}
	c.warned = true
	c.view.note("· no reply yet: is DOGGIE's brain running?")
}

func (c *chatState) oldest() time.Time {
	var t time.Time
	for _, at := range c.unanswered {
		if t.IsZero() || at.Before(t) {
			t = at
		}
	}
	return t
}

// short cuts a sentence for a status line.
func short(s string) string {
	if r := []rune(s); len(r) > 40 {
		return string(r[:40]) + "…"
	}
	return s
}

// ended is why the chat stops when a subscription closes: the user stopped
// it, or the connection is gone.
func ended(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	return errors.New("the connection to DOGGIE closed")
}
