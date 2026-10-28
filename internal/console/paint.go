package console

import (
	"bytes"
	"strconv"
	"time"
)

// Full-screen modes: the alternate screen, so leaving puts the terminal back
// as it was; a hidden cursor; bracketed paste; focus reports, so animations
// can rest while the window is in the background; and the mouse, reported
// as SGR, its moves only with a button down.
const (
	enterFull = "\x1b[?1049h\x1b[?25l\x1b[?2004h\x1b[?1004h\x1b[?1002h\x1b[?1006h"
	leaveFull = "\x1b[?1006l\x1b[?1002l\x1b[?1004l\x1b[?2004l\x1b[0m\x1b[?25h\x1b[?1049l"
)

// maxGap is how many unchanged cells a frame writes again rather than move
// the cursor over them: a move takes about as many bytes.
const maxGap = 4

// painter draws grids on a terminal, writing only the cells that changed
// since the frame before.
type painter struct {
	styles  *styleTable
	profile Profile

	front grid // what the terminal shows
	valid bool // front is on the terminal

	out    bytes.Buffer // the cells of the frame being made
	buf    []byte       // the frame, reused
	cx, cy int          // the terminal's cursor; cx -1 when unknown
	cur    styleID
	shown  bool // the cursor is visible
}

func newPainter(styles *styleTable, p Profile) *painter {
	return &painter{styles: styles, profile: p, cx: -1}
}

// invalidate makes the next frame draw everything, e.g. after a resize.
func (p *painter) invalidate() { p.valid = false }

// frame returns what turns the terminal from the last frame into g, with
// the cursor shown at x, y if show. It returns nil when nothing changes.
// The bytes are valid until the next call.
func (p *painter) frame(g *grid, x, y int, show bool) []byte {
	p.out.Reset()
	if !p.valid || p.front.W != g.W || p.front.H != g.H {
		p.front.resize(g.W, g.H)
		p.out.WriteString("\x1b[0m\x1b[2J") // blank, in the terminal's own look
		p.cur, p.cx, p.valid = 0, -1, true
	}
	cells := p.out.Len() > 0
	all := cells // the terminal was cleared: every row is new
	for row := range g.H {
		if all || g.dirty[row] {
			cells = p.row(g, row) || cells
			g.dirty[row] = false
		}
	}
	show = show && g.in(x, y)
	if !cells && show == p.shown && (!show || (p.cx == x && p.cy == y)) {
		return nil
	}
	b := append(p.buf[:0], syncBegin...)
	if cells && p.shown {
		b = append(b, "\x1b[?25l"...) // no cursor jumping around while drawing
	}
	b = append(b, p.out.Bytes()...)
	p.out.Reset()
	switch {
	case show:
		p.move(x, y)
		b = append(b, p.out.Bytes()...)
		if cells || !p.shown {
			b = append(b, "\x1b[?25h"...)
		}
	case p.shown && !cells:
		b = append(b, "\x1b[?25l"...)
	}
	p.shown = show
	p.buf = append(b, syncEnd...)
	return p.buf
}

// row writes the cells of row y that changed, and reports whether any did.
func (p *painter) row(g *grid, y int) bool {
	w := g.W
	now, old := g.cells[y*w:(y+1)*w], p.front.cells[y*w:(y+1)*w]
	changed := false
	for x := 0; x < w; {
		if now[x] == old[x] {
			x++
			continue
		}
		changed = true
		if now[x].w == 0 && x > 0 {
			x-- // the second half of a wide cluster: write it from its start
		}
		p.move(x, y)
		for x < w {
			if now[x] == old[x] {
				// Stop, unless another change comes within maxGap cells.
				k := x
				for k < w && k-x < maxGap && now[k] == old[k] {
					k++
				}
				if k == w || k-x == maxGap {
					break
				}
			}
			x = p.put(now, old, x, w)
		}
	}
	return changed
}

// put writes cell x of a row and records it as shown, and returns the
// column after it.
func (p *painter) put(now, old []glyph, x, w int) int {
	g := now[x]
	if g.w == 0 { // an orphan second half: never made by grid, drawn as a space
		g = blankGlyph(g.s)
	}
	if g.s != p.cur {
		p.styles.sgr(&p.out, g.s, p.profile)
		p.cur = g.s
	}
	if g.text == "" {
		p.out.WriteByte(' ')
	} else {
		p.out.WriteString(g.text)
	}
	n := int(g.w)
	copy(old[x:x+n], now[x:x+n])
	p.cx += n
	if p.cx >= w {
		p.cx = -1 // at the last column the cursor waits to wrap: unknown
	}
	return x + n
}

// move puts the cursor at column x, row y.
func (p *painter) move(x, y int) {
	if p.cx == x && p.cy == y {
		return
	}
	var digits [20]byte
	p.out.WriteString("\x1b[")
	p.out.Write(strconv.AppendInt(digits[:0], int64(y+1), 10))
	p.out.WriteByte(';')
	p.out.Write(strconv.AppendInt(digits[:0], int64(x+1), 10))
	p.out.WriteByte('H')
	p.cx, p.cy = x, y
}

// frameGate spaces frames at least frameEvery apart: a change asks for a
// frame, and the changes before it is drawn share it. Its channel is nil
// while no frame is due, so it costs nothing at rest.
type frameGate struct {
	t    *time.Timer
	c    <-chan time.Time
	last time.Time
}

// want asks for a frame, unless one is due already.
func (f *frameGate) want(now time.Time) {
	if f.c != nil {
		return
	}
	wait := max(f.last.Add(frameEvery).Sub(now), 0)
	if f.t == nil {
		f.t = time.NewTimer(wait)
	} else {
		f.t.Reset(wait)
	}
	f.c = f.t.C
}

// drawn records a frame drawn at now.
func (f *frameGate) drawn(now time.Time) {
	f.c, f.last = nil, now
}

func (f *frameGate) stop() {
	if f.t != nil {
		f.t.Stop()
	}
	f.c = nil
}
