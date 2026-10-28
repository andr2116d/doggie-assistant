package console

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"testing"

	"github.com/clipperhouse/uax29/v2/graphemes"
)

// fullVT emulates a full-screen terminal for the sequences the painter
// writes: cursor moves, clearing, styles kept per cell, wide clusters and
// the pending wrap of the last column, which here may never scroll.
type fullVT struct {
	t          *testing.T
	w, h       int
	text       [][]string // "" is the second half of a wide cluster
	style      [][]string // the style sequence each cell was drawn with
	x, y       int
	pending    bool
	sgr        string
	cursorShow bool
	modes      map[string]bool
}

func newFullVT(t *testing.T, w, h int) *fullVT {
	v := &fullVT{t: t, w: w, h: h, modes: map[string]bool{}}
	v.clear()
	return v
}

func (v *fullVT) clear() {
	v.text, v.style = make([][]string, v.h), make([][]string, v.h)
	for y := range v.h {
		v.text[y], v.style[y] = make([]string, v.w), make([]string, v.w)
		for x := range v.w {
			v.text[y][x], v.style[y][x] = " ", v.sgr
		}
	}
}

func (v *fullVT) Write(p []byte) (int, error) {
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
		it := graphemes.FromString(s)
		it.Next()
		g := it.Value()
		s = s[len(g):]
		for _, r := range g {
			if !printable(r) {
				v.t.Fatalf("control character %U reached the terminal", r)
			}
		}
		v.put(g)
	}
	return len(p), nil
}

func (v *fullVT) put(g string) {
	w := clusterWidth(g)
	if w == 0 {
		v.t.Fatalf("zero-width cluster %q written alone", g)
	}
	if v.pending || v.x+w > v.w {
		if v.y == v.h-1 {
			v.t.Fatalf("writing %q past the last row would scroll", g)
		}
		v.x, v.y, v.pending = 0, v.y+1, false
	}
	// Overwriting half of a wide cluster blanks the other half.
	for _, x := range []int{v.x, v.x + w - 1} {
		if v.text[v.y][x] == "" && x > 0 {
			v.text[v.y][x-1] = " "
		}
		if x+1 < v.w && clusterWidth(v.text[v.y][x]) == 2 {
			v.text[v.y][x+1] = " "
		}
	}
	v.text[v.y][v.x], v.style[v.y][v.x] = g, v.sgr
	if w == 2 {
		v.text[v.y][v.x+1], v.style[v.y][v.x+1] = "", v.sgr
	}
	v.x += w
	if v.x >= v.w {
		v.x, v.pending = v.w-1, true
	}
}

func (v *fullVT) csi(params string, final byte) {
	switch final {
	case 'm':
		if params == "0" || params == "" {
			v.sgr = ""
		} else {
			v.sgr = params
		}
		return
	case 'h', 'l':
		v.modes[params] = final == 'h'
		if params == "?25" {
			v.cursorShow = final == 'h'
		}
		return
	}
	v.pending = false
	switch final {
	case 'H':
		row, col := 1, 1
		if params != "" {
			parts := strings.Split(params, ";")
			row, _ = strconv.Atoi(parts[0])
			if len(parts) > 1 {
				col, _ = strconv.Atoi(parts[1])
			}
		}
		if row < 1 || col < 1 || row > v.h || col > v.w {
			v.t.Fatalf("cursor moved off screen to %d;%d", row, col)
		}
		v.y, v.x = row-1, col-1
	case 'J':
		if params != "2" {
			v.t.Fatalf("unexpected erase %q", params)
		}
		v.clear()
	default:
		v.t.Fatalf("unexpected sequence %q%c", params, final)
	}
}

// matches fails the test unless the terminal shows g as drawn with styles.
func (v *fullVT) matches(g *grid, styles *styleTable, p Profile) {
	v.t.Helper()
	for y := range g.H {
		for x := range g.W {
			c := g.cells[y*g.W+x]
			want := c.text
			if c.w == 1 && want == "" {
				want = " "
			}
			var sb bytes.Buffer
			styles.sgr(&sb, c.s, p)
			wantStyle := strings.TrimSuffix(strings.TrimPrefix(sb.String(), "\x1b["), "m")
			if wantStyle == "0" {
				wantStyle = ""
			}
			if v.text[y][x] != want {
				v.t.Fatalf("cell %d,%d shows %q, want %q\n%s", x, y, v.text[y][x], want, v.dump())
			}
			if v.style[y][x] != wantStyle {
				v.t.Fatalf("cell %d,%d styled %q, want %q", x, y, v.style[y][x], wantStyle)
			}
		}
	}
}

func (v *fullVT) dump() string {
	var b strings.Builder
	for _, row := range v.text {
		b.WriteString("|" + strings.Join(row, "") + "|\n")
	}
	return b.String()
}

