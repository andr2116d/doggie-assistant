package console

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// slowFrames makes paced text and the activity line move slowly enough to
// watch, for the test.
func slowFrames(t *testing.T) {
	t.Helper()
	oldFrame, oldCatch, oldAnim := frameEvery, catchUp, animEvery
	frameEvery, catchUp, animEvery = 20*time.Millisecond, 400*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { frameEvery, catchUp, animEvery = oldFrame, oldCatch, oldAnim })
}

func (f *fakeTerm) rawOutput() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.raw.String()
}

// waitFor waits until the screen satisfies ok.
func (f *fakeTerm) waitFor(t *testing.T, what string, ok func([]string) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ok(f.lines()) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: screen %q", what, f.lines())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Paced text shows a little at a time, all of it in the end.
func TestPacedTextFlows(t *testing.T) {
	slowFrames(t)
	c, f := openFake(t, 60, Options{Prompt: "> "})
	text := "Listo, te aviso en tres minutos cuando el té esté."
	c.Print(Text{Text: text, Paced: true})
	partway := false
	f.waitFor(t, "the whole text", func(rows []string) bool {
		if len(rows) == 2 && rows[0] != "" && rows[0] != text && strings.HasPrefix(text, rows[0]) {
			partway = true
		}
		return len(rows) == 2 && rows[0] == text
	})
	if !partway {
		t.Error("the text never showed partway")
	}
}

// Text that isn't paced shows what is held back first, so the order holds.
func TestUnpacedTextKeepsTheOrder(t *testing.T) {
	slowFrames(t)
	c, f := openFake(t, 60, Options{Prompt: "> "})
	c.Print(Text{Text: "DOGGIE "}, Text{Text: "un texto que va saliendo de a poco", Paced: true})
	c.Print(Text{Text: "\n· nota\n"})
	f.waitScreen(t, "DOGGIE un texto que va saliendo de a poco", "· nota", ">")
}

func TestClosingShowsPacedText(t *testing.T) {
	slowFrames(t)
	c, f := openFake(t, 60, Options{Prompt: "> "})
	c.Print(Text{Text: "todo esto tiene que verse al cerrar\n", Paced: true})
	c.Close()
	if got := f.lines(); len(got) != 1 || got[0] != "todo esto tiene que verse al cerrar" {
		t.Errorf("after closing: %q", got)
	}
}

func TestActivityLine(t *testing.T) {
	slowFrames(t)
	c, f := openFake(t, 40, Options{Prompt: "> "})
	c.SetStatus("⏱ té 2:41")
	c.SetActivity("thinking")
	seen := map[string]bool{}
	f.waitFor(t, "the spinner moving", func(rows []string) bool {
		if len(rows) == 3 && strings.HasSuffix(rows[0], " thinking") {
			seen[rows[0]] = true
		}
		return len(seen) >= 3
	})
	if rows := f.lines(); rows[1] != "⏱ té 2:41" || rows[2] != ">" {
		t.Errorf("activity, status and input in that order: %q", rows)
	}
	c.SetActivity("")
	f.waitScreen(t, "⏱ té 2:41", ">")
	if c.Print(Text{Text: "x\n"}) != nil {
		t.Fatal("print failed")
	}
	f.waitScreen(t, "x", "⏱ té 2:41", ">")
}

func TestActivityShowsItsTime(t *testing.T) {
	s := &screen{cols: 30, activity: []rune("thinking"), elapsed: 65 * time.Second}
	s.style(nil, NoColor)
	bb := new(bytes.Buffer)
	s.drawActivity(bb)
	if got := bb.String(); got != "⠋ thinking · 1m 05s" {
		t.Errorf("%q", got)
	}
	// Too narrow for all of it: the label is cut and the time stays.
	s.cols, s.activity = 16, []rune("searching the web")
	bb.Reset()
	s.drawActivity(bb)
	if got := bb.String(); got != "⠋ sear… · 1m 05s" || runewidthOf(got) > s.cols {
		t.Errorf("narrow: %q", got)
	}
	s.elapsed = time.Second
	bb.Reset()
	s.drawActivity(bb)
	if got := bb.String(); got != "⠋ searching the…" {
		t.Errorf("short: %q", got)
	}
}

// Every frame is a synchronized update, styled when there is color.
func TestFramesAreSynchronized(t *testing.T) {
	c, f := openFake(t, 40, Options{Prompt: "> ", PromptRole: Accent, Colors: ANSI16})
	c.Print(Text{Text: "DOGGIE", Role: Assistant}, Text{Text: " hola\n"})
	f.waitScreen(t, "DOGGIE hola", ">")
	raw := f.rawOutput()
	if b, e := strings.Count(raw, syncBegin), strings.Count(raw, syncEnd); b == 0 || b != e {
		t.Errorf("%d frame starts, %d ends", b, e)
	}
	if !strings.Contains(raw, "\x1b[1;33mDOGGIE\x1b[0m hola") || !strings.Contains(raw, "\x1b[33m> \x1b[0m") {
		t.Errorf("not styled: %q", raw)
	}
}

func TestEchoGap(t *testing.T) {
	c, f := openFake(t, 40, Options{Prompt: "› ", EchoGap: true})
	c.Print(Text{Text: "DOGGIE hola\n"})
	f.waitScreen(t, "DOGGIE hola", "›")
	f.typeKeys(t, "qué tal\r")
	next(t, c.Lines())
	f.waitScreen(t, "DOGGIE hola", "", "› qué tal", "›")
}

// holdFrames stops paced text from moving on its own, for the test.
func holdFrames(t *testing.T) {
	t.Helper()
	old := frameEvery
	frameEvery = time.Hour
	t.Cleanup(func() { frameEvery = old })
}

// Paced text arriving in many small writes waits for the next frame
// instead of redrawing the screen for each.
func TestPacedWritesWaitForTheirFrame(t *testing.T) {
	holdFrames(t)
	c, f := openFake(t, 40, Options{Prompt: "> "})
	f.waitScreen(t, ">")
	before := strings.Count(f.rawOutput(), syncBegin)
	for _, w := range strings.Fields("uno dos tres cuatro cinco seis") {
		c.Print(Text{Text: w + " ", Paced: true})
	}
	c.SetStatus("st") // a frame after the writes, to know they arrived
	f.waitScreen(t, "st", ">")
	if n := strings.Count(f.rawOutput(), syncBegin) - before; n != 1 {
		t.Errorf("%d frames for paced writes and a status", n)
	}
	c.Print(Text{Text: "\n"})
	f.waitScreen(t, "uno dos tres cuatro cinco seis", "st", ">")
}

// What the user submits shows after the rows of a reply that already came,
// even if they were still held back.
func TestSubmitShowsHeldRowsFirst(t *testing.T) {
	holdFrames(t)
	c, f := openFake(t, 40, Options{Prompt: "> "})
	c.Print(Text{Text: "línea uno\nlínea dos", Paced: true})
	f.typeKeys(t, "hola\r")
	next(t, c.Lines())
	f.waitScreen(t, "línea uno", "> hola", "línea dos", ">")
}
