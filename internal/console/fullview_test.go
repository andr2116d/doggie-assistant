package console

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

var testTexts = ScreenTexts{
	Online: "en línea", Offline: "sin conexión", Channel: "terminal",
	Hints: "/help comandos", HintsRight: "Tab: panel", Tasks: "TAREAS", Notices: "AVISOS",
	TooSmall: "Agranda la ventana", Newer: func(n int) string { return fmt.Sprintf("↓ %d nuevos", n) },
}

func openScreen(t *testing.T, w, h int, opts ScreenOptions) (*Screen, *fakeTerm) {
	t.Helper()
	if opts.Name == "" {
		opts.Name = "DOGGIE"
	}
	if opts.Texts.Online == "" {
		opts.Texts = testTexts
	}
	f := newFullFake(t, w, h)
	s := NewScreen(f, opts)
	t.Cleanup(func() { s.Close() })
	return s, f
}

// shown replays what the screen wrote on a fresh full-screen terminal and
// returns its rows, trailing blanks cut.
func (f *fakeTerm) shown(t *testing.T) []string {
	t.Helper()
	f.mu.Lock()
	raw, w, h := f.raw.String(), f.width, f.height
	f.mu.Unlock()
	// From the last full redraw: frames before a resize had another size.
	if i := strings.LastIndex(raw, "\x1b[0m\x1b[2J"); i > 0 {
		raw = raw[i:]
	}
	v := newFullVT(t, w, h)
	v.Write([]byte(raw))
	rows := make([]string, h)
	for y := range h {
		rows[y] = strings.TrimRight(strings.Join(v.text[y], ""), " ")
	}
	return rows
}