// Whatever is drawn, frame after frame, the terminal ends up showing the
// grid: drawing only what changed is the same as drawing everything.
func FuzzPainter(f *testing.F) {
	f.Add([]byte{3, 7, 1, 200, 4, 9, 33, 0, 81}, uint8(20), uint8(6), uint8(2))
	f.Add([]byte("日本語 👍🏽 é a\tb \x1b[2J"), uint8(9), uint8(3), uint8(3))
	f.Fuzz(func(t *testing.T, ops []byte, w, h, prof uint8) {
		cols, rows := 1+int(w)%40, 1+int(h)%12
		p := Profile(prof % 4)
		styles := newStyleTable()
		ids := []styleID{0,
			styles.id(Style{Fg: Hex(0xa78bfa, 13), Bold: true}),
			styles.id(Style{Bg: Hex(0x100e16, 0)}),
			styles.id(Style{Fg: Hex(0x6ee7b7, 10), Bg: Hex(0x201b2c, 5), Italic: true}),
		}
		texts := []string{"a", "hola", "日本", "👍🏽x", "é", "\t", "\x1b[31m", "🇵🇪", "  ", "ñandú"}
		v := newFullVT(t, cols, rows)
		pt := newPainter(styles, p)
		var g grid
		g.resize(cols, rows)
		for i := 0; i+3 < len(ops) && i < 400; i += 4 {
			x, y := int(ops[i])%(cols+1), int(ops[i+1])%rows
			st := ids[int(ops[i+2])%len(ids)]
			switch ops[i+3] % 6 {
			case 0:
				g.fill(x, y, int(ops[i+2])%cols+1, int(ops[i+3]>>4)%rows+1, st)
			case 1:
				pt.invalidate()
			default:
				g.text(x, y, cols, texts[int(ops[i+3])%len(texts)], st)
			}
			if ops[i]%3 == 0 || i+7 >= len(ops) {
				out := pt.frame(&g, x, y, ops[i+1]%2 == 0)
				v.Write(out)
				v.matches(&g, styles, p)
				if again := pt.frame(&g, x, y, ops[i+1]%2 == 0); again != nil {
					t.Fatalf("an unchanged frame wrote %q", again)
				}
			}
		}
	})
}

func TestPainterWritesOnlyChanges(t *testing.T) {
	styles := newStyleTable()
	violet := styles.id(Style{Fg: Hex(0xa78bfa, 13)})
	var g grid
	g.resize(30, 4)
	p := newPainter(styles, TrueColor)
	v := newFullVT(t, 30, 4)
	g.text(0, 0, 30, "DOGGIE", violet)
	g.text(8, 0, 30, "Listo, te aviso", 0)
	v.Write(p.frame(&g, 0, 3, true))
	v.matches(&g, styles, TrueColor)
	if !v.cursorShow {
		t.Error("the cursor is hidden")
	}

	g.text(23, 0, 30, " en", 0)
	out := string(p.frame(&g, 0, 3, true))
	v.Write([]byte(out))
	v.matches(&g, styles, TrueColor)
	inner := strings.TrimSuffix(strings.TrimPrefix(out, syncBegin), syncEnd)
	if want := "\x1b[?25l\x1b[1;25Hen\x1b[4;1H\x1b[?25h"; inner != want {
		t.Errorf("a small change wrote %q, want %q", inner, want)
	}
	if p.frame(&g, 0, 3, true) != nil {
		t.Error("an unchanged frame wrote something")
	}
	if out := string(p.frame(&g, 5, 3, true)); !strings.Contains(out, "\x1b[4;6H") || strings.Contains(out, "?25l") {
		t.Errorf("moving the cursor alone: %q", out)
	}
	if out := string(p.frame(&g, 5, 3, false)); !strings.Contains(out, "\x1b[?25l") {
		t.Errorf("hiding the cursor: %q", out)
	}
}

// A wide cluster changed in its second half is written from its start.
func TestPainterWideClusters(t *testing.T) {
	styles := newStyleTable()
	var g grid
	g.resize(6, 1)
	p := newPainter(styles, NoColor)
	v := newFullVT(t, 6, 1)
	g.text(0, 0, 6, "a日本", 0)
	v.Write(p.frame(&g, 0, 0, false))
	g.text(2, 0, 6, "x", 0) // cuts 日 in two: its first half blanks
	v.Write(p.frame(&g, 0, 0, false))
	v.matches(&g, styles, NoColor)
	if got := v.dump(); got != "|a x本 |\n" {
		t.Errorf("screen %q", got)
	}
	// A cluster that doesn't fit before the end of the row isn't written.
	if x := g.text(5, 0, 6, "語", 0); x != 5 {
		t.Errorf("a wide cluster at the last column moved to %d", x)
	}
}

func TestGridDropsControls(t *testing.T) {
	var g grid
	g.resize(20, 1)
	g.text(0, 0, 20, "a\x1b[2Jb\x07c\u009bd\xffe\tf", 0)
	var got strings.Builder
	for _, c := range g.cells {
		if c.text == "" && c.w == 1 {
			got.WriteByte(' ')
		} else {
			got.WriteString(c.text)
		}
	}
	if s := strings.TrimRight(got.String(), " "); s != "a[2Jbcde f" {
		t.Errorf("%q", s)
	}
}

