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
	"github.com/andr2116d/doggie-assistant/internal/config"
	"github.com/andr2116d/doggie-assistant/internal/console"
)

const chatUsage = `Usage:
  doggie chat [-channel terminal] [-thread id] [-history 5] [-as satellite] [-wait 20s]

Talks with DOGGIE: type a sentence and press Enter, and keep typing while
DOGGIE answers or works. Replies show as they are written, and notices
about tasks arrive when they happen. Ctrl+C or Ctrl+D on an empty line
quits.

Commands start with "/" and are run by the chat, not by DOGGIE's brain:
/tasks, /answer <id> [text], /cancel <id> and /help.

If DOGGIE isn't running on this machine, the chat starts it in this
process, logging to $XDG_DATA_HOME/doggie/doggie.log, and stops it on quit.
"doggie run" in a terminal does the same the other way around: it runs
DOGGIE and opens this chat.

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
	// sendTimeout bounds publishing one sentence. While the connection is
	// down, the publish waits for it, so what is said meanwhile goes out
	// once it is back.
	sendTimeout time.Duration
	// history is how many past turns an interactive chat shows on opening.
	history int
	// host, if set, lets the chat run DOGGIE in its own process when none
	// runs on this machine.
	host *hosting
	// words are the chat's own words, in the user's language.
	words chatWords
	// publish sends a sentence; nil means bus.Publish.
	publish func(context.Context, *bus.Conn, bus.Subject[events.UtteranceData], events.UtteranceData) (bus.Meta, error)
}

func defaultChatOptions() chatOptions {
	return chatOptions{channel: "terminal", thread: "terminal", wait: 20 * time.Second,
		replyTimeout: 3 * time.Minute, slowReply: 20 * time.Second, sendTimeout: 10 * time.Minute, history: defaultHistory}
}

func chatCmd(args []string) error {
	flags := flag.NewFlagSet("chat", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprint(flags.Output(), chatUsage) }
	client := addClientFlags(flags)
	o := defaultChatOptions()
	flags.StringVar(&o.channel, "channel", "terminal", "channel to speak on")
	flags.StringVar(&o.thread, "thread", "", "conversation thread (default: the channel's)")
	flags.DurationVar(&o.wait, "wait", 20*time.Second, "without a terminal, keep listening this long after the last reply")
	flags.IntVar(&o.history, "history", defaultHistory, "turns of the conversation so far to show on opening")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("unexpected arguments %q (see \"doggie chat -h\")", flags.Args())
	}
	if o.thread == "" {
		o.thread = o.channel
	}

	cfg, err := config.Load(*client.config)
	if err != nil {
		return err
	}
	ui, err := openChatUI(cfg)
	if err != nil {
		return err
	}
	o.words = words(cfg.Language)
	// The full screen leaves nothing behind but this, after it closes.
	leaving := "DOGGIE keeps running; doggie opens the chat again"
	defer func() {
		ui.Close()
		if ui.IsInteractive() {
			fmt.Println("· " + o.words.tr.f(leaving))
		}
	}()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	states := make(chan connState, 64)
	// A DOGGIE on this machine can be run here; one elsewhere can't.
	if *client.server == "" {
		o.host = newHosting(*client.config, factories())
		defer func() {
			if o.host.own != nil {
				leaving = "DOGGIE stopped"
				if ui.IsInteractive() {
					fmt.Fprintln(ui, "· "+o.words.tr.f("stopping DOGGIE..."))
				}
			}
			o.host.close()
		}()
	}
	conn, url, err := dialChat(client, ui, states)
	// DOGGIE isn't running on this machine, or never ran: start it here.
	if (errors.Is(err, bus.ErrNoServer) || errors.Is(err, errNoToken)) && o.host != nil {
		conn, url, err = startHere(ctx, ui, client, states, o.host, o.words.tr)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	if ui.IsInteractive() {
		fmt.Fprintln(ui, "· "+o.words.tr.f("talking with DOGGIE at %s on %s; /help lists the commands, Ctrl+D quits", url, o.channel))
	}
	return chat(ctx, conn, ui, states, o)
}

// chatName is this chat's connection name, the source of what it says. It
// tells its own sentences apart from those said in its thread elsewhere.
var chatName = fmt.Sprintf("chat-%d", os.Getpid())

// assistantName is the name replies show under.
const assistantName = "DOGGIE"

// activityUI shows that DOGGIE is working on something, as an interactive
// console does.
type activityUI interface {
	SetActivity(text string)
}

// chatView prints replies on ui: as bubbles on a full screen, plain lines
// otherwise.
func chatView(ui chatUI) *replyView {
	if s, ok := ui.(replyScreen); ok {
		return newScreenView(s)
	}
	return newReplyView(ui)
}

// dialChat connects the chat to the bus, reporting what happens to the
// connection on states.
func dialChat(client clientFlags, ui chatUI, states chan<- connState) (*bus.Conn, string, error) {
	return client.dial(chatName, func(d *bus.DialOptions) {
		d.OnDisconnect = func(err error) { sendState(states, connState{kind: connDown, err: err}) }
		d.OnReconnect = func() { sendState(states, connState{kind: connUp}) }
		d.OnClosed = func(err error) { sendState(states, connState{kind: connClosed, err: err}) }
		if ui.IsInteractive() {
			// Its warnings would break the screen; the chat tells what
			// happens to the connection itself.
			d.Log = slog.New(slog.DiscardHandler)
		}
	})
}

// startHere starts DOGGIE in this process and connects the chat to it. If
// another process started DOGGIE at the same time, it connects to that one.
func startHere(ctx context.Context, ui chatUI, client clientFlags, states chan<- connState, host *hosting, tr translator) (*bus.Conn, string, error) {
	if startErr := host.start(ctx); startErr != nil {
		// Lost a race with another start: use that DOGGIE once it listens.
		deadline := time.Now().Add(5 * time.Second)
		for {
			conn, url, err := dialChat(client, ui, states)
			if err == nil || time.Now().After(deadline) {
				if err != nil {
					err = startErr
				}
				return conn, url, err
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	conn, url, err := dialChat(client, ui, states)
	if err != nil {
		stopCore(host.release(), events.StopHostClosed)
		return nil, "", err
	}
	// Piped, stdout carries only the replies.
	var w io.Writer = ui
	if !ui.IsInteractive() {
		w = os.Stderr
	}
	fmt.Fprintln(w, "· "+tr.f("DOGGIE wasn't running: started it here, logging to %s; it stops when you quit", host.logPath))
	return conn, url, nil
}

// stopHere stops the DOGGIE doggie run started, on purpose.
func stopHere(ui chatUI, own *core, tr translator) {
	if ui.IsInteractive() {
		fmt.Fprintln(ui, "· "+tr.f("stopping DOGGIE..."))
	}
	stopCore(own, events.StopOnPurpose)
}

// runWithChat runs DOGGIE with its chat in this terminal: logs go to a file,
// and quitting the chat stops DOGGIE.
func runWithChat(ctx context.Context, cfg config.Config, selected []factory, client clientFlags) error {
	ui, err := openChatUI(cfg)
	if err != nil {
		return err
	}
	defer func() {
		ui.Close()
		fmt.Println("· " + words(cfg.Language).tr.f("DOGGIE stopped"))
	}()
	logFile, logPath, err := openLog(cfg)
	if err != nil {
		return err
	}
	defer logFile.Close()
	log := slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: cfg.LogLevel()}))
	if cfg.UsingDefaults {
		log.Warn("no config file, using defaults", "expected", cfg.File)
	}
	own, err := startCore(ctx, cfg, log, selected, roleRun)
	if err != nil {
		return err
	}
	tr := words(cfg.Language).tr
	defer stopHere(ui, own, tr)

	states := make(chan connState, 64)
	var conn *bus.Conn
	if cfg.Bus.Listen == "" {
		// Only in-process connections: the chat is part of this process.
		conn, err = own.srv.Connect(chatName)
	} else {
		conn, _, err = dialChat(client, ui, states)
	}
	if err != nil {
		return err
	}
	defer conn.Close()
	if cfg.UsingDefaults {
		fmt.Fprintln(ui, "· "+tr.f("no config file at %s, using defaults", cfg.File))
	}
	fmt.Fprintln(ui, "· "+tr.f("DOGGIE is running, logging to %s; /help lists the commands, Ctrl+D stops it", logPath))
	o := defaultChatOptions()
	o.words = words(cfg.Language)
	return chatSession(ctx, conn, ui, states, own, o)
}

// chatSession runs the chat. own, if set, is the DOGGIE this process runs:
// the chat ends if it stops.
func chatSession(ctx context.Context, conn *bus.Conn, ui chatUI, states chan connState, own *core, o chatOptions) error {
	if own != nil {
		sctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			select {
			case <-own.Done():
				err := own.err
				if err == nil {
					err = errors.New("it stopped")
				}
				sendState(states, connState{kind: connClosed, err: err})
			case <-sctx.Done():
			}
		}()
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
	// SetStatus shows the status line; empty removes it.
	SetStatus(text string)
	// AskHidden has the user type a line unseen.
	AskHidden(prompt string) <-chan string
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
	// openSeq is the newest event in the log when the chat opened, 0 if
	// unknown: what came after arrives live.
	openSeq uint64
	down    bool
	// self is the chat's connection name, the source of its sentences.
	self string

	host *hosting
	// stopReason is why DOGGIE said it stops (events.Stop*), until it is
	// back; downAt is when the connection was lost.
	stopReason string
	downAt     time.Time
	// takeover fires when the chat may run DOGGIE itself.
	takeover <-chan time.Time
	// handedOver fires once a DOGGIE run here is handed over.
	handedOver chan struct{}
	// activity is what the chat shows DOGGIE doing, if anything.
	activity string

	board *taskBoard
	// lists and lookups deliver what the board asked for.
	lists   chan boardList
	lookups chan boardLookup
	// statusShown is the status line on screen since statusAt; statusDue
	// fires when a newer one may be shown.
	statusShown string
	statusAt    time.Time
	statusDue   <-chan time.Time

	// hidden delivers the answer to hiddenFor's question, typed unseen.
	hidden    <-chan string
	hiddenFor string
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
	said, err := bus.SubscribeEnvelopes(sctx, conn, events.UserUtterance.Type()+".*")
	if err != nil {
		return err
	}
	stops, err := bus.Subscribe(sctx, conn, events.SystemStopping)
	if err != nil {
		return err
	}
	taskEvents, err := bus.SubscribeEnvelopes(sctx, conn, "task.*")
	if err != nil {
		return err
	}
	subject, err := events.UtteranceOn(o.channel)
	if err != nil {
		return err
	}

	c := &chatState{
		ui: ui, view: chatView(ui), o: o, self: conn.Name(), host: o.host,
		handedOver: make(chan struct{}, 1),
		unanswered: map[string]time.Time{},
		started:    time.Now(),
		board:      newTaskBoard(o.thread),
		lists:      make(chan boardList, 4),
		lookups:    make(chan boardLookup, maxBoardTasks),
	}
	defer ui.SetStatus("")
	defer c.showActivity("")
	c.online(true)
	listActive(sctx, conn, c.lists)
	toSend := make(chan string, 64)
	results := make(chan sent, 64)
	go sender(sctx, conn, subject, o, toSend, results)

	lines := ui.Lines()
	inputDone := false
	// inFlight are sentences handed to the sender and not confirmed yet, and
	// running the commands not done yet; failed tells a piped chat to end
	// with an error.
	inFlight, running, failed := 0, 0, false
	commands := make(chan *command, 64)
	commandsDone := make(chan commandResult, 64)
	c.board.tr = c.wordsOr().tr
	go commander(sctx, conn, o.thread, o.channel, c.wordsOr().tr, commands, commandsDone)
	if ui.IsInteractive() && o.history > 0 {
		qctx, cancel := context.WithTimeout(sctx, 3*time.Second)
		c.openSeq, _ = bus.LastSeq(qctx, conn)
		cancel()
		c.showHistory(sctx, conn)
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	// idleSince is the last activity, for a piped chat to know when to stop.
	idleSince := time.Now()
	for {
		c.showActivity(c.activityNow(inFlight))
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
			if !c.accept(line) {
				continue
			}
			sentence, cmd := parseLine(line)
			switch {
			case cmd != nil && cmd.name == "/answer" && cmd.id != "" && cmd.text == "" && ui.IsInteractive():
				c.hidden, c.hiddenFor = ui.AskHidden(c.wordsOr().tr.f("answer for %s (hidden): ", short(cmd.id))), cmd.id
			case cmd == nil:
				if c.down {
					c.say(console.NoticeWait, "no connection: it goes out when DOGGIE is back")
				}
				inFlight++
				toSend <- sentence
			case cmd.check(nil) != "":
				c.view.notice(console.NoticeBad, cmd.check(c.wordsOr().tr))
				failed = true
			case cmd.name == "/help":
				c.view.note(c.wordsOr().tr.f(chatHelp))
			default:
				running++
				commands <- cmd
			}
		case text, ok := <-c.hidden:
			id := c.hiddenFor
			c.hidden, c.hiddenFor = nil, ""
			switch {
			case !ok:
			case strings.TrimSpace(text) == "":
				c.say(console.NoticeInfo, "not answered")
			case c.accept(text):
				running++
				commands <- &command{name: "/answer", id: id, text: strings.TrimSpace(text)}
			}
		case r := <-commandsDone:
			running--
			failed = failed || !r.ok
			kind := console.NoticeBad
			switch {
			case r.did:
				kind = console.NoticeGood
			case r.ok:
				kind = console.NoticeInfo
			}
			c.view.notice(kind, r.out)
			if r.answered != nil {
				c.board.answered(*r.answered)
				c.showStatus()
			}
			idleSince = time.Now()
		case r := <-results:
			inFlight--
			if r.err != nil {
				failed = true
				c.say(console.NoticeBad, "could not send %q: %v", short(r.text), r.err)
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
		case env, ok := <-said:
			if !ok {
				return ended(ctx)
			}
			if c.saidElsewhere(env) {
				idleSince = time.Now()
			}
		case s := <-states:
			if err := c.connection(ctx, conn, s); err != nil {
				return err
			}
		case env, ok := <-taskEvents:
			if !ok {
				return ended(ctx)
			}
			if c.taskEvent(sctx, conn, env) {
				idleSince = time.Now()
			}
		case l := <-c.lists:
			for _, q := range c.board.merge(l) {
				c.view.notice(console.NoticeWait, q)
				idleSince = time.Now()
			}
			c.showStatus()
		case l := <-c.lookups:
			questions, again := c.board.looked(l)
			for _, q := range questions {
				c.view.notice(console.NoticeWait, q)
				idleSince = time.Now()
			}
			if again != "" {
				c.lookup(sctx, conn, again, recheckDelay)
			}
			c.showStatus()
		case <-c.statusDue:
			c.statusDue = nil
			c.showStatus()
		case ev, ok := <-stops:
			if !ok {
				return ended(ctx)
			}
			c.stopping(ev.Data.Reason)
		case <-c.takeover:
			c.takeover = nil
			c.takeOver(ctx)
		case <-c.handoverRequests():
			c.handOver()
		case <-c.handedOver:
			c.say(console.NoticeInfo, "handed DOGGIE over")
		case <-c.host.done():
			own := c.host.release()
			c.say(console.NoticeBad, "the DOGGIE run here stopped: %v", own.err)
			if own.logFile != nil {
				own.logFile.Close()
			}
			if c.down {
				c.takeover = time.After(takeoverDelay + jitter())
			}
		case now := <-tick.C:
			c.checkSlow(now)
			if inputDone && inFlight == 0 && running == 0 {
				if len(c.unanswered) == 0 && now.Sub(idleSince) >= o.wait {
					if failed {
						return fmt.Errorf("%w: some sentences or commands failed", errShown)
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
		c.say(console.NoticeBad, "not sent: the text is not valid UTF-8")
		return false
	case utf8.RuneCountInString(line) > events.MaxUtteranceRunes:
		c.say(console.NoticeBad, "not sent: %d characters, the limit is %d", utf8.RuneCountInString(line), events.MaxUtteranceRunes)
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
		switch {
		case err == nil || ctx.Err() != nil:
		case errors.Is(err, bus.ErrDisconnected):
			err = fmt.Errorf("the connection to DOGGIE didn't come back within %s", o.sendTimeout)
		default:
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

// stopping takes DOGGIE's word on why it stops.
func (c *chatState) stopping(reason string) {
	if reason == c.stopReason {
		return
	}
	c.stopReason = reason
	switch reason {
	case events.StopOnPurpose:
		c.say(console.NoticeWait, "DOGGIE was stopped; it's back when someone starts it (doggie run or doggie chat)")
	case events.StopHandover:
		c.say(console.NoticeInfo, "DOGGIE is moving to another process...")
	case events.StopHostClosed:
		c.say(console.NoticeWait, "the chat that ran DOGGIE closed")
	}
}

// takeOver runs DOGGIE in this chat's process, unless it was stopped on
// purpose, it is moving and may still arrive, or it is back.
func (c *chatState) takeOver(ctx context.Context) {
	if !c.down || c.host == nil || c.host.own != nil {
		return
	}
	// The file is sure to be there; the event may not have arrived.
	if reason := c.host.lastStop(); reason != "" {
		c.stopping(reason)
	}
	switch c.stopReason {
	case events.StopOnPurpose:
		return
	case events.StopHandover:
		if wait := handoverTimeout - time.Since(c.downAt); wait > 0 {
			c.takeover = time.After(wait)
			return
		}
	}
	err := c.host.start(ctx)
	switch {
	case err == nil:
		c.say(console.NoticeInfo, "DOGGIE was gone: started it here, logging to %s; it stops when you quit", c.host.logPath)
	case errors.Is(err, errRunning):
		// Another chat or doggie run took it: the connection comes back.
	default:
		c.say(console.NoticeBad, "could not start DOGGIE here: %v", err)
		c.takeover = time.After(10 * time.Second)
	}
}

// handoverRequests delivers an explicit DOGGIE's requests for the one run
// here; nil when none runs here.
func (c *chatState) handoverRequests() <-chan struct{} {
	if c.host == nil || c.host.own == nil {
		return nil
	}
	return c.host.handover
}

// handOver stops the DOGGIE run here for an explicit one, without holding
// the chat meanwhile. The chat stays, and reconnects to the new DOGGIE.
func (c *chatState) handOver() {
	own := c.host.release()
	c.say(console.NoticeInfo, "doggie run is taking DOGGIE over; handing it over...")
	go func() {
		stopCore(own, events.StopHandover)
		c.handedOver <- struct{}{}
	}()
}

// saidElsewhere shows a sentence said in this thread by someone else, e.g.
// another chat, so the reply to it makes sense. It reports whether it did.
func (c *chatState) saidElsewhere(env bus.Envelope) bool {
	ev, channel, err := bus.DecodeUnder(env, events.UserUtterance)
	if err != nil || env.Source == c.self || !c.fresh(env.Meta) {
		return false
	}
	thread := ev.Data.ThreadID
	if thread == "" {
		thread = channel
	}
	if thread != c.o.thread {
		return false
	}
	who := channel
	if channel == c.o.channel {
		who = "you (elsewhere)"
	}
	if s := c.view.screen; s != nil {
		if who == "you (elsewhere)" {
			who = c.wordsOr().elsewhere
		}
		s.SaidElsewhere(who, ev.Data.Text)
		return true
	}
	c.view.note(fmt.Sprintf("%s> %s", who, oneLineText(ev.Data.Text)))
	return true
}

// oneLineText keeps a sentence on one line of the chat.
func oneLineText(s string) string {
	return strings.Join(strings.Fields(s), " ")
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
	if why := failure(c.wordsOr().tr, ev.Data); why != "" {
		c.say(console.NoticeBad, "the turn failed: %s", why)
	}
	return true
}

// connection tells the user about the bus connection, and on its return
// shows what was said meanwhile. A connection closed for good ends the chat.
func (c *chatState) connection(ctx context.Context, conn *bus.Conn, s connState) error {
	switch s.kind {
	case connDown:
		if !c.down {
			c.down, c.downAt = true, time.Now()
			c.board.clear()
			c.showStatus()
			if c.host != nil && c.stopReason == "" {
				// On this machine, the reason is sure to be in the file.
				c.stopping(c.host.lastStop())
			}
			if c.stopReason == "" {
				c.say(console.NoticeWait, "lost the connection to DOGGIE; reconnecting...")
			}
			c.online(false)
			if c.host != nil && c.host.own == nil {
				c.takeover = time.After(takeoverDelay + jitter())
			}
		}
	case connUp:
		c.down, c.stopReason, c.takeover = false, "", nil
		c.say(console.NoticeGood, "connected again")
		c.online(true)
		c.catchUp(ctx, conn)
		listActive(ctx, conn, c.lists)
	case connClosed:
		reason := "closed"
		if s.err != nil {
			reason = s.err.Error()
		}
		c.say(console.NoticeBad, "DOGGIE ended the connection: %s", reason)
		return fmt.Errorf("%w: connection ended: %s", errShown, reason)
	}
	return nil
}

// catchUpMargin widens the catch-up window, since a satellite's clock may
// lag the core's; events already seen are skipped anyway.
const catchUpMargin = 2 * time.Minute

// catchUp shows this thread's sentences, replies and turns logged while
// the chat was disconnected, oldest first. Events seen live are skipped.
func (c *chatState) catchUp(ctx context.Context, conn *bus.Conn) {
	since := c.lastSeen
	if since.IsZero() {
		since = c.started
	}
	// Event times come from the clocks of whoever published them.
	since = since.Add(-catchUpMargin)
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var missed []bus.Envelope
	for _, typ := range []string{events.OutSpeak.Type(), events.BrainTurn.Type(), events.UserUtterance.Type() + ".*"} {
		envs, err := bus.History(hctx, conn, since, typ)
		if err != nil {
			c.say(console.NoticeBad, "could not read what was said meanwhile: %v", err)
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
		default:
			c.saidElsewhere(env)
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
	c.say(console.NoticeWait, "no reply yet: is DOGGIE's brain running?")
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

// activityNow is what DOGGIE is doing for this chat, if anything, while
// something said here waits for its answer and no reply is being written,
// as when it uses a tool between two blocks: the task it started for what
// was said, or else thinking.
func (c *chatState) activityNow(inFlight int) string {
	if (inFlight > 0 || len(c.unanswered) > 0) && c.view.open == nil && !c.down {
		if what := c.board.doingFor(c.unanswered); what != "" {
			return what
		}
		return c.wordsOr().thinking
	}
	return ""
}

// showActivity shows text as DOGGIE's activity, if it changed and the UI
// can show one.
func (c *chatState) showActivity(text string) {
	a, ok := c.ui.(activityUI)
	if !ok || text == c.activity {
		return
	}
	c.activity = text
	a.SetActivity(text)
}
