package console

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"time"
)

// minCols is the narrowest terminal drawn as such; narrower ones are drawn
// as if they had minCols.
const minCols = 8

// Text is a piece of output and how to show it.
type Text struct {
	Text string
	Role Role
	// Indent is how many columns each row the text begins is shifted by, at
	// most half the terminal's width. Text after a label on the same row,
	// e.g. a name, hangs under the label's end with an Indent of its width.
	Indent int
	// Wrap breaks rows between words, rather than at the last column that
	// fits. A word longer than a row is still broken.
	Wrap bool
	// Gap leaves an empty row before the text, unless the output is at its
	// start or one is there already. The text starts on a row of its own.
	Gap bool
	// Paced shows the text at a steady pace rather than all at once, so text
	// arriving in bursts, like a reply being written, flows on screen.
	Paced bool
}

// cell is one rune on screen and the role it is drawn with.
type cell struct {
	r    rune
	role Role
}

// screen draws the console on a terminal in raw mode. Below what was printed
// it keeps a live region it redraws on every change: the output line still
// being written, if any, the activity line, if any, the status line, if any,
// and the input line. None ever wraps, so the region is at most four rows
// and can always be found again: a partial output line is broken into rows
// as it overflows them, the activity and status lines are cut, and the input
// line scrolls sideways.
type screen struct {
	cols       int
	prompt     []rune
	promptRole Role
	partial    []cell // output line being written, at most cols wide
	// padded is how many cells at the start of partial are indentation;
	// soft is set when partial began because the row before wrapped.
	padded int
	soft   bool
	status []rune // shown between the output and the input line, if set
	scroll int    // first rune of the input line shown

	// activity is shown with a spinner while set; elapsed is for how long,
	// and phase moves the animation.
	activity []rune
	elapsed  time.Duration
	phase    int

	profile Profile
	theme   *Theme
	sgrs    [numRoles]string // each role's sequence, once styled

	// printed is set once a row was printed; blank when the last one was
	// empty.
	printed, blank bool

	drawn     bool // the live region is on screen
	rowsAbove int  // rows of the live region above the input line
}

// style sets how roles are drawn.
func (s *screen) style(theme *Theme, p Profile) {
	if theme == nil {
		theme = &DefaultTheme
	}
	s.theme, s.profile = theme, p
	for r := range numRoles {
		s.sgrs[r] = theme[r].sgr(p)
	}
}

// clear erases the live region, leaving the cursor where it started.
func (s *screen) clear(b *bytes.Buffer) {
	if !s.drawn {
		return
	}
	b.WriteString("\r")
	if s.rowsAbove > 0 {
		fmt.Fprintf(b, "\x1b[%dA", s.rowsAbove)
	}
	b.WriteString("\x1b[J")
	s.drawn = false
}

// printAbove writes texts as finished rows above the partial line, which
// stays as it is: what the user submits must not split a reply being
// written.
func (s *screen) printAbove(b *bytes.Buffer, texts ...Text) {
	saved, savedPadded, savedSoft := s.partial, s.padded, s.soft
	s.partial, s.padded, s.soft = nil, 0, false
	for _, t := range texts {
		s.printText(b, t)
	}
	if len(s.partial) > 0 {
		s.commit(b)
	}
	s.partial, s.padded, s.soft = saved, savedPadded, savedSoft
}

// print adds plain output.
func (s *screen) print(b *bytes.Buffer, text string) { s.printText(b, Text{Text: text}) }

// printText adds output. Complete rows go above the live region for good;
// the rest stays as the partial line.
func (s *screen) printText(b *bytes.Buffer, t Text) {
	if t.Gap {
		s.gap(b)
	}
	indent := min(max(t.Indent, 0), s.cols/2)
	for _, r := range clean(t.Text) {
		if r == '\n' {
			s.commit(b)
			continue
		}
		w := width(r)
		if len(s.partial) == 0 {
			if w == 0 {
				continue // a combining mark with nothing to combine with
			}
			s.pad(indent)
		}
		if r == ' ' && s.soft && len(s.partial) == s.padded {
			continue // a wrapped row doesn't start with the space it broke at
		}
		if cellsWidth(s.partial)+w > s.cols {
			if t.Wrap && r == ' ' {
				s.wrapAt(b, len(s.partial), indent, true)
				continue
			}
			s.overflow(b, t.Wrap, indent, w)
		}
		s.partial = append(s.partial, cell{r, t.Role})
	}
}

// pad starts the partial line with indent columns.
func (s *screen) pad(indent int) {
	for range indent {
		s.partial = append(s.partial, cell{' ', Normal})
	}
	s.padded = indent
}

