package console

import (
	"strings"
	"unicode/utf8"

	"github.com/clipperhouse/uax29/v2/graphemes"
)

// sender is who a message comes from.
type sender uint8

const (
	fromUser      sender = iota // said here
	fromAssistant               // the assistant's reply
	fromElsewhere               // said in this conversation on another channel
	fromChat                    // the chat's own note
)

// noteKind is what a notice tells, which picks its mark and color.
type noteKind uint8

const (
	noteInfo noteKind = iota
	noteGood
	noteWait
	noteBad
)

// notice is a line about something, e.g. a task, under the message that
// caused it or on its own.
type notice struct {
	kind noteKind
	text string
}

// message is one block of the conversation: a bubble, or a note.
type message struct {
	id    string
	from  sender
	label string // who said it, for fromElsewhere
	kind  noteKind
	text  string // all of it so far
	shown int    // bytes of text on screen; fewer while it is paced in
	open  bool   // still being written
	notes []notice
	wrap  wrapCache
	buf   strings.Builder // holds text, to grow it in place
}

// wrapCache keeps a message's rows at one width.
type wrapCache struct {
	width int
	// from is where the paragraph being wrapped starts, in bytes of text,
	// and code whether it is in a block of code. tail is where its last
	// row starts in it as it shows, past rows that can't change as text
	// grows, kept with those of the paragraphs before in stable. wrapped
	// is set when that row began at a break.
	from    int
	code    bool
	tail    int
	wrapped bool
	stable  []textRow
	stableW int // the widest of stable, in columns
	// paraText is how the paragraph being wrapped showed, and paraRows how
	// many of its rows are in stable. Unless it shows now as it did with
	// more at its end, it is wrapped again whole: a mark may turn a line
	// into a heading, change its rows' indent (paraHang), or hide a * that
	// showed; and a cluster may now go across tail.
	paraText string
	paraHang int
	paraRows int
	rows     []textRow // stable and the rows from tail, for text[:shown]
	w        int       // the widest of rows
	shown    int       // what rows covers; -1 when stale
}

// lines are the message's shown text wrapped to width, as markdown for a
// reply. As text comes in only its last row is wrapped again.
func (m *message) lines(width int) []textRow {
	width = max(width, 2)
	c := &m.wrap
	if c.width == width && c.shown == m.shown {
		return c.rows
	}
	if c.width != width || c.shown < 0 || m.shown < c.from+c.tail {
		*c = wrapCache{width: width, stable: c.stable[:0]}
	}
	text := m.text[:m.shown]
	known := len(c.stable)
	wrap := func(para string) (int, bool) {
		p := mdPara{text: para}
		if m.from == fromAssistant {
			p = markdown(para, c.code)
		}
		if p.fence { // it doesn't show: neither do rows it had before
			c.stable = c.stable[:len(c.stable)-c.paraRows]
			c.tail, c.wrapped, c.paraRows, c.paraText = 0, false, 0, ""
			c.stableW = -1
			return 0, true
		}
		if !strings.HasPrefix(p.text, c.paraText) || !boundaryAt(p.text, c.tail) || p.hang != c.paraHang {
			c.stable = c.stable[:len(c.stable)-c.paraRows]
			c.tail, c.wrapped, c.paraRows = 0, false, 0
			c.stableW = -1 // to measure again
		}
		c.paraText, c.paraHang = p.text, p.hang
		before := len(c.stable)
		var last int
		c.stable, last = wrapRows(c.stable, p.text[c.tail:], tailAttrs(p.attrs, c.tail), width, c.wrapped, p.hang)
		c.paraRows += len(c.stable) - before
		return c.tail + last, false
	}
	for {
		i := strings.IndexByte(text[c.from:], '\n')
		if i < 0 {
			break
		}
		if _, fence := wrap(text[c.from : c.from+i]); fence {
			c.code = !c.code
		}
		c.from, c.tail, c.wrapped, c.paraRows, c.paraText, c.paraHang = c.from+i+1, 0, false, 0, "", 0
	}
	last, fence := wrap(text[c.from:])
	all := c.stable
	if fence {
		c.stable = all // a fence being typed adds no row
	} else {
		c.stable = all[:len(all)-1]
		c.paraRows-- // its last row isn't kept
		if last > c.tail {
			c.tail, c.wrapped = last, true
		}
	}
	if len(all) == 0 {
		all = []textRow{{}}
	}
	c.rows = all
	if c.stableW < 0 || known > len(c.stable) {
		c.stableW, known = 0, 0
	}
	for _, r := range c.stable[known:] {
		c.stableW = max(c.stableW, textWidth(r.text))
	}
	c.w = max(c.stableW, textWidth(all[len(all)-1].text))
	c.shown = m.shown
	return c.rows
}

