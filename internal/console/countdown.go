package console

import (
	"fmt"
	"time"
)

// counting reports whether t tells when it ends: the panel counts down to
// it instead of showing a percent.
func (t TaskView) counting() bool { return !t.EndsAt.IsZero() && !t.Waiting }

// left is how long t has left at now, as the panel says it, and how far
// along it is (0-1), or -1 if it doesn't say since when it counts.
func (t TaskView) left(now time.Time) (text string, done float64) {
	left := max(t.EndsAt.Sub(now), 0)
	done = -1
	if total := t.EndsAt.Sub(t.Since); !t.Since.IsZero() && total > 0 {
		done = min(max(1-float64(left)/float64(total), 0), 1)
	}
	return countdown(left), done
}

// countdown says d as a clock counting down, "1:05" or "2:00:00". Part of
// a second counts as a whole one, so it reaches 0:00 when the time is over
// and not a second before.
func countdown(d time.Duration) string {
	s := int64((d + time.Second - 1) / time.Second)
	if h := s / 3600; h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

// nextCount is how long until a task's countdown says something else, or
// false if none counts at now.
func nextCount(tasks []TaskView, now time.Time) (time.Duration, bool) {
	next, any := time.Duration(0), false
	for _, t := range tasks {
		left := t.EndsAt.Sub(now)
		if !t.counting() || left <= 0 {
			continue
		}
		// The second it shows ends when what is left is whole again.
		change := left % time.Second
		if change == 0 {
			change = time.Second
		}
		if !any || change < next {
			next, any = change, true
		}
	}
	return next, any
}