// overflow makes room for a rune w columns wide that doesn't fit on the
// partial line: it moves the last word to the next row when wrap is set and
// there is a word to move, and else just starts the next row.
func (s *screen) overflow(b *bytes.Buffer, wrap bool, indent, w int) {
	if wrap {
		for i := len(s.partial) - 1; i > s.padded; i-- {
			if s.partial[i].r != ' ' {
				continue
			}
			// Only if the word fits after the indentation with the rune.
			if indent+cellsWidth(s.partial[i+1:])+w <= s.cols {
				s.wrapAt(b, i, indent, true)
				return
			}
			break
		}
	}
	s.wrapAt(b, len(s.partial), indent, wrap)
}

// wrapAt ends the row at cell i and starts the next one indented with what
// came after. Between words, the spaces around the break are dropped.
func (s *screen) wrapAt(b *bytes.Buffer, i, indent int, words bool) {
	rest := slices.Clone(s.partial[i:])
	head := s.partial[:i]
	if words {
		for len(rest) > 0 && rest[0].r == ' ' {
			rest = rest[1:]
		}
		for len(head) > s.padded && head[len(head)-1].r == ' ' {
			head = head[:len(head)-1]
		}
	}
	s.partial = head
	s.commit(b)
	s.soft = words
	if len(rest) > 0 || indent > 0 {
		s.pad(indent)
		s.partial = append(s.partial, rest...)
	}
}

// commit writes the partial line as a finished row.
func (s *screen) commit(b *bytes.Buffer) {
	s.writeCells(b, s.partial)
	b.WriteString("\r\n")
	s.printed = true
	s.blank = !slices.ContainsFunc(s.partial, func(c cell) bool { return c.r != ' ' })
	s.partial = s.partial[:0]
	s.padded, s.soft = 0, false
}

// gap ends the partial line, if any, and leaves an empty row after what was
// printed, if there is no empty row already.
func (s *screen) gap(b *bytes.Buffer) {
	if len(s.partial) > 0 {
		s.commit(b)
	}
	if s.printed && !s.blank {
		b.WriteString("\r\n")
		s.blank = true
	}
}

// writeCells writes cells with their roles' looks, ending in the terminal's
// own.
func (s *screen) writeCells(b *bytes.Buffer, cells []cell) {
	current := Normal
	for _, c := range cells {
		if c.role != current && s.sgrs[c.role] != s.sgrs[current] {
			if s.sgrs[current] != "" {
				b.WriteString(reset)
			}
			b.WriteString(s.sgrs[c.role])
		}
		current = c.role
		b.WriteRune(c.r)
	}
	if s.sgrs[current] != "" {
		b.WriteString(reset)
	}
}

// writeRole writes rs with role's look.
func (s *screen) writeRole(b *bytes.Buffer, role Role, rs []rune) {
	if len(rs) == 0 {
		return
	}
	b.WriteString(s.sgrs[role])
	b.WriteString(string(rs))
	if s.sgrs[role] != "" {
		b.WriteString(reset)
	}
}

// draw writes the live region for e. The cursor ends on the input line, at
// e's cursor.
func (s *screen) draw(b *bytes.Buffer, e *editor) {
	s.rowsAbove = 0
	if len(s.partial) > 0 {
		s.writeCells(b, s.partial)
		b.WriteString("\r\n")
		s.rowsAbove++
	}
	if len(s.activity) > 0 {
		s.drawActivity(b)
		b.WriteString("\r\n")
		s.rowsAbove++
	}
	if len(s.status) > 0 {
		s.writeRole(b, Muted, cut(s.status, s.cols))
		b.WriteString("\r\n")
		s.rowsAbove++
	}
	prompt := s.prompt
	if e.hidden {
		prompt = e.hiddenPrompt
	}
	// Room for the prompt, at least one rune and the cursor after it.
	for len(prompt) > 0 && widthOf(prompt) > s.cols-3 {
		prompt = prompt[:len(prompt)-1]
	}
	room := s.cols - 1 - widthOf(prompt) // the last column stays free
	cursor := s.fit(e, room)
	shown := fitting(e.line[s.scroll:], room)
	if e.hidden {
		cursor, shown = 0, nil
	}
	s.writeRole(b, s.promptRole, prompt)
	b.WriteString(string(shown))
	b.WriteString("\r")
	if col := widthOf(prompt) + cursor; col > 0 {
		fmt.Fprintf(b, "\x1b[%dC", col)
	}
	s.drawn = true
}

// spinner are the activity indicator's frames.
var spinner = []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")

// shownAfter is how long an activity goes before it shows its time.
const shownAfter = 3 * time.Second