// tailAttrs is attrs from i on, or nil for no attributes.
func tailAttrs(attrs []mdAttr, i int) []mdAttr {
	if attrs == nil {
		return nil
	}
	return attrs[min(i, len(attrs)):]
}

// setText replaces the text, cleaned of what mustn't reach the terminal.
// Text that only grows keeps what is wrapped and what is on screen.
func (m *message) setText(text string, paced bool) {
	text = sanitize(text, true)
	if !strings.HasPrefix(text, m.text) {
		m.wrap.shown = -1
		m.shown = min(m.shown, commonPrefix(m.text, text))
	}
	m.buf.Reset()
	m.buf.WriteString(text)
	m.text = m.buf.String()
	if !paced {
		m.shown = len(text)
	}
}

// appendText adds text, cleaned, to the end. The text grows in place, so a
// reply that comes in many pieces isn't copied whole for each.
func (m *message) appendText(text string, paced bool) {
	if m.buf.Len() != len(m.text) { // text was set without buf
		m.buf.Reset()
		m.buf.WriteString(m.text)
	}
	m.buf.WriteString(sanitize(text, true))
	m.text = m.buf.String()
	if !paced {
		m.shown = len(m.text)
	}
}

// linesWidth is how wide the widest of the message's lines at width is.
func (m *message) linesWidth(width int) int {
	m.lines(width)
	return m.wrap.w
}

// commonPrefix is how many bytes a and b start with alike, whole runes.
func commonPrefix(a, b string) int {
	i := 0
	for i < len(a) && i < len(b) {
		r, n := utf8.DecodeRuneInString(a[i:])
		if r2, n2 := utf8.DecodeRuneInString(b[i:]); r2 != r || n2 != n {
			break
		}
		i += n
	}
	return i
}

// maxMessages bounds the messages kept: past it the oldest go, and are
// asked for again when scrolled back to.
const maxMessages = 500

// conversation is the messages of the chat and the view on them.
type conversation struct {
	name string // the assistant's, above its bubbles
	msgs []*message
	byID map[string]*message
	// scroll is how many rows the view is above the newest; 0 follows
	// what comes. When scrolled, anchor is the message at the view's
	// bottom and anchorRows how many of its rows are below the view, so
	// the view stays put while messages come, go or rewrap.
	scroll     int
	anchor     *message
	anchorRows int
	unseen     int // messages that came while scrolled up
	// activity, if set, shows the assistant at work below the newest
	// message, with dots moving by phase.
	activity string
	phase    int
	// top is set when the view reached the oldest message kept.
	top     bool
	heights []int // of each message, reused by draw
}

func newConversation(name string) *conversation {
	return &conversation{name: name, byID: map[string]*message{}}
}

