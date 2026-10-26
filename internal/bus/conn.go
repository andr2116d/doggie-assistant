package bus

import (
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
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
	// identity is who the bus vouches the connection is (see Identity).
	identity string
	log      *slog.Logger
	closed   chan struct{}
	// closing is set by Close, so ending the connection on purpose isn't
	// reported as losing it.
	closing atomic.Bool
}

// connHooks report what happens to a connection (see DialOptions).
type connHooks struct {
	onDisconnect func(error)
	onReconnect  func()
	onClosed     func(error)
}

// newConn opens a connection with connect, passing it the options that track
// its state. The Conn exists before connecting, so every callback sees
// whether Close was called.
func newConn(source, identity string, log *slog.Logger, h connHooks, connect func(...nats.Option) (*nats.Conn, error)) (*Conn, error) {
	c := &Conn{source: source, identity: identity, log: log, closed: make(chan struct{})}
	opts := []nats.Option{nats.ClosedHandler(func(nc *nats.Conn) {
		close(c.closed)
		if h.onClosed != nil && !c.closing.Load() {
			h.onClosed(nc.LastError())
		}
	})}
	if h.onDisconnect != nil {
		opts = append(opts, nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			if !c.closing.Load() && !nc.IsClosed() {
				h.onDisconnect(err)
			}
		}))
	}
	if h.onReconnect != nil {
		opts = append(opts, nats.ReconnectHandler(func(*nats.Conn) {
			if !c.closing.Load() {
				h.onReconnect()
			}
		}))
	}
	nc, err := connect(opts...)
	if err != nil {
		return nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("bus: jetstream: %w", err)
	}
	c.nc, c.js = nc, js
	return c, nil
}

// Name returns the connection name, which is the source of its events.
func (c *Conn) Name() string { return c.source }

// Identity is who the bus vouches the connection is: a named satellite's
// name, or the connection name of DOGGIE's own components, which may use
// any. It is empty for the shared satellite token, which vouches for no one.
// Subjects that carry a name, such as a worker's reports, are only open to
// that identity.
func (c *Conn) Identity() string { return c.identity }

// Close flushes pending publishes, waits until they reach the server and
// closes the connection. Calling it again is a no-op.
func (c *Conn) Close() error {
	c.closing.Store(true)
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
