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
