package brain

import (
	"slices"
	"sync"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/brain/llm"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

// lowUsage is the share of a usage window spent from which DOGGIE warns the
// owner, once per window.
const lowUsage = 0.9

// usageWindows are how long each usage window lasts, by name. The length
// stands in for when a window renews if the provider doesn't say.
var usageWindows = map[string]time.Duration{
	"five_hour": 5 * time.Hour,
	"seven_day": 7 * 24 * time.Hour,
}

// usageWarnings remembers until when each usage window was already warned
// about. Turns run concurrently, so it has its own lock.
type usageWarnings struct {
	mu    sync.Mutex
	until map[string]time.Time
}

// claim returns the windows of u that are running low and weren't warned
// about yet, and marks them warned. The one that renews last comes first:
// it is the one that matters to the owner.
func (w *usageWarnings) claim(u *llm.Usage, now time.Time) []events.UsageWindow {
	if u == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []events.UsageWindow
	for _, win := range []struct {
		name   string
		spent  float64
		renews time.Time
	}{
		{"seven_day", u.SevenDay, u.SevenDayRenews},
		{"five_hour", u.FiveHour, u.FiveHourRenews},
	} {
		if win.spent < lowUsage || now.Before(w.until[win.name]) {
			continue
		}
		w.mark(win.name, win.renews, now)
		out = append(out, events.UsageWindow{Window: win.name, Renews: win.renews})
	}
	slices.SortStableFunc(out, func(a, b events.UsageWindow) int { return w.until[b.Window].Compare(w.until[a.Window]) })
	return out
}

// mark records that window was warned about at the time at, until it renews.
func (w *usageWarnings) mark(window string, renews, at time.Time) {
	length, ok := usageWindows[window]
	if !ok {
		return
	}
	// A renewal time that is missing or makes no sense counts as a whole
	// window from the warning.
	if renews.IsZero() || !renews.After(at) || renews.Sub(at) > length {
		renews = at.Add(length)
	}
	if w.until == nil {
		w.until = make(map[string]time.Time)
	}
	if renews.After(w.until[window]) {
		w.until[window] = renews
	}
}

// recall marks the warnings a logged turn gave, at the time it was logged.
func (w *usageWarnings) recall(rec events.BrainTurnData, at time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, win := range rec.LowUsage {
		w.mark(win.Window, win.Renews, at)
	}
}