// add appends m, dropping the oldest message past maxMessages.
func (c *conversation) add(m *message) {
	c.msgs = append(c.msgs, m)
	if m.id != "" {
		c.byID[m.id] = m
	}
	if c.scroll > 0 {
		c.unseen++
	}
	if len(c.msgs) > maxMessages {
		old := c.msgs[0]
		c.msgs[0] = nil
		c.msgs = c.msgs[1:]
		if old.id != "" && c.byID[old.id] == old {
			delete(c.byID, old.id)
		}
		if c.anchor == old {
			c.anchor = nil
		}
	}
}

// say adds what someone said, or a note, shown whole at once.
func (c *conversation) say(from sender, label, text string, kind noteKind) {
	m := &message{from: from, label: label, kind: kind}
	m.setText(text, false)
	c.add(m)
}

// stream adds text to the assistant's reply id, starting it if new. The
// text is paced in.
// Whitespace starting a reply is dropped.
func (c *conversation) stream(id, text string) {
	m := c.byID[id]
	if m == nil || m.text == "" {
		text = strings.TrimLeft(text, " \t\r\n")
		if text == "" {
			return
		}
	}
	if m == nil {
		m = &message{id: id, from: fromAssistant, open: true}
		c.add(m)
	}
	m.appendText(text, true)
}

// finish ends the reply id with its whole text, which corrects what
// came in pieces. A reply never streamed shows at once.
func (c *conversation) finish(id, text string) {
	text = strings.TrimSpace(text)
	m := c.byID[id]
	if m == nil {
		m = &message{id: id, from: fromAssistant}
		c.add(m)
		m.setText(text, false)
		return
	}
	m.setText(text, true)
	m.open = false
}

// noteOn adds a notice under the message id, or on its own if there is
// no such message.
func (c *conversation) noteOn(id string, n notice) {
	if m := c.byID[id]; m != nil && id != "" {
		m.notes = append(m.notes, n)
		return
	}
	c.say(fromChat, "", n.text, n.kind)
}

// older puts messages from before the oldest one kept in front, as long
// as fewer than maxMessages are kept.
func (c *conversation) older(msgs []*message) {
	room := maxMessages - len(c.msgs)
	if room <= 0 || len(msgs) == 0 {
		return
	}
	msgs = msgs[max(len(msgs)-room, 0):]
	for _, m := range msgs {
		if m.id != "" {
			c.byID[m.id] = m
		}
		m.setText(m.text, false)
	}
	c.msgs = append(append([]*message(nil), msgs...), c.msgs...)
	c.top = false
}

// pace shows more of the replies paced in, and reports whether any still
// has more to show.
func (c *conversation) pace() bool {
	more := false
	for _, m := range c.msgs {
		if m.shown >= len(m.text) {
			continue
		}
		rest := m.text[m.shown:]
		n := utf8.RuneCountInString(rest)
		step := max(minStep, (n*int(frameEvery)+int(catchUp)-1)/int(catchUp))
		if n > maxBacklog {
			step = n
		}
		for i := 0; i < step && m.shown < len(m.text); i++ {
			_, size := utf8.DecodeRuneInString(m.text[m.shown:])
			m.shown += size
		}
		more = more || m.shown < len(m.text)
	}
	return more
}

// scrollBy moves the view up by rows, down if negative.
func (c *conversation) scrollBy(rows int) {
	c.scroll = max(c.scroll+rows, 0)
	c.anchor = nil
	if c.scroll == 0 {
		c.unseen = 0
	}
}

// Measures of the conversation, in columns.
const (
	maxReplyCols = 76 // text of a reply, for reading comfort
	maxSaidCols  = 64 // text of what someone said
)

// widths are how wide the text of each kind of bubble may be in a
// conversation cols wide.
func widths(cols int) (reply, said int) {
	return min(cols-4, maxReplyCols), min(cols*3/4, maxSaidCols) - 4
}

// named reports whether message i shows the assistant's name above it:
// when it starts a run of the assistant's bubbles.
func (c *conversation) named(i int) bool {
	return c.msgs[i].from == fromAssistant && (i == 0 || c.msgs[i-1].from != fromAssistant)
}

