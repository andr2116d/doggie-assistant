package console

import (
	"strings"

	"github.com/clipperhouse/uax29/v2/graphemes"
)

// wrapLines breaks text into lines at most width columns wide (2 at the
// least, for a wide cluster): at its newlines, and between words where a
// line would overflow. A word longer than a line is broken where it
// overflows. The spaces at a break are dropped; those starting a paragraph
// are kept, as indentation. Whatever mustn't reach the terminal is dropped
// first.
func wrapLines(text string, width int) []string {
	var lines []string
	for para := range strings.SplitSeq(sanitize(text, true), "\n") {
		lines, _ = wrapParagraph(lines, para, max(width, 2), false)
	}
	return lines
}

// piece is a grapheme cluster, its width and where it starts in the
// paragraph, in bytes.
type piece struct {
	s     string
	w, at int
}

// wrapParagraph appends the lines of para, which must be clean and have no
// newline, and returns where its last line starts in para. Lines before the
// last never change as text is added to the paragraph, so wrapping it again
// may start there; wrapped then says that line began at a break.
func wrapParagraph(lines []string, para string, width int, wrapped bool) ([]string, int) {
	var line []piece
	lineW := 0
	emit := func(ps []piece) {
		var b strings.Builder
		for _, p := range ps {
			b.WriteString(p.s)
		}
		lines = append(lines, strings.TrimRight(b.String(), " "))
	}
	lastStart := 0
	it := graphemes.FromString(para)
	for it.Next() {
		c, at := safeCluster(it.Value()), it.Start()
		w := clusterWidth(c)
		switch {
		case c == "":
			continue
		case w == 0: // a mark the cleaning left without its base
			if len(line) > 0 {
				line[len(line)-1].s += c
			}
			continue
		case c == " " && wrapped && len(line) == 0:
			continue // a wrapped line doesn't start with a space
		}
		if lineW+w > width {
			if c == " " {
				emit(line)
				line, lineW, wrapped = nil, 0, true
				lastStart = it.End()
				continue
			}
			line = breakLine(line, emit)
			lineW, wrapped = widthOfPieces(line), true
			if lineW+w > width { // what was carried over and c don't fit
				emit(line)
				line, lineW = nil, 0
			}
			lastStart = at
			if len(line) > 0 {
				lastStart = line[0].at
			}
		}
		line = append(line, piece{c, w, at})
		lineW += w
	}
	emit(line)
	return lines, lastStart
}

// breakLine emits line up to its last space, past any indentation, and
// returns the word after it, to go on the next line. Without such a space
// it emits the whole line.
func breakLine(line []piece, emit func([]piece)) []piece {
	cut := -1
	for i := len(line) - 1; i > 0; i-- {
		if line[i].s == " " && !onlySpaces(line[:i]) {
			cut = i
			break
		}
	}
	if cut < 0 {
		emit(line)
		return nil
	}
	emit(line[:cut])
	rest := line[cut+1:]
	for len(rest) > 0 && rest[0].s == " " {
		rest = rest[1:]
	}
	return append([]piece(nil), rest...)
}

func onlySpaces(ps []piece) bool {
	for _, p := range ps {
		if p.s != " " {
			return false
		}
	}
	return true
}

func widthOfPieces(ps []piece) int {
	w := 0
	for _, p := range ps {
		w += p.w
	}
	return w
}
