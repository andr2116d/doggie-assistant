package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/nats-io/nats.go"
)

// Signals are frequent, disposable messages, such as liveness beats. Unlike
// events they are not stored in the event log, so they never flood it, and
// unlike commands nobody replies. A lost signal is simply superseded by the
// next one.
const signalPrefix = "doggiesig."

// Signal ties a signal name to its payload type.
type Signal[T any] struct{ name string }

// NewSignal declares a signal. Like NewSubject, it panics on an invalid name.
func NewSignal[T any](name string) Signal[T] {
	if !ValidName(name) {
		panic(fmt.Sprintf("bus: invalid signal name %q", name))
	}
	return Signal[T]{name: name}
}

// Wire returns the NATS subject of the signal.
func (s Signal[T]) Wire() string { return signalPrefix + s.name }

// With returns the signal under s named by tokens, e.g. a worker's
// "task.alive.<worker>". It panics on invalid tokens.
func (s Signal[T]) With(tokens ...string) Signal[T] {
	return NewSignal[T](s.name + "." + strings.Join(tokens, "."))
}

// Emit sends a signal, fire and forget.
func Emit[T any](c *Conn, s Signal[T], data T) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("bus: encode signal %s: %w", s.name, err)
	}
	if err := c.nc.Publish(s.Wire(), payload); err != nil {
		return fmt.Errorf("bus: emit %s: %w", s.name, err)
	}
	return nil
}

// SubscribeSignal delivers signals of type s on the returned channel until
// ctx is cancelled. Undecodable signals are dropped.
func SubscribeSignal[T any](ctx context.Context, c *Conn, s Signal[T]) (<-chan T, error) {
	out, err := subscribeChan(ctx, c, s.Wire(), "", func(m *nats.Msg) (T, bool) {
		var v T
		return v, json.Unmarshal(m.Data, &v) == nil
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("bus: subscribe signal %s: %w", s.name, err)
	}
	return out, nil
}

// SignalUnder is a signal sent under a signal, and the token it came under.
type SignalUnder[T any] struct {
	Under string
	Data  T
}

// SubscribeSignalUnder delivers the signals sent to s.With(token), for any
// one token, until ctx is cancelled. Undecodable signals are dropped.
func SubscribeSignalUnder[T any](ctx context.Context, c *Conn, s Signal[T]) (<-chan SignalUnder[T], error) {
	out, err := subscribeChan(ctx, c, s.Wire()+".*", "", func(m *nats.Msg) (SignalUnder[T], bool) {
		v := SignalUnder[T]{Under: strings.TrimPrefix(m.Subject, s.Wire()+".")}
		return v, json.Unmarshal(m.Data, &v.Data) == nil
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("bus: subscribe signal %s: %w", s.name, err)
	}
	return out, nil
}