// height is how many rows message i takes in a conversation cols wide.
func (c *conversation) height(i, cols int) int {
	m := c.msgs[i]
	reply, said := widths(cols)
	switch m.from {
	case fromAssistant:
		h := len(m.lines(reply)) + 2 + len(m.notes)
		if c.named(i) {
			h++
		}
		return h
	case fromElsewhere:
		return len(m.lines(said)) + 3
	case fromUser:
		return len(m.lines(said)) + 2
	default:
		return len(m.lines(cols - 2))
	}
}

// draw draws the view of the conversation in body. newer tells how many
// messages came while scrolled up, e.g. "↓ 3 new".
func (c *conversation) draw(g *grid, body rect, lk looks, newer func(n int) string) {
	cols := body.w - 2*conversationX
	if cols < 8 || body.h < 1 {
		return
	}
	c.heights = c.heights[:0]
	total := 0
	for i := range c.msgs {
		h := c.height(i, cols)
		c.heights = append(c.heights, h)
		total += h + 1 // and the gap above it
	}
	heights := c.heights
	act := 0 // rows of the activity, below the newest message
	if c.activity != "" {
		act = activityRows + 1
	}
	total += act
	if c.scroll > 0 && c.anchor != nil {
		below, found := 0, false
		for i := len(c.msgs) - 1; i >= 0; i-- {
			if c.msgs[i] == c.anchor {
				found = true
				break
			}
			below += heights[i] + 1
		}
		if found {
			c.scroll = below + act + c.anchorRows
		}
	}
	c.scroll = min(c.scroll, max(total-body.h, 0))
	if c.scroll == 0 {
		c.unseen = 0
	}
	viewBottom := body.y + body.h
	bottom := viewBottom + c.scroll // where the newest message ends
	if act > 0 {
		top := bottom - activityRows
		if top < viewBottom {
			c.drawActivity(g, body.x+conversationX, top, cols, body, lk)
		}
		bottom = top - 1
	}
	c.anchor, c.top = nil, true
	for i := len(c.msgs) - 1; i >= 0; i-- {
		top := bottom - heights[i]
		if top < viewBottom && c.anchor == nil && c.scroll > 0 {
			c.anchor, c.anchorRows = c.msgs[i], max(bottom-viewBottom, 0)
		}
		if top < viewBottom {
			c.drawMessage(g, i, body.x+conversationX, top, cols, body, lk)
		}
		bottom = top - 1
		if bottom <= body.y {
			c.top = i == 0
			break
		}
	}
	if c.scroll > 0 && c.unseen > 0 && newer != nil {
		pill := " " + newer(c.unseen) + " "
		w := textWidth(pill)
		textIn(g, centered(body, w), body.y+body.h-1, body.x+body.w, pill, lk.pill)
	}
}

