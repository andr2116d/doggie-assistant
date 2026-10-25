// Package heartbeat periodically reports that DOGGIE is alive.
package heartbeat

import (
	"context"
	"log/slog"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

// Heartbeat publishes DOGGIE's uptime on the bus at a fixed interval.
type Heartbeat struct {
	conn  *bus.Conn
	log   *slog.Logger
	every time.Duration
	start time.Time
}

// New returns a Heartbeat that publishes every interval.
func New(conn *bus.Conn, log *slog.Logger, every time.Duration) *Heartbeat {
	return &Heartbeat{conn: conn, log: log, every: every, start: time.Now()}
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
			uptime := time.Since(h.start)
			if _, err := bus.Publish(h.conn, events.Heartbeat, events.HeartbeatData{UptimeS: int64(uptime.Seconds())}); err != nil {
				h.log.Warn("publish heartbeat", "err", err)
				continue
			}
			h.log.Debug("alive", "uptime", uptime.Round(time.Second))
		}
	}
}
