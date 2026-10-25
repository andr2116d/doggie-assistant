package console

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// vt emulates a terminal for the sequences the console writes, including the
// pending wrap of the last column: writing there leaves the cursor on it,
// and only the next printed rune moves to the next row. Rows are unbounded.
type vt struct {
	t           *testing.T
	cols        int
	rows        [][]string // cells; a wide rune leaves "" in its second cell
	row, col    int
	pendingWrap bool
}

func newVT(t *testing.T, cols int) *vt { return &vt{t: t, cols: cols, rows: [][]string{nil}} }

func (v *vt) ensureRow() {
	for len(v.rows) <= v.row {
		v.rows = append(v.rows, nil)
	}
}

func (v *vt) put(r rune) {
	w := width(r)
	if w == 0 {
		at := v.col - 1
		if v.pendingWrap {
			at = v.col // the cursor stays on the last rune
		}
		if row := v.rows[v.row]; at >= 0 && at < len(row) {
			row[at] += string(r)
		}
		return
	}
	if v.pendingWrap || v.col+w > v.cols {
		v.row, v.col, v.pendingWrap = v.row+1, 0, false
		v.ensureRow()
	}
	row := v.rows[v.row]
	for len(row) < v.col+w {
		row = append(row, " ")
	}
	row[v.col] = string(r)
	if w == 2 {
		row[v.col+1] = ""
	}
	v.rows[v.row] = row
	v.col += w
	if v.col >= v.cols {
		v.col, v.pendingWrap = v.cols-1, true
	}
}

func (v *vt) Write(p []byte) (int, error) {
	s := string(p)
	for len(s) > 0 {
		if strings.HasPrefix(s, "\x1b[") {
			i := 2
			for i < len(s) && (s[i] < 0x40 || s[i] > 0x7e) {
				i++
			}
			if i == len(s) {
				v.t.Fatalf("unterminated sequence %q", s)
			}
			v.csi(s[2:i], s[i])
			s = s[i+1:]
			continue
		}
		r, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		switch {
		case r == '\r':
			v.col, v.pendingWrap = 0, false
		case r == '\n':
			v.row, v.pendingWrap = v.row+1, false
			v.ensureRow()
		case r == '\a':
		case !printable(r):
			v.t.Fatalf("control character %U reached the terminal", r)
		default:
			v.put(r)
		}
	}
	return len(p), nil
}

func (v *vt) csi(params string, final byte) {
	n := 1
	if params != "" && params[0] != '?' {
		var err error
		if n, err = strconv.Atoi(params); err != nil {
			v.t.Fatalf("bad parameters %q", params)
		}
	}
	v.pendingWrap = false
	switch final {
	case 'A':
		v.row = max(v.row-n, 0)
	case 'C':
		v.col = min(v.col+n, v.cols-1)
	case 'J':
		v.rows = v.rows[:v.row+1]
		if row := v.rows[v.row]; len(row) > v.col {
			v.rows[v.row] = row[:v.col]
		}
	case 'h', 'l':
		if params != "?2004" {
			v.t.Fatalf("unexpected mode %q", params)
		}
	default:
		v.t.Fatalf("unexpected sequence %q%c", params, final)
	}
}

