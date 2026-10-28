// Package console is how DOGGIE talks with a person at a terminal: Screen,
// a chat that fills the terminal, and, when there is no terminal, a Console
// that reads and prints plain lines.
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

// Console reads lines from its input and prints plain text, for when there
// is no terminal, as in a pipe. Its methods may be called from any
// goroutine.
type Console struct {
	lines  chan string
	writes chan []byte
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
}

// Lines delivers each line read. It is closed at the end of the input, or
// on Close.
func (c *Console) Lines() <-chan string { return c.lines }

// IsInteractive reports false: a Console doesn't edit lines.
func (c *Console) IsInteractive() bool { return false }

// Write prints p. Control characters are dropped first, so the text may
// come from anyone.
func (c *Console) Write(p []byte) (int, error) {
	select {
	case c.writes <- bytes.Clone(p):
		return len(p), nil
	case <-c.done:
		return 0, ErrClosed
	}
}

// SetStatus does nothing: plain lines have no status.
func (c *Console) SetStatus(string) {}

// AskHidden returns a closed channel: plain lines can't hide one.
func (c *Console) AskHidden(string) <-chan string {
	answer := make(chan string)
	close(answer)
	return answer
}

// Close ends the console. Everything written before is printed when it
// returns.
func (c *Console) Close() error {
	c.once.Do(func() { close(c.stop) })
	<-c.done
	return nil
}

// hiddenAsk asks for a line typed unseen.
type hiddenAsk struct {
	prompt string
	answer chan string
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
// outlive its reader, blocked in Read: a terminal read can't be cancelled.
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

// Plain starts a console that reads lines from in and prints to out. A
// line too long to read ends the input, with an error on out.
func Plain(in io.Reader, out io.Writer) *Console {
	c := &Console{
		lines:  make(chan string),
		writes: make(chan []byte),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go c.run(in, out)
	return c
}

type scanned struct {
	line string
	err  error
}

func (c *Console) run(in io.Reader, out io.Writer) {
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
			c.drain(print)
			q.close()
			return
		}
	}
}

// drain prints the writes already on their way when Close was called.
func (c *Console) drain(print func([]byte)) {
	for {
		select {
		case p := <-c.writes:
			print(p)
		default:
			return
		}
	}
}

// IsTerminal reports whether in and out are both terminals, where a Screen
// can run.
func IsTerminal(in, out *os.File) bool {
	return term.IsTerminal(int(in.Fd())) && term.IsTerminal(int(out.Fd()))
}
