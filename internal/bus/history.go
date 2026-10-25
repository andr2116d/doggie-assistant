package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// validPattern accepts event type patterns: dot-separated tokens where a
// token may be "*" (any one token) and the last one may be ">" (one or more
// tokens), e.g. "task.>".
func validPattern(p string) bool {
	toks := strings.Split(p, ".")
	for i, tok := range toks {
		switch {
		case tok == "*":
		case tok == ">" && i == len(toks)-1:
		case tok == "" || strings.ContainsAny(tok, " \t\r\n*>"):
			return false
		}
	}
	return true
}

// SubscribeEnvelopes delivers every event whose type matches pattern (e.g.
// "task.>") on the returned channel, still encoded, until ctx is cancelled.
// It suits components that handle a family of events in one place.
func SubscribeEnvelopes(ctx context.Context, c *Conn, pattern string) (<-chan Envelope, error) {
	if !validPattern(pattern) {
		return nil, fmt.Errorf("bus: invalid event pattern %q", pattern)
	}
	out, err := subscribeChan(ctx, c, subjectPrefix+pattern, "", func(m *nats.Msg) (Envelope, bool) {
		var env Envelope
		if err := json.Unmarshal(m.Data, &env); err != nil {
			c.log.Warn("bus: dropping undecodable event", "subject", m.Subject, "err", err)
			return env, false
		}
		return env, true
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("bus: subscribe %s: %w", pattern, err)
	}
	return out, nil
}

// History returns the logged events whose type matches pattern, from since
// (or the start of the log, if since is zero) up to now, oldest first.
func History(ctx context.Context, c *Conn, since time.Time, pattern string) ([]Envelope, error) {
	if !validPattern(pattern) {
		return nil, fmt.Errorf("bus: invalid event pattern %q", pattern)
	}
	cfg := jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{subjectPrefix + pattern},
		DeliverPolicy:  jetstream.DeliverAllPolicy,
	}
	if !since.IsZero() {
		cfg.DeliverPolicy = jetstream.DeliverByStartTimePolicy
		cfg.OptStartTime = &since
	}
	cons, err := c.js.OrderedConsumer(ctx, EventLogStream, cfg)
	if err != nil {
		return nil, fmt.Errorf("bus: history consumer: %w", err)
	}

	var out []Envelope
	for {
		batch, err := cons.FetchNoWait(256)
		if err != nil {
			return out, fmt.Errorf("bus: history: %w", err)
		}
		n := 0
		for msg := range batch.Messages() {
			n++
			var env Envelope
			if err := json.Unmarshal(msg.Data(), &env); err != nil {
				c.log.Warn("bus: skipping undecodable logged event", "subject", msg.Subject(), "err", err)
				continue
			}
			if md, err := msg.Metadata(); err == nil {
				env.Seq = md.Sequence.Stream
			}
			out = append(out, env)
		}
		if err := batch.Error(); err != nil {
			return out, fmt.Errorf("bus: history: %w", err)
		}
		if n == 0 {
			return out, nil
		}
	}
}

// Decode returns the payload of env as an event of type s. It fails when env
// is of another type.
// DecodeUnder decodes an event of a type under s, e.g. "user.utterance.x"
// under UserUtterance, and returns the rest of its type ("x").
func DecodeUnder[T any](env Envelope, s Subject[T]) (Event[T], string, error) {
	ev := Event[T]{Meta: env.Meta}
	rest, ok := strings.CutPrefix(env.Type, s.typ+".")
	if !ok || rest == "" {
		return ev, "", fmt.Errorf("bus: event is %s, not under %s", env.Type, s.typ)
	}
	if err := json.Unmarshal(env.Data, &ev.Data); err != nil {
		return ev, "", fmt.Errorf("bus: decode %s: %w", env.Type, err)
	}
	return ev, rest, nil
}

func Decode[T any](env Envelope, s Subject[T]) (Event[T], error) {
	ev := Event[T]{Meta: env.Meta}
	if env.Type != s.typ {
		return ev, fmt.Errorf("bus: event is %s, not %s", env.Type, s.typ)
	}
	if err := json.Unmarshal(env.Data, &ev.Data); err != nil {
		return ev, fmt.Errorf("bus: decode %s: %w", s.typ, err)
	}
	return ev, nil
}

// LastSeq returns the position of the newest event in the log, or 0 if it is
// empty: an event logged later has a greater Seq.
func LastSeq(ctx context.Context, c *Conn) (uint64, error) {
	stream, err := c.js.Stream(ctx, EventLogStream)
	if err != nil {
		return 0, fmt.Errorf("bus: event log: %w", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		return 0, fmt.Errorf("bus: event log: %w", err)
	}
	return info.State.LastSeq, nil
}

// LastEvent returns the most recent logged event of type s, however old, and
// false if the log has none.
func LastEvent[T any](ctx context.Context, c *Conn, s Subject[T]) (Event[T], bool, error) {
	var ev Event[T]
	stream, err := c.js.Stream(ctx, EventLogStream)
	if err != nil {
		return ev, false, fmt.Errorf("bus: event log: %w", err)
	}
	msg, err := stream.GetLastMsgForSubject(ctx, s.subject())
	if errors.Is(err, jetstream.ErrMsgNotFound) {
		return ev, false, nil
	}
	if err != nil {
		return ev, false, fmt.Errorf("bus: last %s: %w", s.typ, err)
	}
	var env Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		return ev, false, fmt.Errorf("bus: decode last %s: %w", s.typ, err)
	}
	env.Seq = msg.Sequence
	ev, err = Decode(env, s)
	return ev, err == nil, err
}
