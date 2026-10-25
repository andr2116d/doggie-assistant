package console

import "testing"

// typed applies keys to a fresh editor, typing s first.
func typed(t *testing.T, max int, s string, keys ...key) *editor {
	t.Helper()
	e := newEditor(max)
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
	e := newEditor(0)
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
	e := newEditor(0)
	for i := range maxHistory + 10 {
		e.apply(r(rune('a' + i%26)))
		e.apply(r(rune('0' + i%10)))
		e.apply(k(keyEnter))
	}
	if len(e.history) > maxHistory {
		t.Errorf("history holds %d lines", len(e.history))
	}
}
