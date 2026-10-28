package console

import (
	"os"
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
	Tasks, Notices    string // the panel's two parts
	TooSmall          string // alone on a screen too small for the chat
	// Newer tells how many messages came while scrolled up.
	Newer func(n int) string
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
	s.do(func(v *view) { v.convo.noteOn(id, notice{kind, text}) })
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
	opts   ScreenOptions
	fs     *fullScreen
	lk     looks
	convo  *conversation
	ed     *editor
	online bool
	status string
	panel  bool // the user wants the panel
	pacing bool // a reply may have text to pace in
	// activity is what the assistant does, since activitySince; taskStatus
	// replaces the hints while set.
	activity      string
	activitySince time.Time
	taskStatus    string
	// unfocused is set while the window is in the background, where
	// nothing needs to move.
	unfocused bool
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
	v.ed.multiline = true
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
	dec := decoder{full: true}
	pace, anim := newClock(), newClock()
	defer pace.stop()
	defer anim.stop()
	defer fs.gate.stop()
	minute := time.NewTimer(untilNextMinute(time.Now()))
	defer minute.Stop()
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
		if v.activity != "" && !v.unfocused {
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
				v.key(k, &q)
			}
			want()
		case op := <-s.ops:
			op(v)
			want()
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
		if g := v.ghost(); g != "" {
			v.ed.apply(key{kind: keyPaste, text: g})
		} else {
			v.panel = !v.panel
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
	t := v.opts.Texts
	width := g.W - 8 // the box's frame, margins, prompt and padding
	rows, _, _ := inputRows(v.ed.line, v.ed.pos, max(width, 1))
	r := layout(g.W, g.H, v.panel, len(rows))
	c := chrome{
		name: v.opts.Name, online: v.online, status: v.status, channel: t.Channel,
		hints: t.Hints, hintsRight: t.HintsRight, tasks: t.Tasks, notices: t.Notices, tooSmall: t.TooSmall,
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
	if v.taskStatus != "" {
		c.hints = v.taskStatus
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
