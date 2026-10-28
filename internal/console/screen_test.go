package console

import (
	"strings"
	"testing"
	"time"
)

func TestFullScreenEntersAndLeaves(t *testing.T) {
	f := newFullFake(t, 30, 5)
	s := newFullScreen(f, NoColor)
	if err := s.enter(); err != nil {
		t.Fatal(err)
	}
	s.leave()
	s.leave()
	if got := f.rawOutput(); got != enterFull+leaveFull {
		t.Errorf("%q", got)
	}
}

func TestFullScreenDrawsAndFits(t *testing.T) {
	f := newFullFake(t, 20, 4)
	s := newFullScreen(f, NoColor)
	s.enter()
	paint := func(text string) func(g *grid) (int, int, bool) {
		return func(g *grid) (int, int, bool) {
			g.fill(0, 0, g.W, g.H, 0)
			g.text(0, 0, g.W, text, 0)
			return 0, g.H - 1, true
		}
	}
	s.draw(paint("hola"))
	v := newFullVT(t, 20, 4)
	v.Write([]byte(strings.TrimPrefix(f.rawOutput(), enterFull)))
	v.matches(&s.g, s.styles, NoColor)

	f.mu.Lock()
	f.width, f.height = 12, 3
	f.mu.Unlock()
	if !s.fit() || s.fit() {
		t.Fatal("fit didn't see the new size once")
	}
	before := len(f.rawOutput())
	s.draw(paint("chau"))
	v = newFullVT(t, 12, 3)
	v.Write([]byte(f.rawOutput()[before:]))
	v.matches(&s.g, s.styles, NoColor)
	if v.text[0][0] != "c" || s.g.W != 12 {
		t.Errorf("after the resize: %q", v.dump())
	}
}

// A terminal that can't tell its size keeps the last one, or 80×24.
func TestFullScreenUnknownSize(t *testing.T) {
	f := newFullFake(t, 0, 0)
	s := newFullScreen(f, NoColor)
	s.enter()
	if s.g.W != 80 || s.g.H != 24 {
		t.Errorf("%dx%d", s.g.W, s.g.H)
	}
}

// A panic while drawing puts the terminal back before it goes on.
func TestFullScreenGuard(t *testing.T) {
	f := newFullFake(t, 20, 4)
	s := newFullScreen(f, NoColor)
	s.enter()
	defer func() {
		if r := recover(); r != "boom" {
			t.Fatalf("recovered %v", r)
		}
		if out := f.rawOutput(); !strings.HasSuffix(out, leaveFull) || f.restored != 1 {
			t.Errorf("not restored: %q, %d", out, f.restored)
		}
	}()
	s.guard(func() { panic("boom") })
}

// Changes before a frame is drawn share it, and frames are frameEvery
// apart; at rest the gate's channel is nil.
func TestFrameGate(t *testing.T) {
	old := frameEvery
	frameEvery = 30 * time.Millisecond
	t.Cleanup(func() { frameEvery = old })
	var g frameGate
	defer g.stop()
	if g.c != nil {
		t.Fatal("a frame is due at rest")
	}
	start := time.Now()
	g.want(start)
	g.want(start)
	<-g.c
	g.drawn(time.Now())
	if g.c != nil {
		t.Fatal("still due after drawing")
	}
	g.want(time.Now())
	<-g.c
	if d := time.Since(start); d < frameEvery {
		t.Errorf("two frames %v apart", d)
	}
}

// newFullFake is a fake terminal w×h, checked through its raw output.
func newFullFake(t *testing.T, w, h int) *fakeTerm {
	f := newFakeTerm(t, w)
	f.v, f.height = nil, h
	return f
}
