// Package console is an interactive prompt that can print while the user
// types: output appears above the line being edited, which is redrawn below
// it, so a reply arriving mid-sentence never mixes with what is being typed.
// Without a terminal it reads and prints plain lines.
package console

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync"
	"time"

	"golang.org/x/term"
)

// ErrClosed is returned by Write after Close.
var ErrClosed = errors.New("console: closed")

// Options configures a Console.
type Options struct {
	// Prompt starts the input line, e.g. "you> ".
	Prompt string
	// MaxRunes bounds a line; typing or pasting beyond it rings the bell.
	// Zero means no bound.
	MaxRunes int
	// Forget reports the submitted lines not to keep for Up and Down, e.g.
	// ones with secrets. Nil keeps all.
	Forget func(line string) bool
	// PromptRole is the prompt's look, also when a submitted line is shown
	// above; EchoRole is the look of the line itself.
	PromptRole, EchoRole Role
	// EchoGap leaves an empty row before each submitted line shown above.
	EchoGap bool
	// Theme gives each role its look; nil is DefaultTheme.
	Theme *Theme
	// Colors is how much color to use. Open sets it from the environment.
	Colors Profile
}

// Terminal is a terminal already in raw mode.
type Terminal interface {
	io.ReadWriter
	// Width is the terminal's width in columns.
	Width() (int, error)
	// Height is the terminal's height in rows.
	Height() (int, error)
	// Resized delivers a value when the terminal changes size; nil if it
	// can't tell.
	Resized() <-chan struct{}
	// Restore takes the terminal out of raw mode.
	Restore() error
}

// Console reads lines from the user and prints text above them. Its methods
// may be called from any goroutine.
type Console struct {
	lines      chan string
	writes     chan []Text
	statuses   chan string
	activities chan string
	hides      chan hiddenAsk
	stop       chan struct{}
	done       chan struct{}
	once       sync.Once

	interactive bool
}

// Open starts a console on in and out: interactive when both are terminals,
// which it keeps in raw mode until Close, and plain otherwise.
func Open(in, out *os.File, opts Options) (*Console, error) {
	if !IsTerminal(in, out) {
		return Plain(in, out), nil
	}
	t, err := openTerminal(in, out)
	if err != nil {
		return nil, err
	}
	opts.Colors = DetectProfile(os.Getenv)
	return Interactive(t, opts), nil
}

// Lines delivers each line the user submits. It is closed when the user
// quits (Ctrl+C or Ctrl+D on an empty line), at the end of the input, or on
// Close.
func (c *Console) Lines() <-chan string { return c.lines }

// IsInteractive reports whether the console edits lines on a terminal.
func (c *Console) IsInteractive() bool { return c.interactive }

