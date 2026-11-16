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
	Hints: "/help comandos", HintsRight: "Tab: panel", Tasks: "TAREAS", Notices: "AVISOS", NoTasks: "sin tareas en curso",
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
	rows := f.waitShown(t, "│ Listo, te aviso en 3 minutos. │", "✓ temporizador «té» · 3:00", "╭─ DOGGIE ─")
	// The cursor is back in the empty box.
	if !strings.Contains(strings.Join(rows, "\n"), "│ ›") {
		t.Errorf("no box:\n%s", strings.Join(rows, "\n"))
	}
	fmt.Fprintln(s, "· DOGGIE is running")
	f.waitShown(t, "· DOGGIE is running")
}

func TestScreenMultilineAndCompletion(t *testing.T) {
	s, f := openScreen(t, 60, 20, ScreenOptions{Commands: testCommands})
	f.typeKeys(t, "/ta")
	f.waitShown(t, "› /tasks") // the rest shows as a hint
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
	f.waitShown(t, "╭─ TAREAS ", "sin tareas en curso")
	f.typeKeys(t, "\t")
	f.waitGone(t, "TAREAS")
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

// The assistant at work shows below the newest message and moves, but
// not while the window is in the background: then nothing is drawn.
func TestScreenActivity(t *testing.T) {
	oldEvery, oldDelay := activityEvery, activityDelay
	activityEvery, activityDelay = 10*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { activityEvery, activityDelay = oldEvery, oldDelay })
	s, f := openScreen(t, 60, 16, ScreenOptions{})
	s.Reply("r", "hola")
	s.SetActivity("revisando tu agenda")
	f.waitShown(t, "│   hola   │", "• • •  revisando tu agenda")
	frames := func() int { return strings.Count(f.rawOutput(), syncBegin) }
	before := frames()
	time.Sleep(100 * time.Millisecond)
	if frames() == before {
		t.Fatal("the dots don't move")
	}
	f.typeKeys(t, "\x1b[O")
	time.Sleep(50 * time.Millisecond)
	before = frames()
	time.Sleep(150 * time.Millisecond)
	if n := frames() - before; n > 0 {
		t.Errorf("%d frames drawn in the background", n)
	}
	f.typeKeys(t, "\x1b[I")
	// Scrolled up, the dots are out of sight: nothing moves either.
	for i := range 30 {
		s.Reply(fmt.Sprint("m", i), "relleno")
	}
	f.typeKeys(t, "\x1b[5~")
	time.Sleep(50 * time.Millisecond)
	beforePaints := paints(s)
	time.Sleep(150 * time.Millisecond)
	if n := paints(s) - beforePaints; n > 0 {
		t.Errorf("%d frames made scrolled up", n)
	}
	f.typeKeys(t, "\x1b[1;5F")
	s.SetActivity("")
	f.waitGone(t, "revisando tu agenda")
	s.SetStatus("⏱ ropa 7:41 · +1 elsewhere")
	f.waitShown(t, "⏱ ropa 7:41 · +1 elsewhere")
	f.waitGone(t, "/help comandos")
	s.SetStatus("")
	f.waitShown(t, "/help comandos")
}

// An activity set and cleared at once, as between two blocks of a reply,
// never shows.
func TestScreenBriefActivityHidden(t *testing.T) {
	s, f := openScreen(t, 60, 16, ScreenOptions{})
	s.Reply("r", "hola")
	f.waitShown(t, "│   hola   │")
	s.SetActivity("pensando")
	s.SetActivity("")
	s.Reply("r2", "chau")
	f.waitShown(t, "│  chau  │")
	time.Sleep(50 * time.Millisecond)
	if strings.Contains(f.rawOutput(), "pensando") {
		t.Error("a brief activity showed")
	}
}

var sampleTasks = []TaskView{
	{ID: "t1", Title: "Recordar estirar", Detail: "5/10 s", Percent: 50},
	{ID: "t2", Title: "Pagar la luz", Detail: "espera tu respuesta · /answer t2", Waiting: true},
	{ID: "t3", Title: "Buscar recetas de lentejas con un nombre largo"},
}

