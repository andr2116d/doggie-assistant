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
	// tasks and notices title the panel's two parts.
	tasks, notices string
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
	g.fill(r.body.x, r.body.y, r.body.w, r.body.h, lk.base)
	if !r.panel.empty() {
		drawPanelFrame(g, r.panel, lk, c)
	}
	g.fill(r.input.x, r.input.y, r.input.w, r.input.h, lk.base)
	border(g, rect{r.input.x + 1, r.input.y, r.input.w - 2, r.input.h}, lk.box)
	g.text(r.input.x+3, r.input.y+1, r.input.x+r.input.w-2, "›", lk.prompt)
	drawFooter(g, r.footer, lk, c)
}

func drawHeader(g *grid, r rect, lk looks, c chrome) {
	g.fill(r.x, r.y, r.w, r.h, lk.header)
	end := r.x + r.w - 1
	clockAt := textRight(g, r.x+r.w/2, end, r.y, c.clock, lk.headerMuted)
	x := g.text(r.x+1, r.y, clockAt-1, "◆ ", lk.brand)
	x = textIn(g, x, r.y, clockAt-1, c.name, lk.brand)
	state := lk.offline
	if c.online {
		state = lk.online
	}
	x = g.text(x, r.y, clockAt-1, "   ● ", state)
	x = textIn(g, x, r.y, clockAt-1, c.status, state)
	if c.channel != "" {
		x = g.text(x, r.y, clockAt-1, " · ", lk.headerMuted)
		textIn(g, x, r.y, clockAt-1, c.channel, lk.headerMuted)
	}
}

func drawPanelFrame(g *grid, r rect, lk looks, c chrome) {
	g.fill(r.x, r.y, r.w, r.h, lk.panel)
	for y := r.y; y < r.y+r.h; y++ {
		g.put(r.x, y, "│", 1, lk.rule)
	}
	textIn(g, r.x+2, r.y+1, r.x+r.w-1, c.tasks, lk.panelTitle)
}

func drawFooter(g *grid, r rect, lk looks, c chrome) {
	g.fill(r.x, r.y, r.w, r.h, lk.base)
	end := r.x + r.w - 2
	rightAt := end
	if c.hintsRight != "" {
		rightAt = textRight(g, r.x+r.w/2, end, r.y, c.hintsRight, lk.hint) - 2
	}
	textIn(g, r.x+2, r.y, rightAt, c.hints, lk.hint)
}
