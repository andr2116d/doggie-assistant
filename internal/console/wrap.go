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
	var rows []textRow
	for para := range strings.SplitSeq(sanitize(text, true), "\n") {
		rows, _ = wrapRows(rows, para, nil, max(width, 2), false, 0)
	}
	lines := make([]string, len(rows))
	for i, r := range rows {
		lines[i] = r.text
	}
	return lines
}

// textRow is a row of wrapped text, with the markdown attribute of each of
// its bytes; nil attributes are none.
type textRow struct {
	text  string
	attrs []mdAttr
}

// piece is a grapheme cluster, its width and where it starts in the
// paragraph, in bytes.
type piece struct {
	s     string
	w, at int
}

// wrapRows appends the rows of para, which must be clean and have no
// newline, with attrs giving each of its bytes an attribute (nil for none).
// Rows after the first are indented by hang. It returns where its last row
// starts in para: rows before the last never change as text is added to
// the paragraph, so wrapping it again may start there, with wrapped set to
// tell that row began at a break.
func wrapRows(rows []textRow, para string, attrs []mdAttr, width int, wrapped bool, hang int) ([]textRow, int) {
	hang = min(hang, width/2)
	var line []piece
	lineW := 0
	limit := func() int {
		if wrapped {
			return max(width-hang, 2)
		}
		return width
	}
	emit := func(ps []piece) {
		for len(ps) > 0 && ps[len(ps)-1].s == " " {
			ps = ps[:len(ps)-1]
		}
		var b strings.Builder
		var as []mdAttr
		if wrapped && hang > 0 {
			b.WriteString(strings.Repeat(" ", hang))
			if attrs != nil {
				as = make([]mdAttr, hang)
			}
		}
		for _, p := range ps {
			b.WriteString(p.s)
			if attrs != nil {
				for i := range len(p.s) {
					as = append(as, attrs[min(p.at+i, len(attrs)-1)])
				}
			}
		}
		rows = append(rows, textRow{b.String(), as})
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
		if lineW+w > limit() {
			if c == " " {
				emit(line)
				line, lineW, wrapped = nil, 0, true
				lastStart = it.End()
				continue
			}
			var rest []piece
			line, rest = breakLine(line)
			emit(line)
			line, wrapped = rest, true
			lineW = widthOfPieces(line)
			if lineW+w > limit() { // what was carried over and c don't fit
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
	return rows, lastStart
}

// breakLine splits line at its last space, past any indentation, into what
// stays on it and the word after, to go on the next line. Without such a
// space all of it stays.
func breakLine(line []piece) (stay, next []piece) {
	cut := -1
	for i := len(line) - 1; i > 0; i-- {
		if line[i].s == " " && !onlySpaces(line[:i]) {
			cut = i
			break
		}
	}
	if cut < 0 {
		return line, nil
	}
	rest := line[cut+1:]
	for len(rest) > 0 && rest[0].s == " " {
		rest = rest[1:]
	}
	return line[:cut], append([]piece(nil), rest...)
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