// waitShown waits until some row of the screen contains each of want.
func (f *fakeTerm) waitShown(t *testing.T, want ...string) []string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows := f.shown(t)
		all := true
		for _, w := range want {
			if !slices.ContainsFunc(rows, func(r string) bool { return strings.Contains(r, w) }) {
				all = false
			}
		}
		if all {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("screen lacks %q:\n%s", want, strings.Join(rows, "\n"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestScreenTalks(t *testing.T) {
	s, f := openScreen(t, 80, 20, ScreenOptions{})
	f.waitShown(t, "◆ DOGGIE", "sin conexión", "/help comandos")
	s.SetConnection(true, "")
	f.typeKeys(t, "pon un temporizador de 3 minutos para el té\r")
	if l, ok := next(t, s.Lines()); !ok || l != "pon un temporizador de 3 minutos para el té" {
		t.Fatalf("line %q", l)
	}
	f.waitShown(t, "en línea", "pon un temporizador de 3 minutos para el té")
	s.Stream("r1", "Listo, te ")
	s.Stream("r1", "aviso en 3 minutos.")
	s.Notice("r1", NoticeGood, "temporizador «té» · 3:00")
	s.Reply("r1", "Listo, te aviso en 3 minutos.")
	rows := f.waitShown(t, "│ Listo, te aviso en 3 minutos. │", "✓ temporizador «té» · 3:00", "DOGGIE")
	// The cursor is back in the empty box.
	if !strings.Contains(strings.Join(rows, "\n"), "│ ›") {
		t.Errorf("no box:\n%s", strings.Join(rows, "\n"))
	}
	fmt.Fprintln(s, "· DOGGIE is running")
	f.waitShown(t, "· DOGGIE is running")
}

func TestScreenMultilineAndCompletion(t *testing.T) {
	s, f := openScreen(t, 60, 20, ScreenOptions{Commands: []string{"/tasks", "/tasks all", "/help"}})
	f.typeKeys(t, "/ta")
	f.waitShown(t, "/tasks") // the rest shows as a hint
	f.typeKeys(t, "\t\r")
	if l, _ := next(t, s.Lines()); l != "/tasks" {
		t.Errorf("completed %q", l)
	}
	f.typeKeys(t, "primera línea\x1b\rsegunda línea")
	f.waitShown(t, "› primera línea", "  segunda línea")
	f.typeKeys(t, "\r")
	if l, _ := next(t, s.Lines()); l != "primera línea\nsegunda línea" {
		t.Errorf("multiline %q", l)
	}
}

func TestScreenScrolls(t *testing.T) {
	s, f := openScreen(t, 60, 16, ScreenOptions{})
	for i := range 20 {
		s.Reply(fmt.Sprint(i), fmt.Sprintf("respuesta número %d", i))
	}
	f.waitShown(t, "respuesta número 19")
	f.typeKeys(t, "\x1b[5~\x1b[5~")
	f.waitGone(t, "número 19")
	s.Reply("new", "una nueva")
	f.waitShown(t, "↓ 1 nuevos")
	f.typeKeys(t, "\x1b[1;5F")
	f.waitShown(t, "una nueva")
	// Saying something while scrolled up goes back down to it.
	f.typeKeys(t, "\x1b[5~\x1b[5~")
	f.waitGone(t, "una nueva")
	f.typeKeys(t, "y ahora qué\r")
	next(t, s.Lines())
	f.waitShown(t, "y ahora qué", "una nueva")
}

func TestScreenHidden(t *testing.T) {
	s, f := openScreen(t, 60, 16, ScreenOptions{})
	f.typeKeys(t, "a medias")
	f.waitShown(t, "› a medias")
	answer := s.AskHidden("answer for t3 (hidden): ")
	f.waitShown(t, "answer for t3 (hidden):")
	f.typeKeys(t, "secreto\r")
	if got := <-answer; got != "secreto" {
		t.Errorf("answer %q", got)
	}
	rows := f.waitShown(t, "a medias")
	if strings.Contains(strings.Join(rows, "\n"), "secreto") {
		t.Error("the hidden answer shows")
	}
}

func TestScreenPanelAndSize(t *testing.T) {
	s, f := openScreen(t, 120, 20, ScreenOptions{})
	f.waitShown(t, "/help")
	f.typeKeys(t, "\t")
	f.waitShown(t, "│ TAREAS")
	f.mu.Lock()
	f.width, f.height = 30, 8
	f.raw.Reset() // what was drawn before had the old size
	f.mu.Unlock()
	f.resized <- struct{}{}
	f.waitShown(t, "Agranda la ventana")
	s.Close()
	if out := f.rawOutput(); !strings.HasSuffix(out, leaveFull) || f.restored != 1 {
		t.Errorf("not left as it was: restored %d", f.restored)
	}
}

func TestScreenQuits(t *testing.T) {
	s, f := openScreen(t, 60, 16, ScreenOptions{})
	f.typeKeys(t, "\x03")
	select {
	case _, ok := <-s.Lines():
		if ok {
			t.Fatal("a line instead of quitting")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Ctrl+C on an empty line didn't quit")
	}
}

func TestInputRows(t *testing.T) {
	rows, r, c := inputRows([]rune("abcdef\ngh"), 9, 4)
	got := []string{}
	for _, row := range rows {
		got = append(got, string(row))
	}
	if !slices.Equal(got, []string{"abcd", "ef", "gh"}) || r != 2 || c != 2 {
		t.Errorf("%q, cursor %d,%d", got, r, c)
	}
	rows, r, c = inputRows([]rune("abcd"), 4, 4)
	if len(rows) != 2 || r != 1 || c != 0 {
		t.Errorf("a full row: %d rows, cursor %d,%d", len(rows), r, c)
	}
	_, r, c = inputRows([]rune("日本語"), 3, 4)
	if r != 1 || c != 2 {
		t.Errorf("wide: cursor %d,%d", r, c)
	}
}

// waitGone waits until no row of the screen contains text.
func (f *fakeTerm) waitGone(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for slices.ContainsFunc(f.shown(t), func(r string) bool { return strings.Contains(r, text) }) {
		if time.Now().After(deadline) {
			t.Fatalf("%q still shows:\n%s", text, strings.Join(f.shown(t), "\n"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}
