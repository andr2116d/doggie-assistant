package console

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-runewidth"
)

// ScreenTexts are the words the full screen shows, in the user's language.
type ScreenTexts struct {
	Online, Offline string // the connection, on the top bar
	Channel         string // the conversation's channel, on the top bar
	// Hints go below the input box, on the left and on the right.
	Hints, HintsRight string
	Tasks, Notices    string // the panel's two cards
	NoTasks           string // in the tasks card while there are none
	TooSmall          string // alone on a screen too small for the chat
	// Newer tells how many messages came while scrolled up.
	Newer func(n int) string
	// Copied tells, for a moment, that what was marked was copied.
	Copied string
	// Clock tells the time on the top bar; nil shows none.
	Clock func(time.Time) string
}

// ScreenOptions configures a Screen.
type ScreenOptions struct {
	Name string // the assistant's, above its replies
	// MaxRunes bounds what is typed; zero means no bound.
	MaxRunes int
	// Forget reports the submitted lines not to keep for Up and Down.
	Forget func(line string) bool
	// Commands are completed as they are typed, e.g. "/tasks".
	Commands []string
	Texts    ScreenTexts
	// Colors is how much color to use. OpenScreen sets it from the
	// environment.
	Colors Profile
}

// NoticeKind is what a notice tells: it picks its mark and color.
type NoticeKind = noteKind

// Kinds of notices.
const (
	NoticeInfo = noteInfo
	NoticeGood = noteGood
	NoticeWait = noteWait
	NoticeBad  = noteBad
)

// Screen is a chat that fills the terminal: the conversation in bubbles, a
// panel of tasks, and a box to type in. Its methods may be called from any
// goroutine; what they show is drawn in the next frame.
type Screen struct {
	ops   chan func(*view)
	lines chan string
	hides chan hiddenAsk
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
}

// OpenScreen starts a Screen on in and out, which must be terminals; it
// keeps them in raw mode until Close.
func OpenScreen(in, out *os.File, opts ScreenOptions) (*Screen, error) {
	t, err := openTerminal(in, out)
	if err != nil {
		return nil, err
	}
	opts.Colors = DetectProfile(os.Getenv)
	return NewScreen(t, opts), nil
}

