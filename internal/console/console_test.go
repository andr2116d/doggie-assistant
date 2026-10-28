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

// fakeTerm is a terminal the test types into, keeping all it is sent.
type fakeTerm struct {
	keys    *io.PipeWriter
	in      *io.PipeReader
	resized chan struct{}

	mu       sync.Mutex
	raw      bytes.Buffer
	width    int
	height   int
	restored int
}

// newFullFake is a fake terminal w×h.
func newFullFake(t *testing.T, w, h int) *fakeTerm {
	in, keys := io.Pipe()
	return &fakeTerm{keys: keys, in: in, resized: make(chan struct{}, 1), width: w, height: h}
}

func (f *fakeTerm) Read(p []byte) (int, error) { return f.in.Read(p) }

func (f *fakeTerm) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.raw.Write(p)
}

func (f *fakeTerm) Width() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.width, nil
}

func (f *fakeTerm) Height() (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.height, nil
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

func (f *fakeTerm) rawOutput() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.raw.String()
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

// Whoever reads Lines may be writing to the screen at the same time: the
// screen must not wait for it.
func TestScreenNeverBlocksOnItsReader(t *testing.T) {
	s, f := openScreen(t, 60, 16, ScreenOptions{})
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
			s.Reply(fmt.Sprint(i), fmt.Sprintf("out %d", i))
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the screen blocked while nobody read the lines")
	}
	for i := range n {
		if l, _ := next(t, s.Lines()); l != fmt.Sprintf("line %d", i) {
			t.Fatalf("line %d = %q", i, l)
		}
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
func (brokenTerm) Height() (int, error)      { return 0, errors.New("no size") }
func (brokenTerm) Resized() <-chan struct{}  { return nil }
func (brokenTerm) Restore() error            { return errors.New("can't restore") }

func TestScreenOnABrokenTerminal(t *testing.T) {
	b := brokenTerm{readErr: make(chan struct{})}
	s := NewScreen(b, ScreenOptions{Name: "DOGGIE"})
	s.Reply("r", "text")
	if _, ok := next(t, s.Lines()); ok {
		t.Error("lines open on a terminal that takes nothing")
	}
	close(b.readErr)
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung on a broken terminal")
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
	c.SetStatus("· t3 12/30 s")
	if _, ok := <-c.AskHidden("answer: "); ok {
		t.Error("a hidden line without a terminal")
	}
	c.Close()
	if s := out.String(); s != "red [31mtext\n" {
		t.Errorf("printed %q", s)
	}
	if c.IsInteractive() {
		t.Error("plain console is interactive")
	}
	if _, err := c.Write([]byte("late")); !errors.Is(err, ErrClosed) {
		t.Errorf("Write after Close: %v", err)
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
