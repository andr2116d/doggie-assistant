package console

// chrome is what the parts around the conversation show. Its texts come
// from the caller, in the user's language.
type chrome struct {
	name    string // the assistant's, e.g. "DOGGIE"
	online  bool
	status  string // e.g. "online" or "reconnecting…"
	channel string
	clock   string
	// hints are shown below the input box, on the left and on the right.
	hints, hintsRight string
	// tooSmall is shown alone when the screen can't hold the chat.
	tooSmall string
}

// drawChrome draws the parts around the conversation in their regions.
func drawChrome(g *grid, r regions, lk looks, c chrome) {
	if r.tooSmall {
		tooSmallNote(g, lk.muted, c.tooSmall)
		return
	}
	drawHeader(g, r.header, lk, c)
	if !r.rule.empty() {
		g.fill(r.rule.x, r.rule.y, r.rule.w, 1, lk.base)
		for x := r.rule.x; x < r.rule.x+r.rule.w; x++ {
			g.put(x, r.rule.y, "─", 1, lk.line)
		}
	}
	g.fill(r.body.x, r.body.y, r.body.w, r.body.h, lk.base)
	if !r.panel.empty() {
		g.fill(r.panel.x, r.panel.y, r.panel.w, r.panel.h, lk.base)
	}
	g.fill(r.input.x, r.input.y, r.input.w, r.input.h, lk.base)
	border(g, rect{r.input.x + 1, r.input.y, r.input.w - 2, r.input.h}, lk.box)
	g.text(r.input.x+3, r.input.y+1, r.input.x+r.input.w-2, "›", lk.prompt)
	drawFooter(g, r.footer, lk, c)
}

// drawHeader draws the top bar: the assistant's name and its connection on
// the left, the time on the right. Short of room, the channel goes first,
// then the time.
func drawHeader(g *grid, r rect, lk looks, c chrome) {
	g.fill(r.x, r.y, r.w, r.h, lk.header)
	left := []string{"◆ ", c.name, "   ● ", c.status}
	leftW := 0
	for _, s := range left {
		leftW += textWidth(s)
	}
	channel := ""
	if c.channel != "" {
		channel = " · " + c.channel
	}
	clock := c.clock
	room := r.w - 2
	if leftW+textWidth(channel)+textWidth(clock)+2 > room {
		channel = ""
	}
	if leftW+textWidth(clock)+2 > room {
		clock = ""
	}
	end := r.x + r.w - 1
	if clock != "" {
		end = textRight(g, r.x, end, r.y, clock, lk.headerMuted) - 2
	}
	state := lk.offline
	if c.online {
		state = lk.online
	}
	x := g.text(r.x+1, r.y, end, "◆ ", lk.brand)
	x = textIn(g, x, r.y, end, c.name, lk.brand)
	x = g.text(x, r.y, end, "   ● ", state)
	x = textIn(g, x, r.y, end, c.status, state)
	if channel != "" {
		textIn(g, x, r.y, end, channel, lk.headerMuted)
	}
}

// drawFooter draws the hints below the input box; short of room, the ones
// on the right go first.
func drawFooter(g *grid, r rect, lk looks, c chrome) {
	g.fill(r.x, r.y, r.w, r.h, lk.base)
	end := r.x + r.w - 2
	right := c.hintsRight
	if textWidth(c.hints)+textWidth(right)+6 > r.w {
		right = ""
	}
	if right != "" {
		end = textRight(g, r.x, end, r.y, right, lk.hint) - 2
	}
	textIn(g, r.x+2, r.y, end, c.hints, lk.hint)
}