// NewScreen starts a Screen on t.
func NewScreen(t Terminal, opts ScreenOptions) *Screen {
	s := &Screen{
		ops:   make(chan func(*view), 64),
		lines: make(chan string),
		hides: make(chan hiddenAsk),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go s.run(t, opts)
	return s
}

// Lines delivers each line the user submits. It is closed when the user
// quits (Ctrl+C or Ctrl+D on an empty line), at the end of the input, or on
// Close.
func (s *Screen) Lines() <-chan string { return s.lines }

// IsInteractive reports true: a Screen always edits lines on a terminal.
func (s *Screen) IsInteractive() bool { return true }

// do runs f on the view in its goroutine, unless the screen is closed.
func (s *Screen) do(f func(*view)) {
	select {
	case s.ops <- f:
	case <-s.done:
	}
}

// Write shows each line of p as a note of the chat. Control characters are
// dropped, so the text may come from anyone.
func (s *Screen) Write(p []byte) (int, error) {
	text := strings.TrimRight(string(p), "\n")
	select {
	case <-s.done:
		return 0, ErrClosed
	default:
	}
	s.do(func(v *view) {
		for line := range strings.SplitSeq(text, "\n") {
			v.convo.say(fromChat, "", strings.TrimPrefix(line, "· "), noteInfo)
		}
	})
	return len(p), nil
}

// Said shows text the user said before, e.g. on opening the conversation.
func (s *Screen) Said(text string) {
	s.do(func(v *view) { v.convo.say(fromUser, "", text, noteInfo) })
}

// SaidElsewhere shows text said in the conversation on another channel,
// under label, e.g. "voice:kitchen".
func (s *Screen) SaidElsewhere(label, text string) {
	s.do(func(v *view) { v.convo.say(fromElsewhere, label, text, noteInfo) })
}

// Stream adds text to the assistant's reply id, starting it if new. The
// text flows in at a steady pace.
func (s *Screen) Stream(id, text string) {
	s.do(func(v *view) { v.convo.stream(id, text); v.pacing = true })
}

// Reply ends the reply id with its whole text, which corrects what came
// in pieces; a reply never streamed shows at once.
func (s *Screen) Reply(id, text string) {
	s.do(func(v *view) { v.convo.finish(id, text); v.pacing = true })
}

// Notice shows text under the message id, or on its own if there is none.
func (s *Screen) Notice(id string, kind NoticeKind, text string) {
	s.do(func(v *view) {
		n := notice{kind, strings.TrimSpace(oneLine(text))}
		v.convo.noteOn(id, notice{kind, text})
		v.remember(n)
	})
}

// SetConnection shows whether the chat is connected, and how, on the top
// bar.
func (s *Screen) SetConnection(online bool, status string) {
	s.do(func(v *view) { v.online, v.status = online, status })
}

// SetActivity shows the assistant at work below the newest message, with
// text telling what it does and, once it goes long, for how long; empty
// text removes it.
func (s *Screen) SetActivity(text string) {
	s.do(func(v *view) {
		text = strings.TrimSpace(oneLine(text))
		if text != "" && v.activity == "" {
			v.activitySince = time.Now()
		}
		v.activity = text
	})
}

// SetStatus shows text, e.g. the tasks running, below the input box in
// place of the hints; empty text brings the hints back.
func (s *Screen) SetStatus(text string) {
	s.do(func(v *view) { v.taskStatus = strings.TrimSpace(oneLine(text)) })
}

// AskHidden has the user type the next line unseen, after prompt, as a
// password, and delivers it on the returned channel, never on Lines. What
// was being typed waits meanwhile. Ctrl+C, or Ctrl+D on an empty line,
// cancel it, which delivers an empty line, as does a newer AskHidden. The
// channel is closed if the screen closes first.
func (s *Screen) AskHidden(prompt string) <-chan string {
	answer := make(chan string, 1)
	select {
	case s.hides <- hiddenAsk{prompt, answer}:
	case <-s.done:
		close(answer)
	}
	return answer
}

// Close ends the screen and puts the terminal back as it was.
func (s *Screen) Close() error {
	s.once.Do(func() { close(s.stop) })
	<-s.done
	return nil
}

// view is the state of a Screen, owned by its goroutine.
type view struct {
	opts      ScreenOptions
	fs        *fullScreen
	lk        looks
	convo     *conversation
	ed        *editor
	online    bool
	status    string
	panelMode panelMode
	tasks     []TaskView
	moreTasks string
	notices   []notice // the last ones that weren't only information
	pacing    bool     // a reply may have text to pace in
	// activity is what the assistant does, since activitySince; taskStatus
	// replaces the hints while set.
	activity      string
	activitySince time.Time
	taskStatus    string
	// unfocused is set while the window is in the background, where
	// nothing needs to move.
	unfocused bool
	// sel is the text marked with the mouse; copiedAt when it was last
	// copied, which the hints tell for a while.
	sel      selection
	copiedAt time.Time
	// paints counts the frames made, drawn or not; quiet is set by an op
	// that changes nothing on screen, so it asks for none.
	paints int
	quiet  bool
	// inputScroll is the first row of what is typed shown in the box.
	inputScroll int
	body        rect // where the conversation was last drawn
}

func (s *Screen) run(t Terminal, opts ScreenOptions) {
	defer close(s.done)
	fs := newFullScreen(t, opts.Colors)
	v := &view{
		opts: opts, fs: fs, lk: newLooks(fs.styles, violetMint),
		convo: newConversation(opts.Name), ed: newEditor(opts.MaxRunes, opts.Forget),
	}
	fs.guard(func() { s.loop(t, v) })
}

func (s *Screen) loop(t Terminal, v *view) {
	q := lineQueue{lines: s.lines}
	fs := v.fs
	if err := fs.enter(); err != nil {
		q.close()
		t.Restore()
		return
	}
	input := reader(t, s.done)
	var dec decoder
	pace, anim := newClock(), newClock()
	defer pace.stop()
	defer anim.stop()
	defer fs.gate.stop()
	minute := time.NewTimer(untilNextMinute(time.Now()))
	defer minute.Stop()
	// A terminal that doesn't tell when it changes size, as on Windows, is
	// asked now and then.
	var poll <-chan time.Time
	if t.Resized() == nil {
		ticker := time.NewTicker(sizePoll)
		defer ticker.Stop()
		poll = ticker.C
	}
	want := func() { fs.gate.want(time.Now()) }
	draw := func() {
		fs.fit()
		fs.draw(v.paint)
		fs.gate.drawn(time.Now())
	}
	draw()
	for {
		q.closeIfDone()
		if v.pacing {
			pace.start(frameEvery)
			v.pacing = false
		}
		// The dots move only where they can be seen: in front, at the bottom.
		if v.activity != "" && !v.unfocused && v.convo.scroll == 0 {
			anim.start(activityEvery)
		} else {
			anim.stop()
		}
		select {
		case p, ok := <-input:
			if !ok {
				input = nil
				q.ending = true
				continue
			}
			for _, k := range dec.feed(p) {
				if k.kind == keyMouse {
					v.mouse(k.mouse, t, s)
					continue
				}
				v.sel = selection{} // a key ends what was marked
				v.key(k, &q)
			}
			want()
		case op := <-s.ops:
			op(v)
			if !v.quiet {
				want()
			}
			v.quiet = false
		case ask := <-s.hides:
			if q.hidden != nil {
				q.hidden <- "" // replaced
			}
			q.hidden = ask.answer
			v.ed.hide(ask.prompt)
			want()
		case <-anim.c:
			v.convo.phase++
			want()
		case <-pace.c:
			if !v.convo.pace() {
				pace.stop()
			}
			want()
		case <-fs.gate.c:
			draw()
		case <-t.Resized():
			want()
		case <-poll:
			if fs.fit() {
				want()
			}
		case <-minute.C:
			minute.Reset(untilNextMinute(time.Now()))
			want()
		case q.send() <- q.next():
			q.sent()
		case <-s.stop:
			fs.leave()
			t.Restore()
			q.close()
			if q.hidden != nil {
				close(q.hidden)
			}
			return
		}
	}
}

func untilNextMinute(now time.Time) time.Duration {
	return now.Truncate(time.Minute).Add(time.Minute).Sub(now)
}

// key applies a key: to the conversation's view, or to what is typed.
func (v *view) key(k key, q *lineQueue) {
	if q.ending {
		return
	}
	page := max(v.body.h-2, 1)
	switch k.kind {
	case keyPageUp:
		v.convo.scrollBy(page)
		return
	case keyPageDown:
		v.convo.scrollBy(-page)
		return
	case keyScrollUp:
		v.convo.scrollBy(1)
		return
	case keyScrollDown:
		v.convo.scrollBy(-1)
		return
	case keyTop:
		v.convo.scrollBy(1 << 30)
		return
	case keyBottom:
		v.convo.scrollBy(-(1 << 30))
		return
	case keyTab:
		switch g := v.ghost(); {
		case g != "":
			v.ed.apply(key{kind: keyPaste, text: g})
		case v.showsPanel():
			v.panelMode = panelOff
		default:
			v.panelMode = panelOn
		}
		return
	case keyRight:
		if g := v.ghost(); g != "" {
			v.ed.apply(key{kind: keyPaste, text: g})
			return
		}
	case keyFocusIn, keyFocusOut:
		v.unfocused = k.kind == keyFocusOut
		return
	case keyRedraw:
		v.fs.painter.invalidate()
		return
	}
	hidden := v.ed.hidden
	act, line := v.ed.apply(k)
	switch act {
	case actSubmit:
		if hidden {
			q.hidden <- line
			q.hidden = nil
			return
		}
		q.pending = append(q.pending, line)
		if !isBlank(line) {
			v.convo.say(fromUser, "", line, noteInfo)
			v.convo.scrollBy(-(1 << 30))
		}
	case actQuit:
		q.ending = true
	}
}

// ghost is what would complete the command being typed, if one does: the
// rest that all the commands starting with it share.
func (v *view) ghost() string {
	line := string(v.ed.line)
	if v.ed.hidden || !strings.HasPrefix(line, "/") || strings.ContainsAny(line, " \n") || v.ed.pos != len(v.ed.line) {
		return ""
	}
	common := ""
	for _, c := range v.opts.Commands {
		if !strings.HasPrefix(c, line) || c == line {
			continue
		}
		rest := c[len(line):]
		if common == "" {
			common = rest
			continue
		}
		n := 0
		for n < len(common) && n < len(rest) && common[n] == rest[n] {
			n++
		}
		common = common[:n]
	}
	return common
}

// paint draws the whole screen, and returns where the cursor goes.
func (v *view) paint(g *grid) (int, int, bool) {
	v.paints++
	t := v.opts.Texts
	width := g.W - 8 // the box's frame, margins, prompt and padding
	rows, _, _ := inputRows(v.ed.line, v.ed.pos, max(width, 1))
	r := layout(g.W, g.H, v.showsPanel(), len(rows))
	c := chrome{
		name: v.opts.Name, online: v.online, status: v.status, channel: t.Channel,
		hints: t.Hints, hintsRight: t.HintsRight, tooSmall: t.TooSmall,
	}
	if c.status == "" {
		c.status = t.Offline
		if v.online {
			c.status = t.Online
		}
	}
	if t.Clock != nil {
		c.clock = t.Clock(time.Now())
	}
	if v.taskStatus != "" && r.panel.empty() {
		c.hints = v.taskStatus // where the panel doesn't show the tasks
	}
	if time.Since(v.copiedAt) < copiedFor && t.Copied != "" {
		c.hints = t.Copied
	}
	// An activity shows from the first move of its dots on, so one set
	// and cleared at once, as between two blocks of a reply, never shows.
	v.convo.activity = ""
	if d := time.Since(v.activitySince); v.activity != "" && d >= activityEvery {
		v.convo.activity = v.activity
		if d >= shownAfter {
			v.convo.activity += " · " + since(d)
		}
	}
	drawChrome(g, r, v.lk, c)
	if r.tooSmall {
		return 0, 0, false
	}
	v.body = r.body
	v.convo.draw(g, r.body, v.lk, t.Newer)
	if !r.panel.empty() {
		v.drawPanel(g, r.panel)
	}
	if v.sel.on {
		v.highlight(g)
	}
	x, y := v.drawInput(g, r.input)
	return x, y, true
}

// drawInput draws what is typed in the box, and returns where the cursor
// goes.
func (v *view) drawInput(g *grid, box rect) (int, int) {
	left, right := box.x+5, box.x+box.w-3
	if v.ed.hidden {
		x := textIn(g, left, box.y+1, right, string(v.ed.hiddenPrompt), v.lk.hint)
		return min(x, right-1), box.y + 1
	}
	rows, crow, ccol := inputRows(v.ed.line, v.ed.pos, right-left)
	shown := box.h - 2
	v.inputScroll = min(max(v.inputScroll, crow-shown+1), crow)
	for i := range shown {
		if v.inputScroll+i < len(rows) {
			g.text(left, box.y+1+i, right, string(rows[v.inputScroll+i]), v.lk.input)
		}
	}
	cx, cy := left+ccol, box.y+1+crow-v.inputScroll
	if gh := v.ghost(); gh != "" {
		textIn(g, cx, cy, right, gh, v.lk.hint)
	}
	return cx, cy
}

// inputRows breaks what is typed into rows width columns wide, at its
// newlines and where a row is full, and tells the row and column of the
// cursor at rune pos.
func inputRows(line []rune, pos, width int) (rows [][]rune, crow, ccol int) {
	rows = [][]rune{nil}
	col := 0
	for i := 0; i <= len(line); i++ {
		if i == pos {
			if col >= width { // at the end of a full row: on the next
				rows = append(rows, nil)
				col = 0
			}
			crow, ccol = len(rows)-1, col
		}
		if i == len(line) {
			break
		}
		r := line[i]
		if r == '\n' {
			rows = append(rows, nil)
			col = 0
			continue
		}
		w := runewidth.RuneWidth(r)
		if col+w > width {
			rows = append(rows, nil)
			col = 0
		}
		rows[len(rows)-1] = append(rows[len(rows)-1], r)
		col += w
	}
	return rows, crow, ccol
}

// activityEvery is how often the dots of the assistant at work move: a few
// times a second is enough to look alive. A variable so tests can change
// it.
var activityEvery = 300 * time.Millisecond

// TaskView is a task as the panel shows it.
type TaskView struct {
	ID, Title string
	// Detail tells how it goes, e.g. "5/10 s", or what it waits for.
	Detail string
	// Percent is how far it is, 1-100, or 0 to show no bar.
	Percent int
	// Waiting is set while it waits for an answer.
	Waiting bool
}

// SetTasks shows tasks in the panel, with more below them, e.g. how many
// run in other conversations. The panel opens by itself when there are
// some, unless the user closed it.
func (s *Screen) SetTasks(tasks []TaskView, more string) {
	tasks = slices.Clone(tasks)
	s.do(func(v *view) { v.tasks, v.moreTasks = tasks, strings.TrimSpace(oneLine(more)) })
}

// panelMode is what the user asked of the panel.
type panelMode uint8

const (
	panelOn panelMode = iota // the default: open
	panelOff
)

// maxPanelNotices bounds the notices the panel keeps.
const maxPanelNotices = 8

// showsPanel reports whether the panel is wanted now.
func (v *view) showsPanel() bool { return v.panelMode == panelOn }

// remember keeps a notice for the panel, unless it is only information.
func (v *view) remember(n notice) {
	if n.kind == noteInfo {
		return
	}
	v.notices = append(v.notices, n)
	if len(v.notices) > maxPanelNotices {
		v.notices = slices.Delete(v.notices, 0, 1)
	}
}

// drawPanel draws the panel's cards: the tasks, and below them the last
// notices when there is room.
func (v *view) drawPanel(g *grid, r rect) {
	lk, t := v.lk, v.opts.Texts
	inner := rect{r.x + 1, r.y, r.w - 2, r.h}
	rows := v.taskRows(inner.w - 4)
	tasksH := min(len(rows)+2, inner.h)
	v.card(g, rect{inner.x, inner.y, inner.w, tasksH}, t.Tasks, rows)
	noticesY := inner.y + tasksH + 1
	if len(v.notices) == 0 || inner.y+inner.h-noticesY < 3 {
		return
	}
	var notes []cardRow
	for i := len(v.notices) - 1; i >= 0; i-- {
		n := v.notices[i]
		mark, st := "✓", lk.good
		switch n.kind {
		case noteWait:
			mark, st = "?", lk.wait
		case noteBad:
			mark, st = "✗", lk.bad
		}
		lines := wrapLines(n.text, inner.w-6)
		notes = append(notes, cardRow{mark: mark, markStyle: st, left: lines[0], leftStyle: lk.text})
		if len(lines) > 1 {
			notes = append(notes, cardRow{left: "  " + lines[1], leftStyle: lk.text})
		}
	}
	h := min(len(notes)+2, inner.y+inner.h-noticesY)
	v.card(g, rect{inner.x, noticesY, inner.w, h}, t.Notices, notes)
}

// cardRow is one row inside a card: a mark, text on the left, text on the
// right, or a bar.
type cardRow struct {
	mark      string
	markStyle styleID
	left      string
	leftStyle styleID
	right     string
	// isBar draws a bar instead, bar of the way done (0 to 1).
	isBar bool
	bar   float64
}

// taskRows are the rows the tasks take in a card w columns wide inside.
func (v *view) taskRows(w int) []cardRow {
	lk := v.lk
	if len(v.tasks) == 0 && v.moreTasks == "" {
		return []cardRow{{left: v.opts.Texts.NoTasks, leftStyle: lk.muted}}
	}
	var rows []cardRow
	for i, t := range v.tasks {
		if i > 0 {
			rows = append(rows, cardRow{})
		}
		mark, st := "●", lk.name
		if t.Waiting {
			mark, st = "?", lk.wait
		}
		right := ""
		if t.Percent > 0 {
			right = fmt.Sprintf("%d%%", min(t.Percent, 100))
		}
		// The title takes up to two rows, the percent on the first.
		title := wrapLines(t.Title, w-2-textWidth(right)-1)
		rows = append(rows, cardRow{mark: mark, markStyle: st, left: title[0], leftStyle: lk.text, right: right})
		if len(title) > 1 {
			rows = append(rows, cardRow{left: "  " + title[1], leftStyle: lk.text})
		}
		if t.Percent > 0 {
			rows = append(rows, cardRow{isBar: true, bar: float64(min(t.Percent, 100)) / 100})
		}
		if t.Detail != "" {
			detail := lk.muted
			if t.Waiting {
				detail = lk.wait
			}
			// Up to two rows, so a hint like "/answer t2" isn't cut.
			lines := wrapLines(t.ID+" · "+t.Detail, w)
			for _, l := range lines[:min(len(lines), 2)] {
				rows = append(rows, cardRow{left: l, leftStyle: detail})
			}
		}
	}
	if v.moreTasks != "" {
		rows = append(rows, cardRow{}, cardRow{left: v.moreTasks, leftStyle: lk.muted})
	}
	return rows
}

// card draws a rounded card on r, with title in its top edge and rows
// inside, as many as fit.
func (v *view) card(g *grid, r rect, title string, rows []cardRow) {
	lk := v.lk
	if r.w < 8 || r.h < 3 {
		return
	}
	border(g, r, lk.line)
	if title != "" {
		at := g.text(r.x+2, r.y, r.x+r.w-2, " ", lk.line)
		at = textIn(g, at, r.y, r.x+r.w-3, title, lk.cardTitle)
		g.text(at, r.y, r.x+r.w-2, " ", lk.line)
	}
	left, right := r.x+2, r.x+r.w-2
	for i, row := range rows {
		y := r.y + 1 + i
		if y >= r.y+r.h-1 {
			break
		}
		if row.isBar {
			thinBar(g, left+2, y, right-left-2, row.bar, lk)
			continue
		}
		x := left
		if row.mark != "" {
			x = g.text(x, y, right, row.mark, row.markStyle) + 1
		}
		end := right
		if row.right != "" {
			end = textRight(g, x, right, y, row.right, lk.muted) - 1
		}
		textIn(g, x, y, end, row.left, row.leftStyle)
	}
}

// thinBar draws a thin progress bar w cells wide from x on row y, done of
// the way (0 to 1).
func thinBar(g *grid, x, y, w int, done float64, lk looks) {
	n := int(min(max(done, 0), 1)*float64(w) + 0.5)
	for i := range w {
		if i < n {
			g.put(x+i, y, "━", 1, lk.name)
		} else {
			g.put(x+i, y, "─", 1, lk.line)
		}
	}
}

// selection is text marked with the mouse, from the cell where the drag
// started to where it is, in the conversation.
type selection struct {
	on, dragging   bool
	ax, ay, bx, by int
}

// ordered is the selection's start and end, in reading order.
func (s selection) ordered() (x0, y0, x1, y1 int) {
	if s.ay < s.by || s.ay == s.by && s.ax <= s.bx {
		return s.ax, s.ay, s.bx, s.by
	}
	return s.bx, s.by, s.ax, s.ay
}

// wheelRows is how many rows a turn of the wheel scrolls.
const wheelRows = 3

// copiedFor is how long the hints tell what was copied.
const copiedFor = 2 * time.Second

// mouse applies what the mouse did: the wheel scrolls, and a drag with the
// left button marks text in the conversation, copied when it ends.
func (v *view) mouse(m mouseEvent, t Terminal, s *Screen) {
	b := v.body
	inBody := m.x >= b.x && m.x < b.x+b.w && m.y >= b.y && m.y < b.y+b.h
	clamp := func(x, y int) (int, int) {
		return min(max(x, b.x), b.x+b.w-1), min(max(y, b.y), b.y+b.h-1)
	}
	switch {
	case m.button == wheelUp, m.button == wheelDown:
		rows := wheelRows
		if m.button == wheelDown {
			rows = -rows
		}
		v.convo.scrollBy(rows)
		v.sel = selection{} // what was marked moves away
	case m.button != 0:
	case m.release:
		if v.sel.dragging && v.sel.on {
			if text := v.selectedText(&v.fs.g); text != "" {
				copyText(t, text)
				v.copiedAt = time.Now()
				time.AfterFunc(copiedFor, func() { s.do(func(*view) {}) }) // to take the note away
			}
		}
		v.sel.dragging = false
	case m.drag:
		if v.sel.dragging {
			v.sel.bx, v.sel.by = clamp(m.x, m.y)
			v.sel.on = v.sel.bx != v.sel.ax || v.sel.by != v.sel.ay
		}
	case inBody:
		v.sel = selection{dragging: true, ax: m.x, ay: m.y, bx: m.x, by: m.y}
	default:
		v.sel = selection{}
	}
}

// selectedRange calls f for each row of the selection with the columns
// selected on it, [from, to).
func (v *view) selectedRange(f func(y, from, to int)) {
	x0, y0, x1, y1 := v.sel.ordered()
	left, right := v.body.x, v.body.x+v.body.w
	for y := y0; y <= y1; y++ {
		from, to := left, right
		if y == y0 {
			from = x0
		}
		if y == y1 {
			to = x1 + 1
		}
		f(y, from, to)
	}
}

// highlight draws the selection over what is on screen.
func (v *view) highlight(g *grid) {
	v.selectedRange(func(y, from, to int) {
		for x := from; x < min(to, g.W); x++ {
			c := *g.at(x, y)
			c.s = v.lk.selected
			g.set(x, y, c)
		}
	})
}

// boxRunes are the frames of the bubbles, left out of what is copied.
const boxRunes = "│╭╮╰╯─"

// selectedText is the text selected on g, row by row, without the frames
// of the bubbles and the blanks around them.
func (v *view) selectedText(g *grid) string {
	var rows []string
	v.selectedRange(func(y, from, to int) {
		var b strings.Builder
		for x := from; x < min(to, g.W); x++ {
			c := g.at(x, y)
			switch {
			case c.w == 0:
			case c.text == "":
				b.WriteByte(' ')
			default:
				b.WriteString(c.text)
			}
		}
		row := strings.Trim(b.String(), " "+boxRunes)
		if row != "" {
			rows = append(rows, row)
		}
	})
	return strings.Join(rows, "\n")
}

// shownAfter is how long an activity goes before it shows its time.
const shownAfter = 3 * time.Second

// since says d in whole seconds, and minutes past one: "12s", "2m 05s".
func since(d time.Duration) string {
	sec := int(d / time.Second)
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	return fmt.Sprintf("%dm %02ds", sec/60, sec%60)
}

// sizePoll is how often a terminal that doesn't tell when it changes size
// is asked for it.
const sizePoll = 500 * time.Millisecond
