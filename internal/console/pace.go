package console

import "time"

// Text paced in moves on once every frameEvery, about 60 times a second,
// and the pace grows with what waits, so a burst is shown in about catchUp.
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
