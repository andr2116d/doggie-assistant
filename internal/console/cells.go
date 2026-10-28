package console

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/clipperhouse/uax29/v2/graphemes"
	"github.com/mattn/go-runewidth"
)

// Color is a color with its fallbacks: RGB for 24-bit and 256 colors, ANSI
// (0-15) for the terminal's own 16, where NoANSI keeps the terminal's own
// color instead. The zero Color is the terminal's own foreground or
// background.
type Color struct {
	RGB  RGB
	ANSI uint8
	Set  bool
}

// NoANSI as a Color's ANSI keeps the terminal's own color in 16 colors,
// e.g. for a background meant only for terminals with more.
const NoANSI uint8 = 255

// Hex is the color #rrggbb, falling back to ansi in 16 colors.
func Hex(rgb uint32, ansi uint8) Color {
	return Color{RGB: RGB{uint8(rgb >> 16), uint8(rgb >> 8), uint8(rgb)}, ANSI: ansi, Set: true}
}

// Style is how a cell is drawn.
type Style struct {
	Fg, Bg                       Color
	Bold, Dim, Italic, Underline bool
}

// styleID indexes a styleTable; 0 is the terminal's own look.
type styleID uint16

// styleTable interns styles, so a cell holds a small index and comparing
// two cells is cheap.
type styleTable struct {
	styles []Style
	ids    map[Style]styleID
}

func newStyleTable() *styleTable {
	return &styleTable{styles: []Style{{}}, ids: map[Style]styleID{{}: 0}}
}

// maxStyles bounds the table; past it, new styles draw as the terminal's own.
const maxStyles = 1 << 12

func (t *styleTable) id(s Style) styleID {
	if id, ok := t.ids[s]; ok {
		return id
	}
	if len(t.styles) >= maxStyles {
		return 0
	}
	id := styleID(len(t.styles))
	t.styles = append(t.styles, s)
	t.ids[s] = id
	return id
}

// sgr writes the sequence that draws style id on profile p from any
// previous look: a full reset first, so nothing of the previous style
// leaks. Without color it writes nothing.
func (t *styleTable) sgr(b *bytes.Buffer, id styleID, p Profile) {
	if p == NoColor {
		return
	}
	s := t.styles[id]
	b.WriteString("\x1b[0")
	if s.Bold {
		b.WriteString(";1")
	}
	if s.Dim {
		b.WriteString(";2")
	}
	if s.Italic {
		b.WriteString(";3")
	}
	if s.Underline {
		b.WriteString(";4")
	}
	writeColor(b, s.Fg, p, false)
	writeColor(b, s.Bg, p, true)
	b.WriteByte('m')
}

// writeColor adds c as the foreground, or the background if bg, in the
// form profile p knows.
func writeColor(b *bytes.Buffer, c Color, p Profile, bg bool) {
	if !c.Set {
		return
	}
	base := 38
	if bg {
		base = 48
	}
	switch p {
	case TrueColor:
		writeInts(b, base, 2, int(c.RGB.R), int(c.RGB.G), int(c.RGB.B))
	case ANSI256:
		writeInts(b, base, 5, to256(c.RGB))
	default:
		if c.ANSI == NoANSI {
			return
		}
		n := int(c.ANSI % 16)
		code := 30 + n // 30-37, then 90-97 for the bright ones
		if n >= 8 {
			code = 90 + n - 8
		}
		if bg {
			code += 10
		}
		writeInts(b, code)
	}
}

// writeInts adds each n as ";n".
func writeInts(b *bytes.Buffer, ns ...int) {
	var digits [20]byte
	for _, n := range ns {
		b.WriteByte(';')
		b.Write(strconv.AppendInt(digits[:0], int64(n), 10))
	}
}

// glyph is one cell: a grapheme cluster and its look. A wide cluster takes
// two cells; the second has w 0 and no text.
type glyph struct {
	text string // empty is a space
	w    uint8
	s    styleID
}

// blankGlyph is an empty cell in style s.
func blankGlyph(s styleID) glyph { return glyph{w: 1, s: s} }

// grid is cells to draw, W columns by H rows. The zero value is empty.
type grid struct {
	W, H  int
	cells []glyph
	// dirty marks the rows where a cell changed since the painter last
	// looked, so a frame compares only those.
	dirty []bool
}

// resize makes b w×h, blank.
func (b *grid) resize(w, h int) {
	b.W, b.H = max(w, 0), max(h, 0)
	n := b.W * b.H
	if cap(b.cells) < n {
		b.cells = make([]glyph, n)
	}
	b.cells = b.cells[:n]
	if cap(b.dirty) < b.H {
		b.dirty = make([]bool, b.H)
	}
	b.dirty = b.dirty[:b.H]
	clear(b.cells)
	b.fill(0, 0, b.W, b.H, 0)
	for y := range b.dirty {
		b.dirty[y] = true
	}
}

