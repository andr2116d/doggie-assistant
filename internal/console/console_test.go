package console

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTerm is a terminal the test types into, drawn on a vt.
type fakeTerm struct {
	keys    *io.PipeWriter
	in      *io.PipeReader
	resized chan struct{}

	mu       sync.Mutex
	v        *vt
	raw      bytes.Buffer
	width    int
	restored int
}

func newFakeTerm(t *testing.T, width int) *fakeTerm {
	in, keys := io.Pipe()
	return &fakeTerm{keys: keys, in: in, resized: make(chan struct{}, 1), v: newVT(t, width), width: width}
}

func (f *fakeTerm) Read(p []byte) (int, error) { return f.in.Read(p) }

func (f *fakeTerm) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raw.Write(p)
	return f.v.Write(p)
}

func (f *fakeTerm) Width() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.width, nil
}

func (f *fakeTerm) Resized() <-chan struct{} { return f.resized }

func (f *fakeTerm) Restore() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restored++
	return nil
}

func (f *fakeTerm) typeKeys(t *testing.T, s string) {
	t.Helper()
	if _, err := f.keys.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeTerm) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.v.lines()
}

// waitScreen waits until the screen shows want.
func (f *fakeTerm) waitScreen(t *testing.T, want ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Equal(f.lines(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("screen %q, want %q", f.lines(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func next(t *testing.T, lines <-chan string) (string, bool) {
	t.Helper()
	select {
	case l, ok := <-lines:
		return l, ok
	case <-time.After(5 * time.Second):
		t.Fatal("no line")
		return "", false
	}
}

func openFake(t *testing.T, width int, opts Options) (*Console, *fakeTerm) {
	t.Helper()
	f := newFakeTerm(t, width)
	c := Interactive(f, opts)
	t.Cleanup(func() { c.Close() })
	return c, f
}

func TestConsoleReadsLines(t *testing.T) {
	c, f := openFake(t, 40, Options{Prompt: "you> "})
	f.typeKeys(t, "hola qué tal\r")
	if l, ok := next(t, c.Lines()); !ok || l != "hola qué tal" {
		t.Fatalf("line %q, %v", l, ok)
	}
	f.waitScreen(t, "you> hola qué tal", "you>")
	if !c.IsInteractive() {
		t.Error("not interactive")
	}
}

func TestConsoleWritesWhileTyping(t *testing.T) {
	c, f := openFake(t, 40, Options{Prompt: "you> "})
	f.typeKeys(t, "abc")
	f.waitScreen(t, "you> abc")
	fmt.Fprint(c, "doggie> Voy")
	f.waitScreen(t, "doggie> Voy", "you> abc")
	f.typeKeys(t, "d\r")
	fmt.Fprint(c, ", listo.\n")
	if l, _ := next(t, c.Lines()); l != "abcd" {
		t.Errorf("line %q", l)
	}
	f.waitScreen(t, "you> abcd", "doggie> Voy, listo.", "you>")
}

// Whoever reads Lines may be writing to the console at the same time: the
// console must not wait for it.
func TestConsoleNeverBlocksOnItsReader(t *testing.T) {
	c, f := openFake(t, 40, Options{Prompt: "> "})
	const n = 200
	go func() {
		for i := range n {
			f.keys.Write([]byte(fmt.Sprintf("line %d\r", i)))
		}
	}()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range n {
			fmt.Fprintf(c, "out %d\n", i)
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("writes blocked while nobody read the lines")
	}
	for i := range n {
		if l, _ := next(t, c.Lines()); l != fmt.Sprintf("line %d", i) {
			t.Fatalf("line %d = %q", i, l)
		}
	}
}

func TestConsoleQuits(t *testing.T) {
	for _, quit := range []string{"\x03", "\x04"} {
		c, f := openFake(t, 40, Options{Prompt: "> "})
		// Lines submitted before quitting still arrive.
		f.typeKeys(t, "first\rx\x03"+quit)
		if l, ok := next(t, c.Lines()); !ok || l != "first" {
			t.Fatalf("%q: line %q, %v", quit, l, ok)
		}
		if l, ok := next(t, c.Lines()); ok {
			t.Fatalf("%q: line %q after quitting", quit, l)
		}
		// Output still shows; the input line is gone.
		fmt.Fprint(c, "bye\n")
		f.waitScreen(t, "> first", "bye")
	}
}

func TestConsoleInputEnds(t *testing.T) {
	c, f := openFake(t, 40, Options{})
	f.typeKeys(t, "last\r")
	f.keys.Close()
	if l, ok := next(t, c.Lines()); !ok || l != "last" {
		t.Fatalf("line %q, %v", l, ok)
	}
	if _, ok := next(t, c.Lines()); ok {
		t.Fatal("lines not closed at the end of the input")
	}
}

func TestConsoleClose(t *testing.T) {
	c, f := openFake(t, 40, Options{Prompt: "> "})
	fmt.Fprint(c, "partial")
	f.typeKeys(t, "typed")
	f.waitScreen(t, "partial", "> typed")
	c.Close()
	c.Close()
	if got := f.lines(); !slices.Equal(got, []string{"partial"}) {
		t.Errorf("after Close: %q", got)
	}
	f.mu.Lock()
	raw, restored := f.raw.String(), f.restored
	f.mu.Unlock()
	if restored != 1 || !strings.HasPrefix(raw, "\x1b[?2004h") || !strings.HasSuffix(raw, "\x1b[?2004l") {
		t.Errorf("restored %d times, bracketed paste on and off: %v", restored, strings.HasSuffix(raw, "\x1b[?2004l"))
	}
	if _, err := c.Write([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Errorf("Write after Close: %v", err)
	}
	if _, ok := <-c.Lines(); ok {
		t.Error("Lines open after Close")
	}
}

func TestConsoleRedrawsOnResize(t *testing.T) {
	c, f := openFake(t, 20, Options{Prompt: "> "})
	f.typeKeys(t, strings.Repeat("a", 30))
	f.waitScreen(t, "> "+strings.Repeat("a", 16)) // the cursor takes a column
	f.mu.Lock()
	f.width, f.v.cols = 40, 40
	f.mu.Unlock()
	f.resized <- struct{}{}
	f.waitScreen(t, "> "+strings.Repeat("a", 30))
	_ = c
}

func TestConsoleRingsWhenFull(t *testing.T) {
	_, f := openFake(t, 40, Options{Prompt: "> ", MaxRunes: 3})
	f.typeKeys(t, "abcd")
	f.waitScreen(t, "> abc")
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(f.raw.String(), "\a") {
		t.Error("no bell when the line is full")
	}
}

func TestPlainConsole(t *testing.T) {
	var out syncBuffer
	c := Plain(strings.NewReader("one\ntwo\n\nthree"), &out)
	defer c.Close()
	var got []string
	for l := range c.Lines() {
		got = append(got, l)
	}
	if !slices.Equal(got, []string{"one", "two", "", "three"}) {
		t.Errorf("lines %q", got)
	}
	fmt.Fprint(c, "red \x1b[31mtext\n")
	c.Close()
	if s := out.String(); s != "red [31mtext\n" {
		t.Errorf("printed %q", s)
	}
	if c.IsInteractive() {
		t.Error("plain console is interactive")
	}
}

func TestPlainConsoleLineTooLong(t *testing.T) {
	var out syncBuffer
	c := Plain(strings.NewReader("ok\n"+strings.Repeat("x", maxPaste+1)+"\nnever\n"), &out)
	defer c.Close()
	var got []string
	for l := range c.Lines() {
		got = append(got, l)
	}
	if !slices.Equal(got, []string{"ok"}) || !strings.Contains(out.String(), "too long") {
		t.Errorf("lines %q, printed %q", got, out.String())
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestConsoleNarrowsWithAReplyBeingWritten(t *testing.T) {
	c, f := openFake(t, 30, Options{Prompt: "> "})
	fmt.Fprint(c, "doggie> "+strings.Repeat("x", 15))
	f.waitScreen(t, "doggie> xxxxxxxxxxxxxxx", ">")
	f.mu.Lock()
	f.width, f.v.cols = 10, 10
	f.mu.Unlock()
	f.resized <- struct{}{}
	f.waitScreen(t, "doggie> xx", "xxxxxxxxxx", "xxx", ">")
	// The next redraw finds the region again.
	f.typeKeys(t, "a")
	f.waitScreen(t, "doggie> xx", "xxxxxxxxxx", "xxx", "> a")
}

// Many writers, typing and two Closes at once: no panic, no deadlock, and a
// write that loses the race with Close fails with ErrClosed.
func TestConsoleUnderLoad(t *testing.T) {
	for round := range 20 {
		c, f := openFake(t, 20+round, Options{Prompt: "> "})
		var wg sync.WaitGroup
		for w := range 8 {
			wg.Go(func() {
				for i := range 50 {
					if _, err := fmt.Fprintf(c, "w%d-%d 日本 ", w, i); err != nil {
						if !errors.Is(err, ErrClosed) {
							t.Errorf("write: %v", err)
						}
						return
					}
				}
			})
		}
		wg.Go(func() {
			for i := range 30 {
				if _, err := f.keys.Write([]byte(fmt.Sprintf("línea %d\r\x1b[A", i))); err != nil {
					return
				}
			}
		})
		wg.Go(func() {
			for range c.Lines() {
			}
		})
		time.Sleep(time.Duration(round) * time.Millisecond)
		closed := make(chan struct{})
		go func() { c.Close(); close(closed) }()
		c.Close()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("Close deadlocked")
		}
		f.keys.Close()
		wg.Wait()
	}
}

// brokenTerm fails in every way a terminal can.
type brokenTerm struct{ readErr chan struct{} }

func (b brokenTerm) Read([]byte) (int, error) {
	<-b.readErr
	return 0, errors.New("input/output error")
}
func (brokenTerm) Write([]byte) (int, error) { return 0, errors.New("terminal gone") }
func (brokenTerm) Width() (int, error)       { return 0, errors.New("no size") }
func (brokenTerm) Resized() <-chan struct{}  { return nil }
func (brokenTerm) Restore() error            { return errors.New("can't restore") }

func TestConsoleOnABrokenTerminal(t *testing.T) {
	b := brokenTerm{readErr: make(chan struct{})}
	c := Interactive(b, Options{Prompt: "> "})
	if _, err := c.Write([]byte("text\n")); err != nil {
		t.Errorf("Write: %v", err)
	}
	close(b.readErr)
	if _, ok := next(t, c.Lines()); ok {
		t.Error("lines open after the input failed")
	}
	done := make(chan struct{})
	go func() { c.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung on a broken terminal")
	}
}

// A zero width falls back to a usable one.
func TestConsoleWithoutAWidth(t *testing.T) {
	f := newFakeTerm(t, 80) // drawn on 80 columns
	f.width = 0             // but the terminal can't tell its width
	c := Interactive(f, Options{Prompt: "> "})
	t.Cleanup(func() { c.Close() })
	f.typeKeys(t, strings.Repeat("a", 100))
	f.waitScreen(t, "> "+strings.Repeat("a", 76))
}

func TestConsoleStatus(t *testing.T) {
	c, f := openFake(t, 40, Options{Prompt: "you> "})
	f.typeKeys(t, "abc")
	c.SetStatus("· t3 12/30 s")
	f.waitScreen(t, "· t3 12/30 s", "you> abc")
	fmt.Fprint(c, "doggie> Voy")
	f.waitScreen(t, "doggie> Voy", "· t3 12/30 s", "you> abc")
	fmt.Fprint(c, ".\n")
	c.SetStatus("· t3 13/30 s")
	f.waitScreen(t, "doggie> Voy.", "· t3 13/30 s", "you> abc")
	c.SetStatus("")
	f.waitScreen(t, "doggie> Voy.", "you> abc")
	c.SetStatus("· t4 asks a question")
	c.Close()
	if got := f.lines(); !slices.Equal(got, []string{"doggie> Voy."}) {
		t.Errorf("after Close the screen is %q", got)
	}
	c.SetStatus("after close") // must not block
}

func TestPlainConsoleIgnoresStatus(t *testing.T) {
	var out syncBuffer
	c := Plain(strings.NewReader(""), &out)
	c.SetStatus("· t3 12/30 s")
	c.Close()
	if s := out.String(); s != "" {
		t.Errorf("printed %q", s)
	}
}

func recvHidden(t *testing.T, ch <-chan string) (string, bool) {
	t.Helper()
	select {
	case l, ok := <-ch:
		return l, ok
	case <-time.After(5 * time.Second):
		t.Fatal("no hidden line")
		return "", false
	}
}

func TestConsoleHiddenLine(t *testing.T) {
	c, f := openFake(t, 40, Options{Prompt: "you> ", Forget: func(l string) bool { return strings.HasPrefix(l, "/answer") }})
	f.typeKeys(t, "/answer t5\r")
	if l, _ := next(t, c.Lines()); l != "/answer t5" {
		t.Fatalf("line %q", l)
	}
	answer := c.AskHidden("answer for t5 (hidden): ")
	f.waitScreen(t, "you> /answer t5", "answer for t5 (hidden):")
	f.typeKeys(t, "1234\r")
	if l, ok := recvHidden(t, answer); !ok || l != "1234" {
		t.Fatalf("hidden line %q, %v", l, ok)
	}
	f.waitScreen(t, "you> /answer t5", "answer for t5 (hidden): •••", "you>")
	f.typeKeys(t, "\x1b[A") // Up: neither line was kept
	f.waitScreen(t, "you> /answer t5", "answer for t5 (hidden): •••", "you>")

	// The hidden line never shows on Lines, and a newer ask cancels the
	// one before.
	first := c.AskHidden("? ")
	second := c.AskHidden("?? ")
	if l, _ := recvHidden(t, first); l != "" {
		t.Errorf("replaced ask got %q", l)
	}
	f.typeKeys(t, "secreto\r")
	if l, _ := recvHidden(t, second); l != "secreto" {
		t.Errorf("hidden line %q", l)
	}
	f.typeKeys(t, "visible\r")
	if l, _ := next(t, c.Lines()); l != "visible" {
		t.Errorf("line %q", l)
	}

	// Closing ends a pending ask.
	pending := c.AskHidden("? ")
	f.waitScreen(t, "you> /answer t5", "answer for t5 (hidden): •••", "?? •••", "you> visible", "?")
	c.Close()
	if _, ok := recvHidden(t, pending); ok {
		t.Error("a pending ask wasn't closed")
	}
	if _, ok := recvHidden(t, c.AskHidden("after close")); ok {
		t.Error("an ask after Close wasn't closed")
	}
	p := Plain(strings.NewReader(""), &syncBuffer{})
	if _, ok := recvHidden(t, p.AskHidden("? ")); ok {
		t.Error("a plain console asked for a hidden line")
	}
	p.Close()
}