// The panel is open from the start and shows the tasks and the last
// notices in cards; Tab closes it, and then, as on a narrow screen, the
// status tells the tasks below the box.
func TestScreenPanel(t *testing.T) {
	s, f := openScreen(t, 120, 26, ScreenOptions{})
	s.Notice("", NoticeGood, "t0 finished")
	s.Notice("", NoticeBad, "t9 failed: timed out")
	s.SetTasks(sampleTasks, "+1 en otras conversaciones")
	s.SetStatus("· t1 Recordar estirar: 5/10 s")
	rows := f.waitShown(t, "╭─ TAREAS ", "● Recordar estirar", "━━━", "50%", "t1 · 5/10 s", "? Pagar la luz", "t2 · espera tu respuesta", "+1 en otras conversaciones", "╭─ AVISOS ", "✗ t9 failed: timed out", "✓ t0 finished")
	golden(t, "panel-120x26.txt", strings.Join(rows, "\n")+"\n")
	if strings.Contains(strings.Join(rows, "\n"), "· t1 Recordar estirar: 5/10 s") {
		t.Error("the status shows with the panel")
	}
	f.typeKeys(t, "\t")
	f.waitGone(t, "TAREAS")
	f.waitShown(t, "· t1 Recordar estirar: 5/10 s")
	f.typeKeys(t, "\t")
	f.waitShown(t, "TAREAS")
	s.SetTasks(nil, "")
	f.waitShown(t, "sin tareas en curso") // it stays open, and says so
}

// A task that tells when it ends counts down in the panel by itself, with
// a bar that fills as its time passes.
func TestScreenPanelCountsDown(t *testing.T) {
	s, f := openScreen(t, 120, 26, ScreenOptions{})
	now := time.Now()
	s.SetTasks([]TaskView{
		{ID: "t1", Title: "Preparar té", Since: now.Add(-time.Hour), EndsAt: now.Add(time.Hour)},
		{ID: "t2", Title: "Hornear el pan", Since: now, EndsAt: now.Add(2900 * time.Millisecond)},
		{ID: "t3", Title: "Pagar la luz", Waiting: true, EndsAt: now.Add(time.Hour)},
	}, "")
	rows := strings.Join(f.waitShown(t, "● Preparar té", "59:5", "● Hornear el pan", "0:02", "━━━"), "\n")
	if strings.Contains(rows, "1:00:00") || strings.Count(rows, "59:5") != 1 {
		t.Errorf("a waiting task counts down, or the first shows no part of a second as a whole one:\n%s", rows)
	}
	f.waitShown(t, "0:01")
	f.waitShown(t, "0:00")
}

func TestCountdown(t *testing.T) {
	for _, tt := range []struct {
		left time.Duration
		want string
	}{
		{-time.Second, "0:00"}, {0, "0:00"}, {time.Millisecond, "0:01"}, {time.Second, "0:01"},
		{59*time.Second + 1, "1:00"}, {65 * time.Second, "1:05"}, {time.Hour, "1:00:00"},
		{24*time.Hour - time.Minute, "23:59:00"},
	} {
		now := time.Now()
		got, _ := TaskView{EndsAt: now.Add(tt.left)}.left(now)
		if got != tt.want {
			t.Errorf("%s left shows %q, want %q", tt.left, got, tt.want)
		}
	}
	since := time.Unix(1000, 0)
	for _, tt := range []struct {
		at   time.Duration
		want float64
	}{{-time.Minute, 0}, {0, 0}, {15 * time.Second, 0.25}, {time.Minute, 1}, {time.Hour, 1}} {
		if _, done := (TaskView{Since: since, EndsAt: since.Add(time.Minute)}).left(since.Add(tt.at)); done != tt.want {
			t.Errorf("%s into a minute is %v done, want %v", tt.at, done, tt.want)
		}
	}
	if _, done := (TaskView{EndsAt: since}).left(since); done != -1 {
		t.Errorf("a task that doesn't say since when is %v done, want no bar", done)
	}
}