// set puts g at x, y, marking the row if the cell changes.
func (b *grid) set(x, y int, g glyph) {
	if c := b.at(x, y); *c != g {
		*c = g
		b.dirty[y] = true
	}
}

func (b *grid) at(x, y int) *glyph { return &b.cells[y*b.W+x] }

func (b *grid) in(x, y int) bool { return x >= 0 && y >= 0 && x < b.W && y < b.H }

// fill blanks the w×h rectangle at x, y in style s, clipped to the buffer.
func (b *grid) fill(x, y, w, h int, s styleID) {
	x0, y0 := max(x, 0), max(y, 0)
	x1, y1 := min(x+w, b.W), min(y+h, b.H)
	if x1 <= x0 || y1 <= y0 {
		return
	}
	for row := y0; row < y1; row++ {
		b.splitWide(x0, row)
		b.splitWide(x1, row)
		for col := x0; col < x1; col++ {
			b.set(col, row, blankGlyph(s))
		}
	}
}

// splitWide blanks a wide cluster that x would cut in two: its first half
// at x-1 and its second at x.
func (b *grid) splitWide(x, y int) {
	if x <= 0 || x >= b.W {
		return
	}
	if g := b.at(x, y); g.w == 0 {
		b.set(x-1, y, blankGlyph(b.at(x-1, y).s))
		b.set(x, y, blankGlyph(g.s))
	}
}

// put writes one cluster w columns wide at x, y, if it fits whole.
func (b *grid) put(x, y int, text string, w int, s styleID) bool {
	if !b.in(x, y) || x+w > b.W || w < 1 || w > 2 {
		return false
	}
	i := y*b.W + x
	if b.cells[i].w == 0 {
		b.splitWide(x, y)
	}
	if x+w < b.W && b.cells[i+w].w == 0 {
		b.splitWide(x+w, y)
	}
	b.set(x, y, glyph{text: text, w: uint8(w), s: s})
	if w == 2 {
		b.set(x+1, y, glyph{s: s})
	}
	return true
}

// text writes s from x on row y in style st, up to column limit (exclusive),
// and returns the column after it. Control characters are dropped, since
// the text may come from the network; tabs count as one space. A wide
// cluster that doesn't fit before limit ends the text there.
func (b *grid) text(x, y, limit int, s string, st styleID) int {
	limit = min(limit, b.W)
	s = sanitize(s, false)
	for i := 0; i < len(s) && x < limit; {
		var g string
		var w int
		if r, size := utf8.DecodeRuneInString(s[i:]); simple(r) && (i+size == len(s) || simple(firstRune(s[i+size:]))) {
			g, w = s[i:i+size], runewidth.RuneWidth(r)
			i += size
		} else {
			it := graphemes.FromString(s[i:])
			it.Next()
			g = it.Value()
			i += len(g)
			g = safeCluster(g)
			w = clusterWidth(g)
		}
		if g == "" || w == 0 {
			continue
		}
		if x+w > limit {
			break
		}
		if g == " " {
			g = ""
		}
		b.put(x, y, g, w, st)
		x += w
	}
	return x
}

// safeCluster is g without what mustn't reach a terminal: control
// characters (a tab becomes a space) and invalid UTF-8.
func safeCluster(g string) string {
	if g == "\t" {
		return " "
	}
	ok := true
	for _, r := range g {
		if r == '�' || !printable(r) {
			ok = false
			break
		}
	}
	if ok {
		return g
	}
	return strings.Map(func(r rune) rune {
		if r == '�' || !printable(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(g, ""))
}

// clusterWidth is how many columns a grapheme cluster takes: 0, 1 or 2.
func clusterWidth(g string) int { return min(runewidth.StringWidth(g), 2) }

// textWidth is how many columns s takes, cluster by cluster.
func textWidth(s string) int {
	w := 0
	it := graphemes.FromString(sanitize(s, false))
	for it.Next() {
		w += clusterWidth(safeCluster(it.Value()))
	}
	return w
}

// sanitize is s as it may reach a terminal: valid UTF-8, tabs as spaces,
// without control characters but, if keepNewlines, newlines. Cleaning the
// whole text before splitting it keeps an invalid byte from joining the
// cluster next to it.
func sanitize(s string, keepNewlines bool) string {
	clean := true
	for _, r := range s {
		if r == '\uFFFD' || r == '\t' || !printable(r) && !(keepNewlines && r == '\n') {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t':
			return ' '
		case r == '\n' && keepNewlines:
			return r
		case r == '\uFFFD' || !printable(r):
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
}

// simple reports whether r is a printable rune below the combining marks:
// next to another such rune it is a grapheme cluster of its own, so text,
// in Latin scripts mostly, needs no cluster splitting.
func simple(r rune) bool {
	return r >= 0x20 && r < 0x300 && r != 0x7f && (r < 0x80 || r >= 0xa0) && r != 0xad
}

func firstRune(s string) rune {
	r, _ := utf8.DecodeRuneInString(s)
	return r
}
