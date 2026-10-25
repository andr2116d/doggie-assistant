package bus

import (
	"log/slog"

	"github.com/nats-io/nats.go"
)

// Conn is a connection to the bus, owned by one component.
type Conn struct {
	nc     *nats.Conn
	source string
	log    *slog.Logger
}

func newConn(nc *nats.Conn, source string, log *slog.Logger) *Conn {
	return &Conn{nc: nc, source: source, log: log}
}

// Close flushes pending publishes and closes the connection.
func (c *Conn) Close() error {
	return c.nc.Drain()
}
