package console

import "testing"

// typed applies keys to a fresh editor, typing s first.
func typed(t *testing.T, max int, s string, keys ...key) *editor {
	t.Helper()
	e := newEditor(max, nil)
	for _, c := range s {
		e.apply(r(c))
	}
	for _, k := range keys {
		e.apply(k)
	}
	return e
}

func TestEditing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		typed string
		keys  []key
		line  string
		pos   int
	}{
		{"type", "héllo", nil, "héllo", 5},
		{"backspace", "abc", []key{k(keyBackspace)}, "ab", 2},
		{"backspace at start", "abc", []key{k(keyHome), k(keyBackspace)}, "abc", 0},
		{"delete", "abc", []key{k(keyHome), k(keyDelete)}, "bc", 0},
		{"delete at end", "abc", []key{k(keyDelete)}, "abc", 3},
		{"insert in the middle", "ac", []key{k(keyLeft), r('b')}, "abc", 2},
		{"left at start", "a", []key{k(keyLeft), k(keyLeft)}, "a", 0},
		{"right at end", "a", []key{k(keyRight)}, "a", 1},
		{"home end", "abc", []key{k(keyHome), k(keyEnd)}, "abc", 3},
		{"kill start", "abc def", []key{k(keyLeft), k(keyLeft), k(keyKillStart)}, "ef", 0},
		{"kill end", "abc def", []key{k(keyHome), k(keyRight), k(keyKillEnd)}, "a", 1},
		{"kill word", "abc  def  ", []key{k(keyKillWord)}, "abc  ", 5},
		{"kill word at start", "abc", []key{k(keyHome), k(keyKillWord)}, "abc", 0},
		{"word left", "ab cd", []key{k(keyWordLeft)}, "ab cd", 3},
		{"word left twice", "ab cd", []key{k(keyWordLeft), k(keyWordLeft)}, "ab cd", 0},
		{"word right", "ab cd", []key{k(keyHome), k(keyWordRight)}, "ab cd", 2},
		{"paste at cursor", "ad", []key{k(keyLeft), paste("bc")}, "abcd", 3},
		{"ctrl+c clears the line", "abc", []key{k(keyInterrupt)}, "", 0},
		{"ctrl+d deletes forward", "abc", []key{k(keyHome), k(keyEOF)}, "bc", 0},
		{"emoji", "a😀b", []key{k(keyLeft), k(keyBackspace)}, "ab", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := typed(t, 0, tc.typed, tc.keys...)
			if string(e.line) != tc.line || e.pos != tc.pos {
				t.Errorf("line %q at %d, want %q at %d", string(e.line), e.pos, tc.line, tc.pos)
			}
		})
	}
}

func TestSubmitAndQuit(t *testing.T) {
	e := typed(t, 0, "  hola  ")
	if act, line := e.apply(k(keyEnter)); act != actSubmit || line != "  hola  " {
		t.Errorf("enter = %v %q", act, line)
	}
	if len(e.line) != 0 || e.pos != 0 {
		t.Error("line not cleared after submit")
	}
	if act, line := e.apply(k(keyEnter)); act != actSubmit || line != "" {
		t.Errorf("empty enter = %v %q", act, line)
	}
	if act, _ := e.apply(k(keyInterrupt)); act != actQuit {
		t.Error("ctrl+c on an empty line doesn't quit")
	}
	if act, _ := e.apply(k(keyEOF)); act != actQuit {
		t.Error("ctrl+d on an empty line doesn't quit")
	}
	if act, _ := typed(t, 0, "x").apply(k(keyInterrupt)); act != actNone {
		t.Error("ctrl+c on a line quits")
	}
}

func TestMaxRunes(t *testing.T) {
	e := typed(t, 3, "abc")
	if act := e.insert([]rune("d")); act != actRefused || string(e.line) != "abc" {
		t.Errorf("typing past the max: %v %q", act, string(e.line))
	}
	e = typed(t, 4, "a")
	if act, _ := e.apply(paste("bcdef")); act != actRefused || string(e.line) != "abcd" || e.pos != 4 {
		t.Errorf("pasting past the max: %v %q at %d", act, string(e.line), e.pos)
	}
}

func TestHistory(t *testing.T) {
	e := newEditor(0, nil)
	for _, s := range []string{"one", "two", "two", " ", "three"} {
		for _, c := range s {
			e.apply(r(c))
		}
		e.apply(k(keyEnter))
	}
	if got := len(e.history); got != 3 {
		t.Fatalf("history %q: repeated and blank lines kept", e.history)
	}
	for _, c := range "draft" {
		e.apply(r(c))
	}
	steps := []struct {
		k    key
		want string
	}{
		{k(keyUp), "three"}, {k(keyUp), "two"}, {k(keyUp), "one"}, {k(keyUp), "one"},
		{k(keyDown), "two"}, {k(keyDown), "three"}, {k(keyDown), "draft"}, {k(keyDown), "draft"},
	}
	for i, s := range steps {
		e.apply(s.k)
		if string(e.line) != s.want || e.pos != len(e.line) {
			t.Fatalf("step %d: %q at %d, want %q at end", i, string(e.line), e.pos, s.want)
		}
	}
	// Submitting from history starts a new line again.
	e.apply(k(keyUp))
	e.apply(k(keyEnter))
	e.apply(k(keyUp))
	if string(e.line) != "three" {
		t.Errorf("after resubmitting: up = %q", string(e.line))
	}
}