// The panel wakes when the nearest countdown says another second, and
// not at all when none counts.
func TestNextCount(t *testing.T) {
	now := time.Unix(1000, 0)
	tasks := []TaskView{
		{EndsAt: now.Add(-time.Second)},
		{EndsAt: now.Add(time.Hour), Waiting: true},
		{},
	}
	if d, ok := nextCount(tasks, now); ok {
		t.Errorf("wakes in %s with nothing counting", d)
	}
	tasks = append(tasks, TaskView{EndsAt: now.Add(10*time.Second + 300*time.Millisecond)}, TaskView{EndsAt: now.Add(5 * time.Second)})
	if d, ok := nextCount(tasks, now); !ok || d != 300*time.Millisecond {
		t.Errorf("wakes in %s (%v), want 300ms", d, ok)
	}
	if d, ok := nextCount(tasks[4:], now); !ok || d != time.Second {
		t.Errorf("wakes in %s (%v), want 1s", d, ok)
	}
}

func TestScreenPanelNarrow(t *testing.T) {
	s, f := openScreen(t, 80, 20, ScreenOptions{})
	s.SetTasks(sampleTasks, "")
	s.SetStatus("· t1 Recordar estirar: 5/10 s")
	f.waitShown(t, "· t1 Recordar estirar: 5/10 s")
	if strings.Contains(strings.Join(f.shown(t), "\n"), "TAREAS") {
		t.Error("a panel on a narrow screen")
	}
}

// On a screen too narrow for the panel beside the conversation, Tab shows
// it in the conversation's place, and Tab again or saying something
// brings the conversation back.
func TestScreenPanelOverANarrowScreen(t *testing.T) {
	s, f := openScreen(t, 80, 20, ScreenOptions{})
	s.Reply("r", "Hola, ¿qué necesitas?")
	s.SetTasks(sampleTasks, "")
	f.waitShown(t, "Hola, ¿qué necesitas?")
	f.typeKeys(t, "\t")
	rows := f.waitShown(t, "╭─ TAREAS ", "● Recordar estirar", "t2 · espera tu respuesta")
	f.waitGone(t, "Hola, ¿qué necesitas?")
	for _, r := range rows {
		if n := len([]rune(r)); n > 80 {
			t.Errorf("a row %d columns wide on a screen of 80: %q", n, r)
		}
	}
	f.typeKeys(t, "\t")
	f.waitShown(t, "Hola, ¿qué necesitas?")
	f.waitGone(t, "TAREAS")

	f.typeKeys(t, "\tgracias\r")
	f.waitShown(t, "│ gracias │")
	f.waitGone(t, "TAREAS")

	// Wide again, the panel is back beside the conversation.
	f.typeKeys(t, "\t")
	f.waitShown(t, "TAREAS")
	f.mu.Lock()
	f.width, f.height = 120, 20
	f.mu.Unlock()
	f.resized <- struct{}{}
	f.waitShown(t, "TAREAS", "Hola, ¿qué necesitas?")
}

// A notice under a reply is told there, so the panel keeps only the ones
// that belong to no reply.
func TestPanelKeepsOnlyNoticesOfNoReply(t *testing.T) {
	s, f := openScreen(t, 120, 26, ScreenOptions{})
	s.Reply("r", "Listo.")
	s.Notice("r", NoticeGood, "t1 terminó")
	s.Notice("", NoticeBad, "t9 falló")
	rows := f.waitShown(t, "✓ t1 terminó", "╭─ AVISOS ", "✗ t9 falló")
	if n := strings.Count(strings.Join(rows, "\n"), "t1 terminó"); n != 1 {
		t.Errorf("the notice under its reply shows %d times, want 1:\n%s", n, strings.Join(rows, "\n"))
	}
}