// Write prints p above the input line. Control characters are dropped
// first, so the text may come from anyone. A line is ended by a later write,
// as on any terminal.
func (c *Console) Write(p []byte) (int, error) {
	if err := c.Print(Text{Text: string(p)}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Print prints texts above the input line, each with its look, as Write
// does. It returns ErrClosed after Close. Without a terminal only the text
// is printed, at once.
func (c *Console) Print(texts ...Text) error {
	select {
	case c.writes <- slices.Clone(texts):
		return nil
	case <-c.done:
		return ErrClosed
	}
}

// SetActivity shows text with a moving indicator on a line of its own,
// above the status line, to tell the console's owner is working on
// something; empty text removes it. Its time shows once it goes long, kept
// while text changes. Without a terminal it does nothing.
func (c *Console) SetActivity(text string) {
	if !c.interactive {
		return
	}
	select {
	case c.activities <- text:
	case <-c.done:
	}
}

// SetStatus shows text on a line of its own between the output and the line
// being typed, replacing what it showed before; empty text removes it. Text
// too long for the terminal is cut. Without a terminal it does nothing.
func (c *Console) SetStatus(text string) {
	if !c.interactive {
		return
	}
	select {
	case c.statuses <- text:
	case <-c.done:
	}
}

// hiddenAsk asks for a line typed unseen.
type hiddenAsk struct {
	prompt string
	answer chan string
}

// AskHidden has the user type the next line unseen, after prompt, as a
// password, and delivers it on the returned channel, never on Lines. The
// line being typed meanwhile waits. Ctrl+C, or Ctrl+D on an empty line,
// cancel it, which delivers an empty line, as does a newer AskHidden.
// Hidden lines are never kept for Up and Down, and show as "•••" once
// submitted. The channel is closed if the console closes first, and at once
// without a terminal.
func (c *Console) AskHidden(prompt string) <-chan string {
	answer := make(chan string, 1)
	if !c.interactive {
		close(answer)
		return answer
	}
	select {
	case c.hides <- hiddenAsk{prompt, answer}:
	case <-c.done:
		close(answer)
	}
	return answer
}

// Close ends the console and restores the terminal. Everything written
// before is on screen when it returns.
func (c *Console) Close() error {
	c.once.Do(func() { close(c.stop) })
	<-c.done
	return nil
}

func newConsole(interactive bool) *Console {
	return &Console{
		lines:       make(chan string),
		writes:      make(chan []Text),
		statuses:    make(chan string),
		activities:  make(chan string),
		hides:       make(chan hiddenAsk),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		interactive: interactive,
	}
}

// lineQueue holds submitted lines until the reader of Lines takes them: the
// console must never block on that reader, who may be writing to it.
type lineQueue struct {
	lines   chan string // nil once closed
	pending []string
	ending  bool // close lines once pending is delivered
	// hidden is where the line typed unseen goes, if one is asked for; it
	// takes one line.
	hidden chan string
}

// send is the channel the next line goes out on, nil if there is none.
func (q *lineQueue) send() chan string {
	if len(q.pending) == 0 {
		return nil
	}
	return q.lines
}

func (q *lineQueue) next() string {
	if len(q.pending) == 0 {
		return ""
	}
	return q.pending[0]
}

// sent drops the line just delivered.
func (q *lineQueue) sent() { q.pending = q.pending[1:] }

// closeIfDone closes lines once the queue is ending and empty.
func (q *lineQueue) closeIfDone() {
	if q.ending && len(q.pending) == 0 && q.lines != nil {
		close(q.lines)
		q.lines = nil
	}
}

func (q *lineQueue) close() {
	if q.lines != nil {
		close(q.lines)
		q.lines = nil
	}
}

// reader reads r into a channel, closed when r fails. The goroutine may
// outlive the console, blocked in Read: a terminal read can't be cancelled.
func reader(r io.Reader, done <-chan struct{}) <-chan []byte {
	out := make(chan []byte)
	go func() {
		defer close(out)
		buf := make([]byte, 4096)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				select {
				case out <- bytes.Clone(buf[:n]):
				case <-done:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return out
}

// Interactive starts a console that edits lines on t.
func Interactive(t Terminal, opts Options) *Console {
	c := newConsole(true)
	go c.runInteractive(t, opts)
	return c
}

func (c *Console) runInteractive(t Terminal, opts Options) {
	defer close(c.done)
	input := reader(t, c.done)
	s := &screen{prompt: []rune(oneLine(opts.Prompt)), promptRole: opts.PromptRole}
	s.style(opts.Theme, opts.Colors)
	e := newEditor(opts.MaxRunes, opts.Forget)
	q := lineQueue{lines: c.lines}
	var dec decoder
	var b bytes.Buffer
	var paced pacer
	frames, anim := newClock(), newClock()
	defer frames.stop()
	defer anim.stop()
	var activitySince time.Time
	flush := func() {
		for _, held := range paced.all() {
			s.printText(&b, held)
		}
	}
	// show prints texts, holding paced ones back for the frames to come.
	// Text not paced shows what was held back first, to keep the order.
	show := func(texts []Text) {
		for _, t := range texts {
			if t.Paced {
				paced.add(t)
				continue
			}
			flush()
			s.printText(&b, t)
		}
		if paced.empty() {
			frames.stop()
		} else {
			frames.start(frameEvery)
		}
	}
	// update erases the live region, runs f, draws the region again unless
	// the user quit, and writes it all at once, as one frame: a terminal
	// that knows synchronized output shows it whole, without flicker.
	update := func(f func()) {
		b.WriteString(syncBegin)
		s.clear(&b)
		if w, err := t.Width(); err == nil && w > 0 {
			s.cols = max(w, minCols)
		} else if s.cols == 0 {
			s.cols = 80
		}
		s.refit(&b)
		if f != nil {
			f()
		}
		if len(s.activity) > 0 {
			s.elapsed = time.Since(activitySince)
		}
		if !q.ending {
			s.draw(&b, e)
		}
		b.WriteString(syncEnd)
		t.Write(b.Bytes())
		b.Reset()
	}
	b.WriteString("\x1b[?2004h") // bracketed paste
	update(nil)
	for {
		q.closeIfDone()
		select {
		case p, ok := <-input:
			if !ok {
				input = nil
				q.ending = true
				update(nil)
				continue
			}
			c.keys(dec.feed(p), e, s, &q, &b, opts, flush, update)
		case texts := <-c.writes:
			if !slices.ContainsFunc(texts, func(t Text) bool { return !t.Paced }) {
				// Nothing to show yet: the next frame shows it.
				show(texts)
				continue
			}
			update(func() { show(texts) })
		case <-frames.c:
			update(func() {
				for _, t := range paced.step() {
					s.printText(&b, t)
				}
				if paced.empty() {
					frames.stop()
				}
			})
		case text := <-c.statuses:
			update(func() { s.setStatus(text) })
		case text := <-c.activities:
			update(func() {
				was := len(s.activity) > 0
				s.setActivity(text)
				switch {
				case len(s.activity) == 0:
					anim.stop()
				case !was:
					activitySince, s.phase = time.Now(), 0
					anim.start(animEvery)
				}
			})
		case <-anim.c:
			s.phase++
			update(nil)
		case ask := <-c.hides:
			if q.hidden != nil {
				q.hidden <- "" // replaced
			}
			q.hidden = ask.answer
			update(func() { e.hide(ask.prompt) })
		case <-t.Resized():
			update(nil)
		case q.send() <- q.next():
			q.sent()
		case <-c.stop:
			// Erase the live region before printing, or the rows would go
			// over the input line.
			s.clear(&b)
			c.drainWrites(show)
			flush()
			s.finish(&b)
			b.WriteString("\x1b[?2004l")
			t.Write(b.Bytes())
			t.Restore()
			q.close()
			if q.hidden != nil {
				close(q.hidden)
			}
			return
		}
	}
}

// keys applies keys to the line being typed. flush shows the paced text
// held back at once.
func (c *Console) keys(keys []key, e *editor, s *screen, q *lineQueue, b *bytes.Buffer, opts Options, flush func(), update func(func())) {
	for _, k := range keys {
		if q.ending {
			return
		}
		hidden, hiddenPrompt := e.hidden, string(e.hiddenPrompt)
		act, line := e.apply(k)
		switch act {
		case actSubmit:
			if hidden {
				q.hidden <- line
				q.hidden = nil
			} else {
				q.pending = append(q.pending, line)
			}
			echo := []Text{
				{Text: string(s.prompt), Role: s.promptRole, Gap: opts.EchoGap},
				{Text: line + "\n", Role: opts.EchoRole},
			}
			switch {
			case hidden && line == "":
				echo = nil // cancelled
			case hidden:
				echo[0].Text, echo[1].Text = hiddenPrompt, "•••\n"
			}
			// Above a reply being written, so it doesn't split it, and after
			// what of it already came.
			update(func() {
				flush()
				s.printAbove(b, echo...)
			})
		case actQuit:
			q.ending = true
			update(nil)
		case actRefused:
			b.WriteString("\a")
			update(nil)
		default:
			update(nil)
		}
	}
}

// drainWrites prints the writes already on their way when Close was called.
func (c *Console) drainWrites(print func([]Text)) {
	for {
		select {
		case p := <-c.writes:
			print(p)
		default:
			return
		}
	}
}

// Plain starts a console that reads lines from in and prints to out, for
// when they aren't terminals. A line too long to read ends the input, with
// an error on out.
func Plain(in io.Reader, out io.Writer) *Console {
	c := newConsole(false)
	go c.runPlain(in, out)
	return c
}

type scanned struct {
	line string
	err  error
}

func (c *Console) runPlain(in io.Reader, out io.Writer) {
	defer close(c.done)
	input := make(chan scanned)
	go func() {
		defer close(input)
		sc := bufio.NewScanner(in)
		sc.Buffer(nil, maxPaste)
		for sc.Scan() {
			select {
			case input <- scanned{line: sc.Text()}:
			case <-c.done:
				return
			}
		}
		if err := sc.Err(); err != nil {
			select {
			case input <- scanned{err: err}:
			case <-c.done:
			}
		}
	}()
	print := func(texts []Text) {
		for _, t := range texts {
			io.WriteString(out, clean(t.Text))
		}
	}
	q := lineQueue{lines: c.lines}
	for {
		q.closeIfDone()
		select {
		case in, ok := <-input:
			switch {
			case !ok:
				input = nil
				q.ending = true
			case in.err != nil:
				fmt.Fprintf(out, "input: %v\n", in.err)
			default:
				q.pending = append(q.pending, in.line)
			}
		case p := <-c.writes:
			print(p)
		case q.send() <- q.next():
			q.sent()
		case <-c.stop:
			c.drainWrites(print)
			q.close()
			return
		}
	}
}

// IsTerminal reports whether in and out are both terminals, so Open would
// edit lines on them.
func IsTerminal(in, out *os.File) bool {
	return term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}