func TestStyleSequences(t *testing.T) {
	styles := newStyleTable()
	id := styles.id(Style{Fg: Hex(0xa78bfa, 13), Bg: Hex(0x100e16, 0), Bold: true})
	for p, want := range map[Profile]string{
		NoColor:   "",
		ANSI16:    "\x1b[0;1;95;40m",
		ANSI256:   "\x1b[0;1;38;5;141;48;5;233m",
		TrueColor: "\x1b[0;1;38;2;167;139;250;48;2;16;14;22m",
	} {
		var b bytes.Buffer
		styles.sgr(&b, id, p)
		if got := b.String(); got != want {
			t.Errorf("profile %d: %q, want %q", p, got, want)
		}
	}
	if styles.id(Style{Fg: Hex(0xa78bfa, 13), Bg: Hex(0x100e16, 0), Bold: true}) != id {
		t.Error("the same style got a new id")
	}
}

// benchGrid is a full screen of a chat: a header, rows of text in a few
// styles and a panel.
func benchGrid(styles *styleTable, w, h int) *grid {
	var g grid
	g.resize(w, h)
	bg := styles.id(Style{Bg: Hex(0x100e16, 0)})
	violet := styles.id(Style{Fg: Hex(0xa78bfa, 13), Bg: Hex(0x100e16, 0), Bold: true})
	panel := styles.id(Style{Fg: Hex(0x948da8, 8), Bg: Hex(0x14111b, 0)})
	g.fill(0, 0, w, h, bg)
	g.text(1, 0, w, "◆ DOGGIE   ● en línea · terminal", violet)
	for y := 2; y < h-3; y++ {
		g.text(2, y, w-32, fmt.Sprintf("Línea %d de la conversación, con algo de texto largo y acentos: ñandú", y), bg)
		g.fill(w-30, y, 30, 1, panel)
		g.text(w-28, y, w, "⏱ ropa 7:41", panel)
	}
	return &g
}

func BenchmarkFrameFull(b *testing.B) {
	styles := newStyleTable()
	g := benchGrid(styles, 200, 60)
	p := newPainter(styles, TrueColor)
	b.ReportAllocs()
	n := 0
	for b.Loop() {
		p.invalidate()
		n = len(p.frame(g, 3, 58, true))
	}
	b.ReportMetric(float64(n), "bytes/frame")
}

// A frame of a reply being written: a few new cells on one row.
func BenchmarkFrameStreaming(b *testing.B) {
	styles := newStyleTable()
	g := benchGrid(styles, 200, 60)
	p := newPainter(styles, TrueColor)
	p.frame(g, 3, 58, true)
	words := []string{"hola ", "qué ", "tal ", "todo ", "bien "}
	rng := rand.New(rand.NewPCG(1, 2))
	b.ReportAllocs()
	n, x := 0, 2
	for b.Loop() {
		w := words[rng.IntN(len(words))]
		if x+len(w) > 160 {
			x = 2
		}
		x = g.text(x, 40, 160, w, 0)
		n = len(p.frame(g, 3, 58, true))
	}
	b.ReportMetric(float64(n), "bytes/frame")
}

func BenchmarkFrameIdle(b *testing.B) {
	styles := newStyleTable()
	g := benchGrid(styles, 200, 60)
	p := newPainter(styles, TrueColor)
	p.frame(g, 3, 58, true)
	b.ReportAllocs()
	for b.Loop() {
		if p.frame(g, 3, 58, true) != nil {
			b.Fatal("an idle frame wrote something")
		}
	}
}

func TestNoANSIKeepsTheTerminalsColor(t *testing.T) {
	styles := newStyleTable()
	id := styles.id(Style{Fg: Hex(0xa78bfa, 13), Bg: Hex(0x100e16, NoANSI)})
	var b bytes.Buffer
	styles.sgr(&b, id, ANSI16)
	if got := b.String(); got != "\x1b[0;95m" {
		t.Errorf("16 colors: %q", got)
	}
	b.Reset()
	styles.sgr(&b, id, ANSI256)
	if got := b.String(); got != "\x1b[0;38;5;141;48;5;233m" {
		t.Errorf("256 colors: %q", got)
	}
}

// An empty fill leaves a wide cluster at its edge alone.
func TestEmptyFill(t *testing.T) {
	var g grid
	g.resize(6, 2)
	g.text(2, 0, 6, "日", 0)
	g.fill(3, 0, -2, 1, 0)
	g.fill(3, 0, 0, 1, 0)
	if c := g.cells[2]; c.text != "日" || c.w != 2 {
		t.Errorf("the cluster was cut: %+v", c)
	}
}

// The first frame, and the first after invalidate, clear the terminal even
// when the grid is blank, since whatever was there before must go.
func TestPainterClearsFirst(t *testing.T) {
	var g grid
	g.resize(10, 3)
	p := newPainter(newStyleTable(), NoColor)
	if out := string(p.frame(&g, 0, 0, false)); !strings.Contains(out, "\x1b[2J") {
		t.Errorf("first frame: %q", out)
	}
	p.invalidate()
	if out := string(p.frame(&g, 0, 0, false)); !strings.Contains(out, "\x1b[2J") {
		t.Errorf("after invalidate: %q", out)
	}
}
