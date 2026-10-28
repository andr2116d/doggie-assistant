package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/console"
)

const sayUsage = `Usage:
  doggie say [-channel terminal] [-thread id] [-wait 20s] [-as satellite] <text...>

Says text to DOGGIE as if it came from a channel, prints the replies, then
keeps listening for -wait after the last one, for notices about tasks the
reply started. Ctrl+C stops listening.

The shared token may only speak for "terminal"; for another channel, connect
as a satellite that owns it (see "doggie satellite help").

It also takes -config, -server, -ca and -as (see "doggie events -h").
`

// sayOptions are the flags of doggie say.
type sayOptions struct {
	channel, thread string
	// wait is how long to keep listening after the last activity.
	wait time.Duration
	// replyTimeout bounds the wait for the brain to answer.
	replyTimeout time.Duration
}

func sayCmd(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("say", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprint(flags.Output(), sayUsage) }
	client := addClientFlags(flags)
	var o sayOptions
	flags.StringVar(&o.channel, "channel", "terminal", "channel the text comes from")
	flags.StringVar(&o.thread, "thread", "", "conversation thread (default: the channel's)")
	flags.DurationVar(&o.wait, "wait", 20*time.Second, "keep listening this long after the last reply")
	if err := flags.Parse(args); err != nil {
		return err
	}
	text := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if text == "" {
		fmt.Fprint(os.Stderr, sayUsage)
		return errors.New("nothing to say")
	}
	if o.thread == "" {
		o.thread = o.channel
	}
	o.replyTimeout = 3 * time.Minute

	conn, _, err := client.dial("say")
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return say(ctx, conn, stdout, text, o)
}

// say publishes text as an utterance and prints what the brain says in the
// thread until the turn has ended and wait has passed with nothing new.
func say(ctx context.Context, conn *bus.Conn, w io.Writer, text string, o sayOptions) error {
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
	// A channel this connection may not speak for is refused by the bus,
	// which then never confirms the publish: bound the wait.
	pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
	meta, err := bus.Publish(pctx, conn, subject, events.UtteranceData{ThreadID: o.thread, Channel: o.channel, Text: text})
	pcancel()
	if err != nil {
		return fmt.Errorf("could not say it on %s (is this connection allowed to speak for that channel? see \"doggie satellite help\"): %w", o.channel, err)
	}

	view := newReplyView(w)
	answered := false
	idle := time.NewTimer(o.replyTimeout)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-idle.C:
			if !answered {
				return errors.New("no reply: is DOGGIE running with the brain component?")
			}
			return nil
		case d, ok := <-deltas:
			if !ok {
				return ctx.Err()
			}
			if d.ThreadID == o.thread {
				view.delta(blockKey{d.CausedBy, d.Block}, d.Text)
			}
		case ev, ok := <-speaks:
			if !ok {
				return ctx.Err()
			}
			if ev.Data.ThreadID == o.thread {
				view.block(blockKey{ev.CausedBy, ev.Data.Block}, ev.Data.Text)
			}
		case ev, ok := <-turns:
			if !ok {
				return ctx.Err()
			}
			if ev.Data.ThreadID != o.thread {
				continue
			}
			if why := failure(ev.Data); why != "" {
				fmt.Fprintf(w, "doggie: the turn failed: %s\n", why)
			}
			if slices.ContainsFunc(ev.Data.Inputs, func(in events.TurnInput) bool { return in.EventID == meta.ID }) {
				answered = true
			}
			if answered {
				// Keep listening for notices, from now on.
				idle.Reset(o.wait)
			}
		}
	}
}

// blockKey identifies a text block: the event its turn answers and its
// number in the turn.
type blockKey struct {
	cause string
	block int
}

// replyView prints replies as they are written. Pieces (out.speak.delta) may
// be lost or arrive before the end of the previous block (out.speak, which
// travels through the event log), so each out.speak checks what is on screen
// and prints the block again if it differs.
type replyView struct {
	w io.Writer
	// p, if set, prints replies with looks, under the assistant's name, in
	// place of plain "doggie>" lines on w.
	p    printer
	name string
	// screen, if set, shows each block as a bubble of its own instead.
	screen  replyScreen
	printed map[blockKey]string // what is on screen, by block
	pending map[blockKey]string // leading whitespace, not shown yet
	open    *blockKey           // the block on the current line, if any
	// done are blocks already shown whole: a piece of them arriving late
	// is dropped.
	done recentIDs
}

// maxOpenBlocks bounds the blocks whose end hasn't come, e.g. when the brain
// stopped in the middle of one.
const maxOpenBlocks = 64

func (k blockKey) id() string { return k.cause + "#" + strconv.Itoa(k.block) }

func newReplyView(w io.Writer) *replyView {
	return &replyView{w: w, printed: map[blockKey]string{}, pending: map[blockKey]string{}}
}

// printer prints text with looks, as an interactive console does.
type printer interface {
	Print(texts ...console.Text) error
}

// newStyledView is a replyView that prints on p with looks, replies under
// name.
func newStyledView(p printer, name string) *replyView {
	v := newReplyView(nil)
	v.p, v.name = p, name
	return v
}

