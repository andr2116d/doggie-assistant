package bus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// pendingLimit is how many messages a subscription queues while its consumer
// is busy. Beyond that, NATS drops messages and reports a slow consumer,
// which is logged. It is a variable so tests can lower it.
var pendingLimit = 65536

// subscribeChan subscribes to subject, in queue group queue if it is not
// empty, and delivers decoded messages on the returned channel until ctx is
// cancelled. decode returns false to skip a message. The subscription is
// active on the server when subscribeChan returns.
//
// Messages that arrive once ctx is cancelled (the unsubscribe happens an
// instant later) are passed to drop, if not nil, so a request can be refused
// at once instead of timing out.
func subscribeChan[E any](ctx context.Context, c *Conn, subject, queue string, decode func(*nats.Msg) (E, bool), drop func(*nats.Msg)) (<-chan E, error) {
	out := make(chan E)
	// The handler holds mu (read) while it may send on out, so out is only
	// closed once no handler can send on it anymore.
	var (
		mu     sync.RWMutex
		closed bool
	)
	handler := func(m *nats.Msg) {
		v, ok := decode(m)
		if !ok {
			return
		}
		mu.RLock()
		defer mu.RUnlock()
		if !closed && ctx.Err() == nil {
			select {
			case out <- v:
				return
			case <-ctx.Done():
			}
		}
		if drop != nil {
			drop(m)
		}
	}

	var (
		sub *nats.Subscription
		err error
	)
	if queue == "" {
		sub, err = c.nc.Subscribe(subject, handler)
	} else {
		sub, err = c.nc.QueueSubscribe(subject, queue, handler)
	}
	if err != nil {
		return nil, err
	}
	if err := sub.SetPendingLimits(pendingLimit, 64<<20); err != nil {
		sub.Unsubscribe()
		return nil, err
	}
	// Wait for the server to register the subscription, so every message
	// sent after we return is delivered, even from another connection.
	if err := c.nc.FlushTimeout(5 * time.Second); err != nil {
		sub.Unsubscribe()
		return nil, err
	}
	// The server refuses forbidden subscriptions asynchronously, before
	// answering the flush: surface it instead of returning a subscription
	// that will never receive anything.
	if err := c.nc.LastError(); errors.Is(err, nats.ErrPermissionViolation) && strings.Contains(err.Error(), fmt.Sprintf("Subscription to %q", subject)) {
		sub.Unsubscribe()
		return nil, err
	}

	go func() {
		<-ctx.Done()
		// Drain, not Unsubscribe: until the server processes the unsubscribe
		// it keeps routing messages here, and Unsubscribe would discard them
		// silently (a request would then wait for its whole timeout). Drain
		// keeps handling them, so the handler can drop them explicitly.
		sub.Drain()
		mu.Lock()
		closed = true
		close(out)
		mu.Unlock()
	}()
	return out, nil
}

// recentLimit is how many event IDs a subscription remembers. An event sent
// again after an outage is stored once in the log, but its first copy may
// have reached subscribers already: they drop the second one.
const recentLimit = 1024

// recentIDs remembers the last event IDs a subscription delivered, forgetting
// the oldest first. Only that subscription's handler uses it, and NATS calls
// a handler one message at a time.
type recentIDs struct {
	ids  map[string]struct{}
	ring []string
	next int // where the next ID goes once ring is full
}

func newRecentIDs() *recentIDs {
	return &recentIDs{ids: make(map[string]struct{})}
}

// fresh reports whether id wasn't delivered yet, and remembers it.
func (r *recentIDs) fresh(id string) bool {
	if id == "" {
		return true
	}
	if _, ok := r.ids[id]; ok {
		return false
	}
	if len(r.ring) < recentLimit {
		r.ring = append(r.ring, id)
	} else {
		delete(r.ids, r.ring[r.next])
		r.ring[r.next] = id
		r.next = (r.next + 1) % recentLimit
	}
	r.ids[id] = struct{}{}
	return true
}
