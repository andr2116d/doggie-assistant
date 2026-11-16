// Package timer provides the timer task kind:
//
//	timer {"seconds": N, "label": "..."}
//
// counts N seconds from the moment the task was asked for, and ends. The
// task tells when it will end, so whoever shows it counts down alone.
package timer

import (
	"context"
	"fmt"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/task/worker"
)

// clockCheck is how often a timer looks at the clock. It doesn't sleep
// until its end in one go: a wait doesn't advance while the machine
// sleeps, and the timer would end late by as long as it slept.
const clockCheck = time.Second

// Timer is a worker component serving the timer task kind.
type Timer struct {
	conn *bus.Conn
	log  *slog.Logger
	// Shortened or replaced in tests.
	second time.Duration // one second of a timer
	check  time.Duration
	now    func() time.Time
}

// New returns the timer worker.
func New(conn *bus.Conn, log *slog.Logger) *Timer {
	return &Timer{conn: conn, log: log, second: time.Second, check: clockCheck, now: time.Now}
}

// Name implements supervisor.Component.
func (t *Timer) Name() string { return "timer" }

// Run implements supervisor.Component.
func (t *Timer) Run(ctx context.Context) error {
	return worker.Serve(ctx, t.conn, t.log, map[string]worker.Func{events.KindTimer.Name: t.count})
}

// Result is what a timer that ended tells.
type Result struct {
	Seconds int    `json:"seconds"`
	Label   string `json:"label,omitempty"`
}

// count runs one timer: it waits until the task's time is over.
func (t *Timer) count(ctx context.Context, task *worker.Task) (any, error) {
	var in Result
	if err := task.Input(&in); err != nil {
		return nil, err
	}
	// Compare in seconds: converting a huge value to a Duration overflows.
	if max := int(events.MaxTimer / time.Second); in.Seconds < 1 || in.Seconds > max {
		return nil, fmt.Errorf("seconds must be between 1 and %d", max)
	}
	if utf8.RuneCountInString(in.Label) > events.MaxTimerLabel {
		return nil, fmt.Errorf("label must be at most %d characters", events.MaxTimerLabel)
	}
	// From when it was asked for, not from when the worker got it.
	ends := task.CreatedAt.Add(time.Duration(in.Seconds) * t.second)
	if err := task.Expect(ctx, "", ends); err != nil {
		return nil, err
	}
	wait := time.NewTimer(0)
	defer wait.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wait.C:
		}
		left := ends.Sub(t.now())
		if left <= 0 {
			return in, nil
		}
		wait.Reset(min(left, t.check))
	}
}