func TestHistoryIsBounded(t *testing.T) {
	e := newEditor(0, nil)
	for i := range maxHistory + 10 {
		e.apply(r(rune('a' + i%26)))
		e.apply(r(rune('0' + i%10)))
		e.apply(k(keyEnter))
	}
	if len(e.history) > maxHistory {
		t.Errorf("history holds %d lines", len(e.history))
	}
}

func TestForgottenLines(t *testing.T) {
	e := newEditor(0, func(l string) bool { return len(l) > 0 && l[0] == '/' })
	for _, s := range []string{"hola", "/answer t1 1234"} {
		for _, c := range s {
			e.apply(r(c))
		}
		if act, line := e.apply(k(keyEnter)); act != actSubmit || line != s {
			t.Fatalf("submitted %q, %v", line, act)
		}
	}
	e.apply(k(keyUp))
	if string(e.line) != "hola" {
		t.Errorf("Up shows %q, want the line before the forgotten one", string(e.line))
	}
	e.apply(k(keyUp))
	if string(e.line) != "hola" {
		t.Errorf("the forgotten line is in the history: %q", string(e.line))
	}
}

func TestHiddenLine(t *testing.T) {
	e := typed(t, 0, "medio escrito")
	e.hide("answer for t5: ")
	if !e.hidden || len(e.line) != 0 {
		t.Fatalf("hidden %v, line %q", e.hidden, string(e.line))
	}
	for _, c := range "1234" {
		e.apply(r(c))
	}
	if act, _ := e.apply(k(keyUp)); act != actRefused || string(e.line) != "1234" {
		t.Errorf("Up in a hidden line: %v, line %q", act, string(e.line))
	}
	if act, line := e.apply(k(keyEnter)); act != actSubmit || line != "1234" {
		t.Errorf("submitted %q, %v", line, act)
	}
	if e.hidden || string(e.line) != "medio escrito" || e.pos != len("medio escrito") {
		t.Errorf("after it: hidden %v, line %q at %d", e.hidden, string(e.line), e.pos)
	}
	e.apply(k(keyEnter))
	e.apply(k(keyUp))
	if string(e.line) != "medio escrito" {
		t.Errorf("Up shows %q", string(e.line))
	}
	e.apply(k(keyUp))
	if string(e.line) == "1234" {
		t.Error("the hidden line is in the history")
	}

	// Ctrl+C, or Ctrl+D on an empty line, cancel it; Ctrl+D deletes otherwise.
	for name, keys := range map[string][]key{
		"ctrl+c":       {r('x'), k(keyInterrupt)},
		"ctrl+d empty": {k(keyEOF)},
		"ctrl+d":       {r('x'), k(keyLeft), k(keyEOF), k(keyEOF)},
	} {
		e := typed(t, 0, "antes")
		e.hide("? ")
		var act action
		var line string
		for _, key := range keys {
			act, line = e.apply(key)
		}
		if act != actSubmit || line != "" || e.hidden || string(e.line) != "antes" {
			t.Errorf("%s: %v %q, hidden %v, line %q", name, act, line, e.hidden, string(e.line))
		}
	}
	// A hidden line that can't be typed in still quits nothing.
	e = typed(t, 0, "")
	e.hide("? ")
	if act, _ := e.apply(k(keyInterrupt)); act == actQuit {
		t.Error("Ctrl+C on a hidden line quit")
	}
}

func TestEditorLines(t *testing.T) {
	e := newEditor(0, nil)
	e.multiline = true
	for _, k := range []key{{kind: keyRune, r: 'a'}, {kind: keyRune, r: 'b'}, {kind: keyNewline}, {kind: keyPaste, text: "cde\nf"}} {
		e.apply(k)
	}
	if string(e.line) != "ab\ncde\nf" || e.pos != len(e.line) {
		t.Fatalf("%q at %d", string(e.line), e.pos)
	}
	e.apply(key{kind: keyUp}) // from after "f" to after "c"
	if e.pos != 4 {
		t.Errorf("up: at %d", e.pos)
	}
	e.apply(key{kind: keyEnd}) // the end of "cde"
	if e.pos != 6 {
		t.Errorf("end: at %d", e.pos)
	}
	e.apply(key{kind: keyUp}) // past the end of the shorter line above
	if e.pos != 2 {
		t.Errorf("up again: at %d", e.pos)
	}
	e.pos = 0
	if e.moveLine(true) {
		t.Error("moved above the first line")
	}
	e.pos = len(e.line)
	if e.moveLine(false) {
		t.Error("moved below the last line")
	}
	if act, line := e.apply(key{kind: keyEnter}); act != actSubmit || line != "ab\ncde\nf" {
		t.Errorf("submit %v %q", act, line)
	}
	single := newEditor(0, nil)
	if act, _ := single.apply(key{kind: keyNewline}); act != actRefused {
		t.Error("a single-line editor took a newline")
	}
	single.apply(key{kind: keyPaste, text: "a\nb"})
	if string(single.line) != "ab" {
		t.Errorf("single-line paste kept a newline: %q", string(single.line))
	}
}