// startBlock prints the start of a reply's block, under the name.
func (v *replyView) startBlock(text string) {
	if v.p == nil {
		fmt.Fprintf(v.w, "doggie> %s", text)
		return
	}
	v.p.Print(console.Text{Text: v.name, Role: console.Assistant, Gap: true}, console.Text{Text: "  "}, v.body(text, true))
}

// more prints more of the block being written.
func (v *replyView) more(text string) {
	if text == "" {
		return
	}
	if v.p == nil {
		fmt.Fprint(v.w, text)
		return
	}
	v.p.Print(v.body(text, true))
}

// wholeBlock prints a block all at once, on its own line.
func (v *replyView) wholeBlock(text string) {
	if v.p == nil {
		fmt.Fprintf(v.w, "doggie> %s\n", text)
		return
	}
	v.p.Print(console.Text{Text: v.name, Role: console.Assistant, Gap: true}, console.Text{Text: "  "}, v.body(text+"\n", false))
}

// body is a reply's text, wrapped under the end of the name. Paced, so
// what the brain writes in bursts flows; the end of a line is paced too,
// or it would show the rest at once.
func (v *replyView) body(text string, paced bool) console.Text {
	return console.Text{Text: text, Wrap: true, Indent: console.Width(v.name) + 2, Paced: paced}
}

// line prints a line of its own, e.g. a note.
func (v *replyView) line(text string, role console.Role) {
	if v.p == nil {
		fmt.Fprintln(v.w, text)
		return
	}
	v.p.Print(console.Text{Text: text + "\n", Role: role, Wrap: true})
}

func (v *replyView) delta(k blockKey, text string) {
	if v.done.has(k.id()) {
		return
	}
	if v.screen != nil {
		v.screenDelta(k, text)
		return
	}
	if v.open != nil && *v.open != k {
		v.close()
	}
	if len(v.printed)+len(v.pending) >= maxOpenBlocks {
		v.forgetOpenBlocks()
	}
	if _, ok := v.printed[k]; ok {
		if v.open != nil && *v.open == k {
			v.more(text)
			v.printed[k] += text
		}
		// Else its line was closed by another block: its out.speak will
		// show it whole.
		return
	}
	s := v.pending[k] + text
	if strings.TrimSpace(s) == "" {
		v.pending[k] = s // start the line once there is something to see
		return
	}
	delete(v.pending, k)
	s = strings.TrimLeft(s, " \t\r\n")
	v.startBlock(s)
	v.printed[k] = s
	v.open = &k
}

func (v *replyView) block(k blockKey, text string) {
	v.done.add(k.id())
	if v.screen != nil {
		v.screenBlock(k, text)
		return
	}
	p, shown := v.printed[k]
	delete(v.printed, k)
	delete(v.pending, k)
	if v.open != nil && *v.open == k {
		// The block came before its last pieces: finish the line with
		// them, unless what is shown isn't how the block starts.
		if rest, ok := strings.CutPrefix(strings.TrimLeft(text, " \t\r\n"), p); ok {
			v.more(strings.TrimRight(rest, " \t\r\n"))
			v.close()
			return
		}
		v.close()
	}
	if shown && strings.TrimSpace(p) == strings.TrimSpace(text) {
		return
	}
	if v.open != nil {
		v.close()
	}
	v.wholeBlock(strings.TrimSpace(text))
}

// forgetOpenBlocks drops the blocks still waiting for their end, but the
// one on the current line.
func (v *replyView) forgetOpenBlocks() {
	for k := range v.printed {
		if v.open == nil || k != *v.open {
			delete(v.printed, k)
		}
	}
	clear(v.pending)
}

func (v *replyView) close() {
	if v.p == nil {
		fmt.Fprintln(v.w)
	} else {
		v.p.Print(v.body("\n", true))
	}
	v.open = nil
}

// note prints a line of its own, e.g. a status, after any open block.
func (v *replyView) note(text string) {
	if v.screen != nil {
		v.screen.Notice("", console.NoticeInfo, strings.TrimPrefix(text, "· "))
		return
	}
	if v.open != nil {
		v.close()
	}
	v.line(text, noteRole(text))
}

// recentIDs is a set of event IDs that forgets the oldest past maxRecent.
type recentIDs struct {
	set   map[string]bool
	order []string
}

// maxRecent is enough to recognize events seen twice, live and from the
// log after a reconnection.
const maxRecent = 4096

// add reports whether id is new, and keeps it.
func (r *recentIDs) add(id string) bool {
	if r.set[id] {
		return false
	}
	if r.set == nil {
		r.set = map[string]bool{}
	}
	r.set[id] = true
	r.order = append(r.order, id)
	if len(r.order) > maxRecent {
		delete(r.set, r.order[0])
		r.order = r.order[1:]
	}
	return true
}

func (r *recentIDs) has(id string) bool { return r.set[id] }

// noteRole is how a note looks: the chat's own notes, which start with "· ",
// are muted; what was asked for, like /tasks, is not.
func noteRole(text string) console.Role {
	if strings.HasPrefix(text, "· ") {
		return console.Muted
	}
	return console.Normal
}