// drawMessage draws message i from row top, x and cols wide, showing only
// the rows within clip.
func (c *conversation) drawMessage(g *grid, i, x, top, cols int, clip rect, lk looks) {
	m := c.msgs[i]
	visible := func(y int) bool { return y >= clip.y && y < clip.y+clip.h }
	reply, said := widths(cols)
	y := top
	switch m.from {
	case fromAssistant:
		if c.named(i) {
			if visible(y) {
				textIn(g, x, y, x+cols, c.name, lk.name)
			}
			y++
		}
		lines := m.lines(reply)
		w := max(m.linesWidth(reply), 1) + 4 // the borders and a space inside each
		if visible(y) {
			hline(g, x, y, w, "╭", "╮", lk.border)
		}
		y++
		for _, l := range lines {
			if visible(y) {
				g.text(x, y, x+1, "│", lk.border)
				drawRow(g, x+2, y, x+w-2, l, lk)
				g.text(x+w-1, y, x+w, "│", lk.border)
			}
			y++
		}
		if visible(y) {
			hline(g, x, y, w, "╰", "╯", lk.border)
		}
		y++
		for _, n := range m.notes {
			if visible(y) {
				drawNotice(g, x+2, y, x+cols, n, lk)
			}
			y++
		}
	case fromUser, fromElsewhere:
		lines := m.lines(said)
		w := m.linesWidth(said)
		left := x + cols - (w + 4)
		if m.from == fromElsewhere {
			if visible(y) {
				textRight(g, x, x+cols, y, m.label, lk.muted)
			}
			y++
		}
		if visible(y) {
			hline(g, left, y, w+4, "╭", "╮", lk.youEdge)
		}
		y++
		for _, l := range lines {
			if visible(y) {
				g.text(left, y, left+1, "│", lk.youEdge)
				g.fill(left+1, y, w+2, 1, lk.you)
				g.text(left+2, y, left+2+w, l.text, lk.you)
				g.text(left+w+3, y, left+w+4, "│", lk.youEdge)
			}
			y++
		}
		if visible(y) {
			hline(g, left, y, w+4, "╰", "╯", lk.youEdge)
		}
		y++
	default:
		for j, l := range m.lines(cols - 2) {
			if visible(y) {
				if j == 0 {
					drawNotice(g, x, y, x+cols, notice{m.kind, l.text}, lk)
				} else {
					textIn(g, x+2, y, x+cols, l.text, lk.muted)
				}
			}
			y++
		}
	}
}

// drawNotice draws a notice's mark and its text on one row.
func drawNotice(g *grid, x, y, limit int, n notice, lk looks) {
	mark, st := "·", lk.muted
	switch n.kind {
	case noteGood:
		mark, st = "✓", lk.good
	case noteWait:
		mark, st = "?", lk.wait
	case noteBad:
		mark, st = "✗", lk.bad
	}
	x = g.text(x, y, limit, mark, st)
	textIn(g, x+1, y, limit, n.text, lk.muted)
}

// activityRows is how tall the assistant at work shows.
const activityRows = 3

// drawActivity draws the assistant at work from row top: a bubble of
// dots, one lit at a time, and what it is doing beside it.
func (c *conversation) drawActivity(g *grid, x, top, cols int, clip rect, lk looks) {
	visible := func(y int) bool { return y >= clip.y && y < clip.y+clip.h }
	if visible(top) {
		hline(g, x, top, 7, "╭", "╮", lk.border)
	}
	if y := top + 1; visible(y) {
		g.text(x, y, x+1, "│", lk.border)
		for i := range 3 {
			st := lk.border
			if i == c.phase%3 {
				st = lk.name
			}
			g.put(x+2+i, y, "•", 1, st)
		}
		g.text(x+6, y, x+7, "│", lk.border)
		textIn(g, x+9, y, x+cols, c.activity, lk.muted)
	}
	if y := top + 2; visible(y) {
		hline(g, x, y, 7, "╰", "╯", lk.border)
	}
}

// drawRow writes a row of a reply from x up to limit, each run of it in
// the look of its markdown attribute.
func drawRow(g *grid, x, y, limit int, r textRow, lk looks) {
	if r.attrs == nil {
		g.text(x, y, limit, r.text, lk.text)
		return
	}
	for i := 0; i < len(r.text) && x < limit; {
		a := r.attrs[i]
		j := i + 1
		for j < len(r.text) && r.attrs[j] == a {
			j++
		}
		x = g.text(x, y, limit, r.text[i:j], lk.forAttr(a))
		i = j
	}
}

// boundaryAt reports whether a grapheme cluster starts at byte i of s:
// text added after a rune may join its cluster, as a combining mark does.
func boundaryAt(s string, i int) bool {
	if i <= 0 || i >= len(s) {
		return true
	}
	_, n := utf8.DecodeLastRuneInString(s[:i])
	it := graphemes.FromString(s[i-n:])
	it.Next()
	return len(it.Value()) <= n
}