// drawActivity writes the activity line: a spinner, the label with a light
// sweeping over it, and its time once it goes long.
func (s *screen) drawActivity(b *bytes.Buffer) {
	var suffix []rune
	if s.elapsed >= shownAfter {
		suffix = []rune(" · " + since(s.elapsed))
	}
	room := s.cols - 2 - widthOf(suffix)
	if room < 1 {
		suffix, room = nil, s.cols-2
	}
	label := cut(s.activity, room)
	s.writeRole(b, Accent, []rune{spinner[s.phase%len(spinner)], ' '})
	s.shimmer(b, label)
	s.writeRole(b, Muted, suffix)
}

// shimmerWidth is how many runes the sweeping light covers on each side.
const shimmerWidth = 3

// shimmer writes label in the muted look, lit toward the accent's color
// around a point that sweeps across it. Without enough colors it is just
// muted.
func (s *screen) shimmer(b *bytes.Buffer, label []rune) {
	base, peak := s.theme[Muted], s.theme[Accent]
	if s.profile < ANSI256 || !base.colored() || !peak.colored() {
		s.writeRole(b, Muted, label)
		return
	}
	span := len(label) + 2*shimmerWidth
	center := s.phase%span - shimmerWidth
	for i, r := range label {
		lit := 1 - float64(abs(i-center))/float64(shimmerWidth)
		c := blend(base.Color, peak.Color, max(lit, 0))
		fmt.Fprintf(b, "\x1b[%sm%c", colorCode(c, base.ANSI, s.profile), r)
	}
	b.WriteString(reset)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// since says d in whole seconds, and minutes past one: "12s", "2m 05s".
func since(d time.Duration) string {
	sec := int(d / time.Second)
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	return fmt.Sprintf("%dm %02ds", sec/60, sec%60)
}

// fit scrolls the input line so the cursor is shown within room columns,
// and returns the cursor's column after the prompt. It only measures the
// runes between the scroll offset and the cursor, which fit on screen.
func (s *screen) fit(e *editor, room int) int {
	if e.pos < s.scroll || s.scroll > len(e.line) {
		s.scroll = e.pos
	}
	// Far behind the cursor: restart from what can fit before it.
	if e.pos-s.scroll > room {
		s.scroll = e.pos - room
	}
	w := widthOf(e.line[s.scroll:e.pos])
	for s.scroll < e.pos && w >= room {
		w -= width(e.line[s.scroll])
		s.scroll++
	}
	// Show as much as fits before the cursor, e.g. after deleting.
	for s.scroll > 0 && w+width(e.line[s.scroll-1]) < room {
		s.scroll--
		w += width(e.line[s.scroll])
	}
	return w
}

// fitting is the longest start of rs that fits in room columns.
func fitting(rs []rune, room int) []rune {
	w := 0
	for i, r := range rs {
		if w += width(r); w > room {
			return rs[:i]
		}
	}
	return rs
}

// cut is rs cut to room columns, ending in "…" when cut.
func cut(rs []rune, room int) []rune {
	if widthOf(rs) <= room {
		return rs
	}
	return append(slices.Clone(fitting(rs, room-1)), '…')
}

// finish leaves the terminal below everything printed: the partial line is
// ended and the input line erased.
func (s *screen) finish(b *bytes.Buffer) {
	s.clear(b)
	if len(s.partial) > 0 {
		s.commit(b)
	}
}

// refit breaks the partial line again after the terminal narrowed, so it
// still fits.
func (s *screen) refit(b *bytes.Buffer) {
	if cellsWidth(s.partial) <= s.cols {
		return
	}
	p := s.partial
	s.partial, s.padded, s.soft = nil, 0, false
	for _, c := range p {
		if cellsWidth(s.partial)+width(c.r) > s.cols {
			s.commit(b)
		}
		s.partial = append(s.partial, c)
	}
}

// setStatus sets the status line, or removes it if text is empty.
func (s *screen) setStatus(text string) {
	s.status = []rune(strings.TrimSpace(oneLine(text)))
}

// statusRow is the status line cut to fit the terminal.
func (s *screen) statusRow() []rune { return cut(s.status, s.cols) }

// setActivity sets the activity line, or removes it if text is empty.
func (s *screen) setActivity(text string) {
	s.activity = []rune(strings.TrimSpace(oneLine(text)))
}

func cellsWidth(cells []cell) int {
	w := 0
	for _, c := range cells {
		w += width(c.r)
	}
	return w
}

// Synchronized output: a terminal that knows it shows what comes between
// these at once; others ignore them.
const (
	syncBegin = "\x1b[?2026h"
	syncEnd   = "\x1b[?2026l"
)
