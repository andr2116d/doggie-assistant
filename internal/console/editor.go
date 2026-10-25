package console

import (
	"slices"
	"unicode"
)

// action is what a key asks of the console, beyond editing the line.
type action int

const (
	actNone action = iota
	actSubmit
	actQuit
	actRedraw
	actRefused // the key was ignored, e.g. the line is full
)

// maxHistory bounds the lines kept for Up and Down.
const maxHistory = 500

// editor is the line being typed, with the lines submitted before.
type editor struct {
	line     []rune
	pos      int // cursor, in runes
	maxRunes int // zero means no bound

	history []string
	// browsing is the history entry shown, len(history) for the new line,
	// which draft keeps meanwhile.
	browsing int
	draft    []rune
}

func newEditor(maxRunes int) *editor {
	return &editor{maxRunes: maxRunes}
}

// apply edits the line with k. On actSubmit, line is what was submitted.
func (e *editor) apply(k key) (act action, line string) {
	switch k.kind {
	case keyRune:
		return e.insert([]rune{k.r}), ""
	case keyPaste:
		return e.insert([]rune(k.text)), ""
	case keyEnter:
		line = string(e.line)
		e.remember(line)
		e.set(nil)
		return actSubmit, line
	case keyBackspace:
		if e.pos > 0 {
			e.line = slices.Delete(e.line, e.pos-1, e.pos)
			e.pos--
		}
	case keyDelete:
		if e.pos < len(e.line) {
			e.line = slices.Delete(e.line, e.pos, e.pos+1)
		}
	case keyLeft:
		e.pos = max(e.pos-1, 0)
	case keyRight:
		e.pos = min(e.pos+1, len(e.line))
	case keyWordLeft:
		e.pos = e.wordStart()
	case keyWordRight:
		e.pos = e.wordEnd()
	case keyHome:
		e.pos = 0
	case keyEnd:
		e.pos = len(e.line)
	case keyKillStart:
		e.line = slices.Delete(e.line, 0, e.pos)
		e.pos = 0
	case keyKillEnd:
		e.line = e.line[:e.pos]
	case keyKillWord:
		start := e.wordStart()
		e.line = slices.Delete(e.line, start, e.pos)
		e.pos = start
	case keyUp:
		e.browse(-1)
	case keyDown:
		e.browse(+1)
	case keyInterrupt:
		// Ctrl+C drops what was typed; on an empty line it quits.
		if len(e.line) == 0 {
			return actQuit, ""
		}
		e.set(nil)
		e.browsing = len(e.history)
	case keyEOF:
		// Ctrl+D quits on an empty line, and deletes forward otherwise.
		if len(e.line) == 0 {
			return actQuit, ""
		}
		return e.apply(key{kind: keyDelete})
	case keyRedraw:
		return actRedraw, ""
	}
	return actNone, ""
}

// insert puts rs at the cursor, as much as fits. Control characters are
// dropped: the line is drawn as is.
func (e *editor) insert(rs []rune) action {
	rs = slices.DeleteFunc(slices.Clone(rs), func(r rune) bool { return !printable(r) })
	if len(rs) == 0 {
		return actNone
	}
	act := actNone
	if e.maxRunes > 0 {
		room := e.maxRunes - len(e.line)
		if room <= 0 {
			return actRefused
		}
		if len(rs) > room {
			rs = rs[:room]
			act = actRefused
		}
	}
	e.line = slices.Insert(e.line, e.pos, rs...)
	e.pos += len(rs)
	return act
}

func (e *editor) set(line []rune) {
	e.line = line
	e.pos = len(line)
}

func (e *editor) remember(line string) {
	e.browsing = len(e.history)
	e.draft = nil
	if isBlank(line) || (len(e.history) > 0 && e.history[len(e.history)-1] == line) {
		return
	}
	if len(e.history) == maxHistory {
		e.history = slices.Delete(e.history, 0, 1)
	}
	e.history = append(e.history, line)
	e.browsing = len(e.history)
}

// browse moves through the history by step, keeping the new line aside.
func (e *editor) browse(step int) {
	next := e.browsing + step
	if next < 0 || next > len(e.history) {
		return
	}
	if e.browsing == len(e.history) {
		e.draft = slices.Clone(e.line)
	}
	e.browsing = next
	if next == len(e.history) {
		e.set(e.draft)
		e.draft = nil
		return
	}
	e.set([]rune(e.history[next]))
}

// wordStart is where the word before the cursor starts.
func (e *editor) wordStart() int {
	i := e.pos
	for i > 0 && unicode.IsSpace(e.line[i-1]) {
		i--
	}
	for i > 0 && !unicode.IsSpace(e.line[i-1]) {
		i--
	}
	return i
}

// wordEnd is where the word after the cursor ends.
func (e *editor) wordEnd() int {
	i := e.pos
	for i < len(e.line) && unicode.IsSpace(e.line[i]) {
		i++
	}
	for i < len(e.line) && !unicode.IsSpace(e.line[i]) {
		i++
	}
	return i
}

func isBlank(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
