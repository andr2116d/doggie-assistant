package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/oklog/ulid/v2"
)

// Meta is the metadata every event carries.
type Meta struct {
	// ID is a ULID: unique and sortable by time.
	ID string `json:"id"`
	// Type names the event, e.g. "system.heartbeat". The NATS subject is
	// "doggie." + Type.
	Type string    `json:"type"`
	Time time.Time `json:"time"`
	// Source is the name of the connection that published the event.
	Source string `json:"source"`
	// CausedBy is the ID of the event that triggered this one, if any.
	CausedBy string `json:"caused_by,omitempty"`
}

// Envelope is an event as it travels on the wire, with its payload still
// encoded.
type Envelope struct {
	Meta
	Data json.RawMessage `json:"data"`
}

// Event is a decoded event with a typed payload.
type Event[T any] struct {
	Meta
	Data T
}

// Subject ties an event type name to its payload type, so the compiler
// rejects publishing or receiving the wrong payload.
type Subject[T any] struct{ typ string }

// NewSubject declares an event type. typ is dot-separated, e.g. "task.done".
// It panics on an invalid type (empty tokens, spaces or wildcards), since
// event types are declared once in the catalog.
func NewSubject[T any](typ string) Subject[T] {
	if !ValidName(typ) {
		panic(fmt.Sprintf("bus: invalid event type %q", typ))
	}
	return Subject[T]{typ: typ}
}

// ValidName reports whether name can be an event type, a command name or a
// name suffix: dot-separated, non-empty tokens with no whitespace or
// wildcards. Validate untrusted input with it before passing it to
// Command.With.
func ValidName(name string) bool {
	for _, tok := range strings.Split(name, ".") {
		if tok == "" || strings.ContainsAny(tok, " \t\r\n*>") {
			return false
		}
	}
	return true
}

// Type returns the event type name.
func (s Subject[T]) Type() string { return s.typ }

func (s Subject[T]) subject() string { return subjectPrefix + s.typ }

// PublishOption customizes a published event.
type PublishOption func(*Meta)

// CausedBy records the event that triggered the one being published.
func CausedBy(id string) PublishOption {
	return func(m *Meta) { m.CausedBy = id }
}

// Publish sends an event and returns its metadata, so the caller can pass the
// ID on as the cause of follow-up events. It returns once the event is stored
// in the event log, so a published event is never lost on shutdown. The event
// ID doubles as the log's deduplication ID, so retrying a publish is safe.
func Publish[T any](ctx context.Context, c *Conn, s Subject[T], data T, opts ...PublishOption) (Meta, error) {
	meta := Meta{
		ID:     ulid.Make().String(),
		Type:   s.typ,
		Time:   time.Now().UTC(),
		Source: c.source,
	}
	for _, opt := range opts {
		opt(&meta)
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return meta, fmt.Errorf("bus: encode %s: %w", s.typ, err)
	}
	msg, err := json.Marshal(Envelope{Meta: meta, Data: payload})
	if err != nil {
		return meta, fmt.Errorf("bus: encode envelope: %w", err)
	}
	if _, err := c.js.Publish(ctx, s.subject(), msg, jetstream.WithMsgID(meta.ID)); err != nil {
		return meta, fmt.Errorf("bus: publish %s: %w", s.typ, err)
	}
	return meta, nil
}

// Subscribe delivers events of type s on the returned channel until ctx is
// cancelled, then unsubscribes and closes the channel. Every event published
// after Subscribe returns is delivered. Undecodable events are logged and
// skipped.
func Subscribe[T any](ctx context.Context, c *Conn, s Subject[T]) (<-chan Event[T], error) {
	out, err := subscribeChan(ctx, c, s.subject(), "", func(m *nats.Msg) (Event[T], bool) {
		var env Envelope
		err := json.Unmarshal(m.Data, &env)
		var ev Event[T]
		if err == nil {
			ev.Meta = env.Meta
			err = json.Unmarshal(env.Data, &ev.Data)
		}
		if err != nil {
			c.log.Warn("bus: dropping undecodable event", "subject", m.Subject, "err", err)
			return ev, false
		}
		return ev, true
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("bus: subscribe %s: %w", s.typ, err)
	}
	return out, nil
}

// Replay delivers every event in the event log from since onwards, then keeps
// delivering new events live until ctx is cancelled. A zero since delivers
// only new events. It is meant for tools such as the event viewer.
func Replay(ctx context.Context, c *Conn, since time.Time) (<-chan Envelope, error) {
	cfg := jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{subjectPrefix + ">"},
		DeliverPolicy:  jetstream.DeliverNewPolicy,
	}
	if !since.IsZero() {
		cfg.DeliverPolicy = jetstream.DeliverByStartTimePolicy
		cfg.OptStartTime = &since
	}
	cons, err := c.js.OrderedConsumer(ctx, EventLogStream, cfg)
	if err != nil {
		return nil, fmt.Errorf("bus: replay consumer: %w", err)
	}
	it, err := cons.Messages()
	if err != nil {
		return nil, fmt.Errorf("bus: replay: %w", err)
	}

	out := make(chan Envelope)
	go func() {
		<-ctx.Done()
		it.Stop()
	}()
	go func() {
		defer close(out)
		for {
			msg, err := it.Next()
			if err != nil {
				return // iterator stopped (ctx cancelled) or connection closed
			}
			var env Envelope
			if err := json.Unmarshal(msg.Data(), &env); err != nil {
				c.log.Warn("bus: skipping undecodable logged event", "subject", msg.Subject(), "err", err)
				continue
			}
			select {
			case out <- env:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}
