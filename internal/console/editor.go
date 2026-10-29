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
	// forget reports the lines not to keep in the history; nil keeps all.
	forget func(string) bool

	// hidden is set while a line is typed unseen, after hiddenPrompt; the
	// line being typed before is kept in stashed meanwhile.
	hidden       bool
	hiddenPrompt []rune
	stashed      []rune
}

func newEditor(maxRunes int, forget func(string) bool) *editor {
	return &editor{maxRunes: maxRunes, forget: forget}
}

// hide starts a line typed unseen, after prompt. It ends when submitted or
// cancelled, and the line being typed before comes back.
func (e *editor) hide(prompt string) {
	if !e.hidden {
		e.stashed = e.line
	}
	e.hidden, e.hiddenPrompt = true, []rune(OneLine(prompt))
	e.set(nil)
}

// unhide ends a hidden line, bringing back the one typed before.
func (e *editor) unhide() {
	e.hidden, e.hiddenPrompt = false, nil
	e.set(e.stashed)
	e.stashed = nil
}

// apply edits the line with k. On actSubmit, line is what was submitted.
func (e *editor) apply(k key) (act action, line string) {
	switch k.kind {
	case keyRune:
		return e.insert([]rune{k.r}), ""
	case keyPaste:
		return e.insert([]rune(k.text)), ""
	case keyNewline:
		if e.hidden {
			return actRefused, "" // a hidden line is one line
		}
		return e.insert([]rune{'\n'}), ""
	case keyEnter:
		line = string(e.line)
		switch {
		case e.hidden:
			e.unhide()
			e.browsing = len(e.history)
		case e.forget != nil && e.forget(line):
			e.set(nil)
			e.browsing, e.draft = len(e.history), nil
		default:
			e.remember(line)
			e.set(nil)
		}
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
		for e.pos = min(e.pos, len(e.line)); e.pos > 0 && e.line[e.pos-1] != '\n'; e.pos-- {
		}
	case keyEnd:
		for ; e.pos < len(e.line) && e.line[e.pos] != '\n'; e.pos++ {
		}
	case keyKillStart:
		e.line = slices.Delete(e.line, 0, e.pos)
		e.pos = 0
	case keyKillEnd:
		e.line = e.line[:e.pos]
	case keyKillWord:
		start := e.wordStart()
		e.line = slices.Delete(e.line, start, e.pos)
		e.pos = start
	case keyUp, keyDown:
		if e.hidden {
			return actRefused, "" // the history would show in the hidden line
		}
		if e.moveLine(k.kind == keyUp) {
			return actNone, ""
		}
		if k.kind == keyUp {
			e.browse(-1)
		} else {
			e.browse(+1)
		}
	case keyInterrupt:
		// Ctrl+C cancels a hidden line, which is then submitted empty. It
		// drops what was typed otherwise; on an empty line it quits.
		switch {
		case e.hidden:
			e.unhide()
			return actSubmit, ""
		case len(e.line) == 0:
			return actQuit, ""
		}
		e.set(nil)
		e.browsing = len(e.history)
	case keyEOF:
		// Ctrl+D on an empty line cancels a hidden one, or quits. It
		// deletes forward otherwise.
		switch {
		case len(e.line) > 0:
			return e.apply(key{kind: keyDelete})
		case e.hidden:
			e.unhide()
			return actSubmit, ""
		}
		return actQuit, ""
	case keyRedraw:
		return actRedraw, ""
	}
	return actNone, ""
}

// insert puts rs at the cursor, as much as fits. Control characters are
// dropped, but newlines out of a hidden line: the line is drawn as is.
func (e *editor) insert(rs []rune) action {
	rs = slices.DeleteFunc(slices.Clone(rs), func(r rune) bool { return !printable(r) && (r != '\n' || e.hidden) })
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

// moveLine moves the cursor to the line above, or below, at the same rune
// in it as far as it goes, and reports whether there was such a line.
func (e *editor) moveLine(up bool) bool {
	start := e.pos
	for start > 0 && e.line[start-1] != '\n' {
		start--
	}
	col := e.pos - start
	if up {
		if start == 0 {
			return false
		}
		prev := start - 1
		for prev > 0 && e.line[prev-1] != '\n' {
			prev--
		}
		e.pos = min(prev+col, start-1)
		return true
	}
	end := e.pos
	for end < len(e.line) && e.line[end] != '\n' {
		end++
	}
	if end == len(e.line) {
		return false
	}
	next := end + 1
	stop := next
	for stop < len(e.line) && e.line[stop] != '\n' {
		stop++
	}
	e.pos = min(next+col, stop)
	return true
}