// lines returns the screen's rows as text, without trailing blanks.
func (v *vt) lines() []string {
	out := make([]string, len(v.rows))
	for i, row := range v.rows {
		out[i] = strings.TrimRight(strings.Join(row, ""), " ")
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

// rig runs a screen and an editor on a vt, as the console does.
type rig struct {
	t *testing.T
	v *vt
	s *screen
	e *editor
	b bytes.Buffer
	// model is what the screen should show above the live region.
	model   []string
	partial string
	status  string // the status row as it should show
}

func newRig(t *testing.T, cols int, prompt string) *rig {
	g := &rig{t: t, v: newVT(t, cols), s: &screen{cols: cols, prompt: []rune(prompt)}, e: newEditor(0, nil)}
	g.update(nil)
	return g
}

func (g *rig) update(f func()) {
	g.s.clear(&g.b)
	if f != nil {
		f()
	}
	g.s.draw(&g.b, g.e)
	g.v.Write(g.b.Bytes())
	g.b.Reset()
	g.check()
}

func (g *rig) print(text string) {
	g.modelPrint(text)
	g.update(func() { g.s.print(&g.b, text) })
}

// setStatus sets the status row; want is how it should show.
func (g *rig) setStatus(text, want string) {
	g.status = want
	g.update(func() { g.s.setStatus(text) })
}

func (g *rig) key(k key) {
	hidden, hiddenPrompt := g.e.hidden, string(g.e.hiddenPrompt)
	if act, line := g.e.apply(k); act == actSubmit {
		echo := string(g.s.prompt) + line + "\n"
		switch {
		case hidden && line == "":
			echo = ""
		case hidden:
			echo = hiddenPrompt + "•••\n"
		}
		saved := g.partial
		g.partial = ""
		g.modelPrint(echo)
		g.partial = saved
		g.update(func() { g.s.printAbove(&g.b, echo) })
		return
	}
	g.update(nil)
}

func (g *rig) typeText(s string) {
	for _, c := range s {
		g.key(r(c))
	}
}

// modelPrint wraps text the simplest way: a row ends at a newline or when
// the next rune doesn't fit.
func (g *rig) modelPrint(text string) {
	for _, c := range clean(text) {
		if c == '\n' {
			g.model = append(g.model, g.partial)
			g.partial = ""
			continue
		}
		w := width(c)
		if w == 0 && g.partial == "" {
			continue
		}
		if runewidthOf(g.partial)+w > g.s.cols {
			g.model = append(g.model, g.partial)
			g.partial = ""
		}
		g.partial += string(c)
	}
}

func runewidthOf(s string) int { return widthOf([]rune(s)) }

// check compares the screen with the model: the finished rows, the partial
// row and the input line, with the cursor on it at the editor's cursor.
func (g *rig) check() {
	g.t.Helper()
	want := slices.Clone(g.model)
	if g.partial != "" {
		want = append(want, g.partial)
	}
	if g.status != "" {
		want = append(want, g.status)
	}
	input := g.inputRow()
	want = append(want, input)
	got := g.v.lines()
	trim := func(ls []string) []string {
		out := make([]string, len(ls))
		for i, l := range ls {
			// A zero-width rune at the start of a row has nothing to
			// attach to, on a real terminal too.
			l = strings.Map(func(c rune) rune {
				if width(c) == 0 {
					return -1
				}
				return c
			}, l)
			out[i] = strings.TrimRight(l, " ")
		}
		for len(out) > 0 && out[len(out)-1] == "" {
			out = out[:len(out)-1]
		}
		return out
	}
	if !slices.Equal(trim(got), trim(want)) {
		g.t.Fatalf("screen:\n%q\nwant:\n%q", trim(got), trim(want))
	}
	if g.v.row != len(want)-1 {
		g.t.Fatalf("cursor on row %d, want the input row %d", g.v.row, len(want)-1)
	}
	prompt := g.shownPrompt()
	col := runewidthOf(prompt) + widthOf(g.e.line[g.s.scroll:g.e.pos])
	if g.e.hidden {
		col = runewidthOf(prompt)
	}
	if g.v.col != col {
		g.t.Fatalf("cursor at column %d, want %d", g.v.col, col)
	}
	for i, l := range got {
		if w := runewidthOf(l); w > g.s.cols {
			g.t.Fatalf("row %d is %d columns wide", i, w)
		}
	}
	if w := runewidthOf(input); w > g.s.cols-1 {
		g.t.Fatalf("input row is %d columns wide on %d", w, g.s.cols)
	}
}

func (g *rig) shownPrompt() string {
	p := g.s.prompt
	if g.e.hidden {
		p = g.e.hiddenPrompt
	}
	for len(p) > 0 && widthOf(p) > g.s.cols-3 {
		p = p[:len(p)-1]
	}
	return string(p)
}

// inputRow is the prompt and the part of the line that fits, from the
// scroll offset the screen chose; check verifies the cursor is within it.
func (g *rig) inputRow() string {
	room := g.s.cols - 1 - runewidthOf(g.shownPrompt())
	shown := g.e.line[g.s.scroll:]
	if g.e.hidden {
		return g.shownPrompt()
	}
	for widthOf(shown) > room {
		shown = shown[:len(shown)-1]
	}
	if g.e.pos < g.s.scroll || g.e.pos > g.s.scroll+len(shown) {
		g.t.Fatalf("cursor %d outside the shown part [%d, %d]", g.e.pos, g.s.scroll, g.s.scroll+len(shown))
	}
	return g.shownPrompt() + string(shown)
}

func TestScreenStreamsAReplyWhileTyping(t *testing.T) {
	g := newRig(t, 30, "you> ")
	g.typeText("hola")
	g.print("doggie> Voy")
	g.typeText(" qué")
	g.print(", ya estoy en eso")
	g.key(k(keyEnter)) // the echo goes above the partial reply
	g.print(" y sigo.\n")
	want := []string{"you> hola qué", "doggie> Voy, ya estoy en eso y", " sigo.", "you>"}
	if got := g.v.lines(); !slices.Equal(got, want) {
		t.Errorf("screen:\n%s", strings.Join(got, "\n"))
	}
}

func TestScreenWrapsExactlyAtTheLastColumn(t *testing.T) {
	g := newRig(t, 10, "> ")
	g.print(strings.Repeat("a", 10))
	g.print(strings.Repeat("b", 20) + "\n")
	g.print("日本語日本語") // 12 columns: a wide rune never splits
	g.typeText("x")
	g.print("\n")
}

func TestScreenScrollsALongInput(t *testing.T) {
	g := newRig(t, 12, "you> ")
	g.typeText(strings.Repeat("abcdefghij", 5))
	g.key(k(keyHome))
	g.key(k(keyWordRight))
	g.typeText("日本😀")
	for range 30 {
		g.key(k(keyLeft))
	}
	g.key(k(keyEnd))
	for range 60 {
		g.key(k(keyBackspace))
	}
}

func TestScreenOnTinyTerminals(t *testing.T) {
	for _, cols := range []int{minCols, minCols + 1} {
		g := newRig(t, cols, "a very long prompt> ")
		g.typeText("hello world")
		g.print("some output that wraps a lot\n")
		g.key(k(keyEnter))
	}
}

func TestScreenNeutralizesEscapes(t *testing.T) {
	g := newRig(t, 40, "> ")
	g.print("red \x1b[31mtext\x1b[0m \x9b2J bell\a\r tab\tend\n")
	// vt fails on any sequence the console didn't write itself.
	if got := g.v.lines()[0]; got != "red [31mtext[0m 2J bell tab    end" {
		t.Errorf("printed %q", got)
	}
}

func TestScreenFinishLeavesOnlyOutput(t *testing.T) {
	g := newRig(t, 20, "you> ")
	g.print("done\npartial")
	g.typeText("typed")
	g.s.finish(&g.b)
	g.v.Write(g.b.Bytes())
	if got := g.v.lines(); !slices.Equal(got, []string{"done", "partial"}) || g.v.col != 0 {
		t.Errorf("after finish: %q, cursor at %d", got, g.v.col)
	}
}

// Random prints and keys keep the screen equal to the model.
func TestScreenRandom(t *testing.T) {
	pieces := []string{"a", "bc ", "ñandú ", "日本", "😀", "é", "\n", "doggie> ", "x\ty", "\x1b[2K", "​", strings.Repeat("z", 37)}
	keys := []key{k(keyLeft), k(keyRight), k(keyHome), k(keyEnd), k(keyBackspace), k(keyDelete),
		k(keyWordLeft), k(keyKillWord), k(keyUp), k(keyDown), k(keyEnter), k(keyInterrupt), paste("pegado 日本")}
	for seed := range 300 {
		rng := rand.New(rand.NewPCG(uint64(seed), 1))
		cols := minCols + rng.IntN(40)
		t.Run(fmt.Sprintf("seed=%d,cols=%d", seed, cols), func(t *testing.T) {
			g := newRig(t, cols, []string{"you> ", "", "日本> "}[rng.IntN(3)])
			for range 120 {
				switch rng.IntN(3) {
				case 0:
					g.print(pieces[rng.IntN(len(pieces))])
				case 1:
					g.typeText(pieces[rng.IntN(len(pieces)-1)])
				default:
					g.key(keys[rng.IntN(len(keys))])
				}
			}
		})
	}
}

// Editing a very long line stays cheap: the screen only measures what it
// shows.
func TestScreenLongLineIsCheap(t *testing.T) {
	s := &screen{cols: 80, prompt: []rune("> ")}
	e := newEditor(0, nil)
	e.apply(paste(strings.Repeat("palabra ", 25000)))
	var b bytes.Buffer
	start := time.Now()
	for _, k := range []key{k(keyHome), k(keyEnd), k(keyHome), k(keyWordRight), k(keyEnd), k(keyBackspace)} {
		e.apply(k)
		s.clear(&b)
		s.draw(&b, e)
		b.Reset()
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("6 keys on a %d-rune line took %v", len(e.line), d)
	}
}

// When the terminal narrows, the partial line is broken again to fit.
func TestScreenNarrowsWithAPartialLine(t *testing.T) {
	g := newRig(t, 30, "> ")
	g.print("doggie> " + strings.Repeat("x", 15))
	g.s.cols, g.v.cols = 10, 10
	// The rows above were drawn at the old width: rebuild the model at the
	// new one from the partial line only.
	g.model, g.partial = nil, ""
	g.modelPrint("doggie> " + strings.Repeat("x", 15))
	g.v = newVT(t, 10)
	g.s.drawn = false
	g.update(func() { g.s.refit(&g.b) })
	if w := widthOf(g.s.partial); w >= 10 {
		t.Errorf("partial line %d columns wide on 10", w)
	}
}

// A combining mark after a rune in the last column stays with it.
func TestScreenKeepsCombiningMarksAtTheEdge(t *testing.T) {
	g := newRig(t, 10, "> ")
	g.print("123456789éx\n")
	if got := g.v.lines(); got[0] != "123456789é" || got[1] != "x" {
		t.Errorf("screen %q", got)
	}
}

func TestStatusRow(t *testing.T) {
	g := newRig(t, 20, "you> ")
	g.typeText("hola")
	g.setStatus("· t3 12/30 s", "· t3 12/30 s")
	g.print("doggie> Voy")
	g.print(", ya.\n")
	g.setStatus(" a\tb\x1b[2J\nc ", "a b[2J c")
	g.setStatus("· t3 Esperar un rato largo: 12/30 s", "· t3 Esperar un rat…")
	g.setStatus("日本語日本語日本語日本語", "日本語日本語日本語…")
	g.print("doggie> una respuesta que no cabe en una fila")
	g.setStatus("", "")
	g.key(k(keyEnter))
	g.setStatus("x", "x")
	var b bytes.Buffer
	g.s.finish(&b)
	g.v.Write(b.Bytes())
	got := g.v.lines()
	if last := strings.TrimRight(got[len(got)-1], " "); last == "x" {
		t.Errorf("the status row stays after finishing: %q", got)
	}
}

func TestHiddenLineOnScreen(t *testing.T) {
	g := newRig(t, 30, "you> ")
	g.typeText("medio")
	g.update(func() { g.e.hide("answer for t5: ") })
	g.typeText("secreto")
	if strings.Contains(strings.Join(g.v.lines(), "\n"), "secreto") {
		t.Fatalf("the hidden line shows: %q", g.v.lines())
	}
	g.print("doggie> algo\n")
	g.key(k(keyEnter))
	if got := g.v.lines(); !slices.Contains(got, "answer for t5: •••") || got[len(got)-1] != "you> medio" {
		t.Errorf("screen = %q", got)
	}
}
