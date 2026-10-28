package console

// fullScreen draws a view that fills a terminal, a frame at a time, and
// puts the terminal back as it was when it is done.
type fullScreen struct {
	t       Terminal
	styles  *styleTable
	painter *painter
	g       grid
	gate    frameGate
	entered bool
}

func newFullScreen(t Terminal, p Profile) *fullScreen {
	styles := newStyleTable()
	return &fullScreen{t: t, styles: styles, painter: newPainter(styles, p)}
}

// enter switches the terminal to the full screen.
func (f *fullScreen) enter() error {
	if _, err := f.t.Write([]byte(enterFull)); err != nil {
		return err
	}
	f.entered = true
	f.painter.invalidate()
	f.fit()
	return nil
}

// leave puts the terminal back as it was before enter, and does nothing
// if it already is.
func (f *fullScreen) leave() {
	if f.entered {
		f.t.Write([]byte(leaveFull))
		f.entered = false
	}
}

// fit sizes the grid to the terminal, and reports whether the size
// changed; then the next frame draws everything. A terminal that can't
// tell its size keeps the last one, or 80×24.
func (f *fullScreen) fit() bool {
	w, werr := f.t.Width()
	h, herr := f.t.Height()
	if werr != nil || herr != nil || w <= 0 || h <= 0 {
		w, h = f.g.W, f.g.H
		if w == 0 || h == 0 {
			w, h = 80, 24
		}
	}
	if w == f.g.W && h == f.g.H {
		return false
	}
	f.g.resize(w, h)
	f.painter.invalidate()
	return true
}

// draw has paint fill the grid, and writes what changed. paint returns
// where the cursor goes, and whether it shows.
func (f *fullScreen) draw(paint func(g *grid) (x, y int, show bool)) error {
	x, y, show := paint(&f.g)
	if out := f.painter.frame(&f.g, x, y, show); out != nil {
		if _, err := f.t.Write(out); err != nil {
			return err
		}
	}
	return nil
}

// guard runs fn, and if it panics puts the terminal back before the panic
// goes on, so its report shows on a usable terminal.
func (f *fullScreen) guard(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			f.leave()
			f.t.Restore()
			panic(r)
		}
	}()
	fn()
}