// paints is how many frames s made, drawn or not: each is the screen
// waking up.
func paints(s *Screen) int {
	n := make(chan int)
	s.do(func(v *view) { v.quiet = true; n <- v.paints })
	return <-n
}

// Dragging over a reply marks it and copies its text, without the frame
// of its bubble; the wheel scrolls.
func TestScreenCopies(t *testing.T) {
	copied := make(chan string, 4)
	old := copyText
	copyText = func(_ Terminal, text string) { copied <- text }
	t.Cleanup(func() { copyText = old })
	texts := testTexts
	texts.Copied = "copiado"
	s, f := openScreen(t, 60, 16, ScreenOptions{Texts: texts})
	s.Reply("r", "Listo, te aviso en tres minutos.")
	rows := f.waitShown(t, "│ Listo, te aviso en tres minutos. │")
	y := slices.IndexFunc(rows, func(r string) bool { return strings.Contains(r, "Listo") })
	x := strings.Index(rows[y], "│")
	// From the frame on the left, over the row, to past the frame below.
	f.typeKeys(t, fmt.Sprintf("\x1b[<0;%d;%dM\x1b[<32;%d;%dM\x1b[<32;%d;%dM\x1b[<0;%d;%dm", x+1, y, x+5, y+1, 59, y+2, 59, y+2))
	select {
	case got := <-copied:
		if got != "Listo, te aviso en tres minutos." {
			t.Errorf("copied %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing copied")
	}
	f.waitShown(t, "copiado")
	// A click alone copies nothing, and a key clears what was marked.
	f.typeKeys(t, fmt.Sprintf("\x1b[<0;%d;%dM\x1b[<0;%d;%dm", x+3, y+1, x+3, y+1))
	f.typeKeys(t, "a")
	select {
	case got := <-copied:
		t.Errorf("a click copied %q", got)
	case <-time.After(100 * time.Millisecond):
	}
	for i := range 20 {
		s.Reply(fmt.Sprint(i), fmt.Sprintf("relleno %d", i))
	}
	f.waitShown(t, "relleno 19")
	f.typeKeys(t, "\x1b[<64;5;5M\x1b[<64;5;5M\x1b[<64;5;5M")
	f.waitGone(t, "relleno 19")
}

func TestSelectedTextKeepsInnerLines(t *testing.T) {
	v := &view{body: rect{0, 0, 20, 4}}
	var g grid
	g.resize(20, 4)
	g.text(0, 0, 20, "╭──────────╮", 0)
	g.text(0, 1, 20, "│ uno dos  │", 0)
	g.text(0, 2, 20, "│ tres     │", 0)
	g.text(0, 3, 20, "╰──────────╯", 0)
	v.sel = selection{on: true, ax: 0, ay: 0, bx: 19, by: 3}
	if got := v.selectedText(&g); got != "uno dos\ntres" {
		t.Errorf("%q", got)
	}
	// Backwards, from the end to the start, it is the same.
	v.sel = selection{on: true, ax: 19, ay: 3, bx: 0, by: 0}
	if got := v.selectedText(&g); got != "uno dos\ntres" {
		t.Errorf("backwards %q", got)
	}
}

// A terminal that doesn't tell when it changes size is asked now and then.
func TestScreenAsksForTheSize(t *testing.T) {
	f := newFullFake(t, 120, 20)
	f.resized = nil
	s := NewScreen(f, ScreenOptions{Name: "DOGGIE", Texts: testTexts})
	t.Cleanup(func() { s.Close() })
	f.waitShown(t, "TAREAS")
	f.mu.Lock()
	f.width = 80
	f.raw.Reset()
	f.mu.Unlock()
	f.waitShown(t, "/help comandos")
	if strings.Contains(strings.Join(f.shown(t), "\n"), "TAREAS") {
		t.Error("a panel at 80 columns")
	}
}

var testCommands = []Command{
	{Name: "/tasks", Hint: "tareas en curso"},
	{Name: "/tasks all", Hint: "también las que terminaron"},
	{Name: "/answer", Hint: "responder una pregunta", Args: true, Tasks: WaitingTasks, Text: true},
	{Name: "/cancel", Hint: "cancelar una tarea", Args: true, Tasks: AnyTask},
	{Name: "/help", Hint: "la lista de comandos"},
}

// Typing a / shows the commands with what they do, narrowed as more is
// typed; Up and Down choose, and Enter takes the one chosen, leaving one
// that needs more to be finished.
func TestScreenCommandMenu(t *testing.T) {
	s, f := openScreen(t, 70, 24, ScreenOptions{Commands: testCommands})
	f.typeKeys(t, "/")
	f.waitShown(t, "tareas en curso", "/tasks all", "también las que terminaron", "/answer", "/help", "la lista de comandos")
	f.typeKeys(t, "ta")
	f.waitGone(t, "la lista de comandos")
	f.typeKeys(t, "\x1b[B\r") // Down to "/tasks all", Enter
	if l, _ := next(t, s.Lines()); l != "/tasks all" {
		t.Errorf("Enter took %q", l)
	}
	f.typeKeys(t, "/an\r")
	f.waitShown(t, "› /answer")
	f.waitGone(t, "responder una pregunta") // typed whole: the menu goes
	f.typeKeys(t, "t3 sí\r")
	if l, _ := next(t, s.Lines()); l != "/answer t3 sí" {
		t.Errorf("an answer %q", l)
	}
	// Typed whole, Enter sends it as it is.
	f.typeKeys(t, "/tasks\r")
	if l, _ := next(t, s.Lines()); l != "/tasks" {
		t.Errorf("/tasks became %q", l)
	}
	// Up and Down go back to the history once no menu shows.
	f.typeKeys(t, "\x1b[A")
	f.waitShown(t, "› /tasks")
}

// Once a command about a task is typed, the menu offers the tasks it may
// be about, so their ids needn't be typed: the ones waiting for /answer,
// which then takes the answer, and any of them for /cancel.
func TestScreenMenuOffersTheTasks(t *testing.T) {
	s, f := openScreen(t, 70, 24, ScreenOptions{Commands: testCommands})
	s.SetTasks(sampleTasks, "")
	f.typeKeys(t, "/an\r")
	rows := f.waitShown(t, "› /answer t2", "/answer t2   Pagar la luz")
	if text := strings.Join(rows, "\n"); strings.Contains(text, "/answer t1") || strings.Contains(text, "/answer t3") {
		t.Errorf("a task that asks nothing is offered to answer:\n%s", text)
	}
	f.typeKeys(t, "\r")
	f.waitGone(t, "/answer t2   Pagar la luz")
	f.typeKeys(t, "sí\r")
	if l, _ := next(t, s.Lines()); l != "/answer t2 sí" {
		t.Errorf("an answer %q", l)
	}

	f.typeKeys(t, "/can\r")
	f.waitShown(t, "/cancel t1   Recordar estirar", "/cancel t2   Pagar la luz", "/cancel t3")
	f.typeKeys(t, "\x1b[B\x1b[B\r") // Down twice, to t3, and Enter sends it
	if l, _ := next(t, s.Lines()); l != "/cancel t3" {
		t.Errorf("a cancel %q", l)
	}

	// What is typed of the id narrows them, and an id of none shows none.
	f.typeKeys(t, "/cancel t2")
	f.waitGone(t, "Recordar estirar")
	f.typeKeys(t, "\x15/cancel x")
	f.waitGone(t, "Pagar la luz")
	f.typeKeys(t, "\x15")

	// With no task to choose, the command waits for the id as before.
	s.SetTasks(nil, "")
	f.typeKeys(t, "/an\r")
	f.waitShown(t, "› /answer")
	f.typeKeys(t, "t9 no\r")
	if l, _ := next(t, s.Lines()); l != "/answer t9 no" {
		t.Errorf("an answer typed whole %q", l)
	}
}
