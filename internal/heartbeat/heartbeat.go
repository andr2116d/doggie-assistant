// Package heartbeat periodically reports that DOGGIE is alive.
package heartbeat

import (
	"context"
	"log/slog"
	"time"
)

// Heartbeat logs DOGGIE's uptime at a fixed interval.
type Heartbeat struct {
	log   *slog.Logger
	every time.Duration
	start time.Time
}

// New returns a Heartbeat that reports every interval.
func New(log *slog.Logger, every time.Duration) *Heartbeat {
	return &Heartbeat{log: log, every: every, start: time.Now()}
}

// Name implements supervisor.Component.
func (h *Heartbeat) Name() string { return "heartbeat" }

// Run implements supervisor.Component.
func (h *Heartbeat) Run(ctx context.Context) error {
	t := time.NewTicker(h.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			h.log.Info("alive", "uptime", time.Since(h.start).Round(time.Second))
		}
	}
}
