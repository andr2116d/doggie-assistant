package console

import "strings"

// rect is a part of the screen: w columns by h rows from x, y.
type rect struct{ x, y, w, h int }

func (r rect) empty() bool { return r.w <= 0 || r.h <= 0 }

// The full screen's measures, in columns and rows.
const (
	panelMinCols  = 100 // narrower screens hide the panel
	panelWideCols = 150 // wider ones give it more room
	ruleMinRows   = 20  // shorter screens leave out the rule below the bar
	maxInputRows  = 6   // the input box grows up to these lines, then scrolls
	fullMinCols   = 40  // smaller screens only ask to be made bigger
	fullMinRows   = 10
	conversationX = 2 // margin of the conversation on each side
)

// regions are where each part of the full screen goes. A region that
// doesn't show is empty.
type regions struct {
	header, rule, body, panel, input, footer rect
	// tooSmall is set when the screen can't hold the chat at all.
	tooSmall bool
}

// layout places the parts on a w×h screen: a bar on top, with a rule below
// it when the screen is tall enough; the input box with inputRows lines (1
// to maxInputRows) and a row of hints at the bottom; and between them the
// conversation, with the panel on its right when withPanel is set and the
// screen is wide enough, wider on a wide screen.
func layout(w, h int, withPanel bool, inputRows int) regions {
	if w < fullMinCols || h < fullMinRows {
		return regions{tooSmall: true}
	}
	inputRows = min(max(inputRows, 1), maxInputRows, h-fullMinRows+1)
	var r regions
	r.header = rect{0, 0, w, 1}
	top := 1
	if h >= ruleMinRows {
		r.rule = rect{0, 1, w, 1}
		top = 2
	}
	r.footer = rect{0, h - 1, w, 1}
	r.input = rect{0, h - 1 - (inputRows + 2), w, inputRows + 2}
	body := rect{0, top, w, r.input.y - top}
	if cols := panelWidth(w); withPanel && cols > 0 {
		r.panel = rect{w - cols, body.y, cols, body.h}
		body.w -= cols
	}
	r.body = body
	return r
}

// textIn writes s from x on row y of g, up to column limit, cut with "…"
// when it doesn't fit, and returns the column after it.
func textIn(g *grid, x, y, limit int, s string, st styleID) int {
	if textWidth(s) <= limit-x {
		return g.text(x, y, limit, s, st)
	}
	if limit-x < 1 {
		return x
	}
	x = g.text(x, y, limit-1, s, st)
	return g.text(x, y, limit, "…", st)
}

// textRight writes s ending just before column end, cut if it doesn't fit
// after column start, and returns the column it begins at.
func textRight(g *grid, start, end, y int, s string, st styleID) int {
	w := min(textWidth(s), end-start)
	x := end - w
	textIn(g, x, y, end, s, st)
	return x
}

// border draws a rounded frame on the edge of r.
func border(g *grid, r rect, st styleID) {
	if r.w < 2 || r.h < 2 {
		return
	}
	right, bottom := r.x+r.w-1, r.y+r.h-1
	g.put(r.x, r.y, "╭", 1, st)
	g.put(right, r.y, "╮", 1, st)
	g.put(r.x, bottom, "╰", 1, st)
	g.put(right, bottom, "╯", 1, st)
	for x := r.x + 1; x < right; x++ {
		g.put(x, r.y, "─", 1, st)
		g.put(x, bottom, "─", 1, st)
	}
	for y := r.y + 1; y < bottom; y++ {
		g.put(r.x, y, "│", 1, st)
		g.put(right, y, "│", 1, st)
	}
}

// centered is the column where text w columns wide starts centered in r.
func centered(r rect, w int) int { return r.x + max((r.w-w)/2, 0) }

// tooSmallNote is shown alone on a screen too small for the chat.
func tooSmallNote(g *grid, st styleID, note string) {
	g.fill(0, 0, g.W, g.H, st)
	lines := strings.Split(note, "\n")
	y := max((g.H-len(lines))/2, 0)
	for i, l := range lines {
		if y+i >= g.H {
			break
		}
		textIn(g, centered(rect{0, 0, g.W, g.H}, textWidth(l)), y+i, g.W, l, st)
	}
}

// hline draws a rule w cells wide from x on row y, between the ends left
// and right.
func hline(g *grid, x, y, w int, left, right string, st styleID) {
	if w < 2 {
		return
	}
	g.put(x, y, left, 1, st)
	for i := 1; i < w-1; i++ {
		g.put(x+i, y, "─", 1, st)
	}
	g.put(x+w-1, y, right, 1, st)
}

// panelWidth is how wide the panel is on a screen w columns wide: none
// under panelMinCols, about a quarter of it otherwise, and more past
// panelWideCols.
func panelWidth(w int) int {
	switch {
	case w < panelMinCols:
		return 0
	case w < panelWideCols:
		return 32
	}
	return min(w/4, 44)
}
