// Package demo provides example task kinds to try the task system end to
// end before real workers exist:
//
//   - demo.sleep {"seconds": N}: waits N seconds (default 5), reporting
//     progress every second.
//   - demo.ask {"question": "...", "options": [...]}: asks a question and
//     returns the answer.
package demo

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/task/worker"
)

// maxSleep bounds demo.sleep.
const maxSleep = time.Hour

// Demo is a worker component serving the demo task kinds.
type Demo struct {
	conn *bus.Conn
	log  *slog.Logger
	tick time.Duration // one "second" of demo.sleep; shortened in tests
}

// New returns the demo worker.
func New(conn *bus.Conn, log *slog.Logger) *Demo {
	return &Demo{conn: conn, log: log, tick: time.Second}
}

// Name implements supervisor.Component.
func (d *Demo) Name() string { return "demo" }

// Run implements supervisor.Component.
func (d *Demo) Run(ctx context.Context) error {
	return worker.Serve(ctx, d.conn, d.log, d.funcs())
}

func (d *Demo) funcs() map[string]worker.Func {
	return map[string]worker.Func{
		"demo.sleep": d.sleep,
		"demo.ask":   ask,
	}
}

func (d *Demo) sleep(ctx context.Context, t *worker.Task) (any, error) {
	in := struct {
		Seconds int `json:"seconds"`
	}{Seconds: 5}
	if err := t.Input(&in); err != nil {
		return nil, err
	}
	// Compare in seconds: converting a huge value to a Duration overflows.
	if in.Seconds < 1 || in.Seconds > int(maxSleep/time.Second) {
		return nil, fmt.Errorf("seconds must be between 1 and %d", int(maxSleep.Seconds()))
	}
	for i := 1; i <= in.Seconds; i++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(d.tick):
		}
		if err := t.Progress(ctx, fmt.Sprintf("%d/%d s", i, in.Seconds), i*100/in.Seconds); err != nil {
			return nil, err
		}
	}
	return map[string]int{"slept_s": in.Seconds}, nil
}

func ask(ctx context.Context, t *worker.Task) (any, error) {
	in := struct {
		Question string   `json:"question"`
		Options  []string `json:"options"`
	}{Question: "What should I do?"}
	if err := t.Input(&in); err != nil {
		return nil, err
	}
	answer, err := t.Ask(ctx, in.Question, in.Options...)
	if err != nil {
		return nil, err
	}
	return map[string]string{"answer": answer}, nil
}
