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
	"sync"

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
}

// Terminal is a terminal already in raw mode.
type Terminal interface {
	io.ReadWriter
	// Width is the terminal's width in columns.
	Width() (int, error)
	// Resized delivers a value when the terminal changes size; nil if it
	// can't tell.
	Resized() <-chan struct{}
	// Restore takes the terminal out of raw mode.
	Restore() error
}

// Console reads lines from the user and prints text above them. Its methods
// may be called from any goroutine.
type Console struct {
	lines    chan string
	writes   chan []byte
	statuses chan string
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once

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
	select {
	case c.writes <- bytes.Clone(p):
		return len(p), nil
	case <-c.done:
		return 0, ErrClosed
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
		writes:      make(chan []byte),
		statuses:    make(chan string),
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
	s := &screen{prompt: []rune(oneLine(opts.Prompt))}
	e := newEditor(opts.MaxRunes)
	q := lineQueue{lines: c.lines}
	var dec decoder
	var b bytes.Buffer
	// update erases the live region, runs f, draws the region again unless
	// the user quit, and writes it all at once.
	update := func(f func()) {
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
		if !q.ending {
			s.draw(&b, e)
		}
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
			c.keys(dec.feed(p), e, s, &q, &b, update)
		case p := <-c.writes:
			update(func() { s.print(&b, string(p)) })
		case text := <-c.statuses:
			update(func() { s.setStatus(text) })
		case <-t.Resized():
			update(nil)
		case q.send() <- q.next():
			q.sent()
		case <-c.stop:
			c.drainWrites(func(p []byte) { s.print(&b, string(p)) })
			s.finish(&b)
			b.WriteString("\x1b[?2004l")
			t.Write(b.Bytes())
			t.Restore()
			q.close()
			return
		}
	}
}

// keys applies keys to the line being typed.
func (c *Console) keys(keys []key, e *editor, s *screen, q *lineQueue, b *bytes.Buffer, update func(func())) {
	for _, k := range keys {
		if q.ending {
			return
		}
		act, line := e.apply(k)
		switch act {
		case actSubmit:
			q.pending = append(q.pending, line)
			// Above a reply being written, so it doesn't split it.
			update(func() { s.printAbove(b, string(s.prompt)+line+"\n") })
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
func (c *Console) drainWrites(print func([]byte)) {
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
	print := func(p []byte) { io.WriteString(out, clean(string(p))) }
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
