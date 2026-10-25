package console

import (
	"bytes"
	"fmt"
)

// minCols is the narrowest terminal drawn as such; narrower ones are drawn
// as if they had minCols.
const minCols = 8

// screen draws the console on a terminal in raw mode. Below what was printed
// it keeps a live region it redraws on every change: the output line still
// being written, if any, and the input line. Neither ever wraps, so the
// region is at most two rows and can always be found again: a partial output
// line is broken into rows as it overflows them, and the input line scrolls
// sideways.
type screen struct {
	cols    int
	prompt  []rune
	partial []rune // output line being written, at most cols wide
	scroll  int    // first rune of the input line shown

	drawn       bool // the live region is on screen
	partialRows int  // rows of the live region above the input line
}

// clear erases the live region, leaving the cursor where it started.
func (s *screen) clear(b *bytes.Buffer) {
	if !s.drawn {
		return
	}
	b.WriteString("\r")
	if s.partialRows > 0 {
		fmt.Fprintf(b, "\x1b[%dA", s.partialRows)
	}
	b.WriteString("\x1b[J")
	s.drawn = false
}

// printAbove writes text as finished rows above the partial line, which
// stays as it is: what the user submits must not split a reply being
// written.
func (s *screen) printAbove(b *bytes.Buffer, text string) {
	saved := s.partial
	s.partial = nil
	s.print(b, text)
	if len(s.partial) > 0 {
		s.commit(b)
	}
	s.partial = saved
}

// print adds output. Complete rows go above the live region for good; the
// rest stays as the partial line.
func (s *screen) print(b *bytes.Buffer, text string) {
	for _, r := range clean(text) {
		if r == '\n' {
			s.commit(b)
			continue
		}
		w := width(r)
		if w == 0 && len(s.partial) == 0 {
			continue // a combining mark with nothing to combine with
		}
		if widthOf(s.partial)+w > s.cols {
			s.commit(b)
		}
		s.partial = append(s.partial, r)
	}
}

// commit writes the partial line as a finished row.
func (s *screen) commit(b *bytes.Buffer) {
	b.WriteString(string(s.partial))
	b.WriteString("\r\n")
	s.partial = s.partial[:0]
}

// draw writes the live region for e. The cursor ends on the input line, at
// e's cursor.
func (s *screen) draw(b *bytes.Buffer, e *editor) {
	s.partialRows = 0
	if len(s.partial) > 0 {
		b.WriteString(string(s.partial))
		b.WriteString("\r\n")
		s.partialRows = 1
	}
	prompt := s.prompt
	// Room for the prompt, at least one rune and the cursor after it.
	for len(prompt) > 0 && widthOf(prompt) > s.cols-3 {
		prompt = prompt[:len(prompt)-1]
	}
	room := s.cols - 1 - widthOf(prompt) // the last column stays free
	cursor := s.fit(e, room)
	shown := fitting(e.line[s.scroll:], room)
	b.WriteString(string(prompt))
	b.WriteString(string(shown))
	b.WriteString("\r")
	if col := widthOf(prompt) + cursor; col > 0 {
		fmt.Fprintf(b, "\x1b[%dC", col)
	}
	s.drawn = true
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
	if widthOf(s.partial) <= s.cols {
		return
	}
	p := s.partial
	s.partial = nil
	s.print(b, string(p))
}
