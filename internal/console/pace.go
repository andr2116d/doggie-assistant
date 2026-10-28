package console

import (
	"time"
	"unicode/utf8"
)

// Paced text moves on once every frameEvery, about 60 times a second, and
// the pace grows with what waits, so a burst is shown in about catchUp.
// Variables so tests can change them.
var (
	frameEvery = 16 * time.Millisecond
	catchUp    = 250 * time.Millisecond
)

const (
	// minStep is the fewest runes a frame shows, so the end of a burst
	// doesn't crawl.
	minStep = 2
	// maxBacklog is how many runes may wait: past it they show at once,
	// rather than leave the screen far behind.
	maxBacklog = 4096
)

// pacer holds paced text until it is shown.
type pacer struct {
	queue []Text // oldest first
	runes int    // runes in queue
}

func (p *pacer) add(t Text) {
	p.queue = append(p.queue, t)
	p.runes += utf8.RuneCountInString(t.Text)
}

func (p *pacer) empty() bool { return len(p.queue) == 0 }

// step takes what to show in this frame.
func (p *pacer) step() []Text {
	if p.runes > maxBacklog {
		return p.all()
	}
	n := max(minStep, int((int64(p.runes)*int64(frameEvery)+int64(catchUp)-1)/int64(catchUp)))
	return p.take(n)
}

// take takes up to n runes from the queue, each piece keeping its look.
// A text split in two leaves its gap with the first piece.
func (p *pacer) take(n int) []Text {
	var out []Text
	for len(p.queue) > 0 && (n > 0 || p.queue[0].Text == "") {
		t := p.queue[0]
		i, k := 0, 0
		for i < len(t.Text) && k < n {
			_, size := utf8.DecodeRuneInString(t.Text[i:])
			i += size
			k++
		}
		head := t
		head.Text = t.Text[:i]
		out = append(out, head)
		n -= k
		p.runes -= k
		if i == len(t.Text) {
			p.queue[0] = Text{}
			p.queue = p.queue[1:]
			continue
		}
		t.Text, t.Gap = t.Text[i:], false
		p.queue[0] = t
	}
	if len(p.queue) == 0 {
		p.queue = nil
	}
	return out
}

// all takes everything in the queue.
func (p *pacer) all() []Text {
	out := p.queue
	p.queue, p.runes = nil, 0
	return out
}

// animEvery is how often the activity line moves. A variable so tests can
// change it.
var animEvery = 80 * time.Millisecond

// clock ticks while started. Its channel is nil while stopped, so a select
// never takes it.
type clock struct {
	t *time.Ticker
	c <-chan time.Time
}

func newClock() *clock { return &clock{} }

// start makes it tick every d, unless it already ticks.
func (k *clock) start(d time.Duration) {
	if k.t == nil {
		k.t = time.NewTicker(d)
		k.c = k.t.C
	}
}

func (k *clock) stop() {
	if k.t != nil {
		k.t.Stop()
		k.t, k.c = nil, nil
	}
}
