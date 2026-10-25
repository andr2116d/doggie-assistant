package bus

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// drainTimeout bounds how long Close waits for pending messages to flush.
const drainTimeout = 5 * time.Second

// Conn is a connection to the bus, owned by one component.
type Conn struct {
	nc     *nats.Conn
	js     jetstream.JetStream
	source string
	log    *slog.Logger
	closed chan struct{}
}

func newConn(nc *nats.Conn, source string, log *slog.Logger) (*Conn, error) {
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("bus: jetstream: %w", err)
	}
	c := &Conn{nc: nc, js: js, source: source, log: log, closed: make(chan struct{})}
	nc.SetClosedHandler(func(*nats.Conn) { close(c.closed) })
	return c, nil
}

// Name returns the connection name, which is the source of its events.
func (c *Conn) Name() string { return c.source }

// Close flushes pending publishes, waits until they reach the server and
// closes the connection. Calling it again is a no-op.
func (c *Conn) Close() error {
	if c.nc.IsClosed() {
		return nil
	}
	if err := c.nc.Drain(); err != nil {
		c.nc.Close()
		return err
	}
	select {
	case <-c.closed:
		return nil
	case <-time.After(drainTimeout):
		c.nc.Close()
		return errors.New("bus: drain timed out")
	}
}
