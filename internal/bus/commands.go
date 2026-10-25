package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
)

// Commands are requests that expect a reply ("start this task"), unlike
// events, which state facts. They live under their own subject prefix,
// outside the event log: JetStream would otherwise answer every request with
// its storage ack. The events a command causes are logged instead.
const commandPrefix = "doggiecmd."

// defaultRequestTimeout applies when the caller's ctx has no deadline.
const defaultRequestTimeout = 10 * time.Second

// ErrNoHandler means nobody serves the command, e.g. there is no worker for a
// task kind.
var ErrNoHandler = errors.New("bus: no handler for command")

// ErrHandlerStopped is the reply to a request that reached a handler while it
// was stopping. The requester receives it as a *RemoteError.
var ErrHandlerStopped = errors.New("handler stopped")

// RemoteError is an error returned by the handler of a command.
type RemoteError struct {
	Command string
	Message string
}

func (e *RemoteError) Error() string { return e.Command + ": " + e.Message }

// Command ties a command name to its request and response types.
type Command[Req, Resp any] struct{ name string }

// NewCommand declares a command. name is dot-separated, e.g. "task.start".
// Like NewSubject, it panics on an invalid name.
func NewCommand[Req, Resp any](name string) Command[Req, Resp] {
	if !ValidName(name) {
		panic(fmt.Sprintf("bus: invalid command name %q", name))
	}
	return Command[Req, Resp]{name: name}
}

// Name returns the command name.
func (c Command[Req, Resp]) Name() string { return c.name }

// With returns the command with extra name tokens, e.g. a task kind:
// run.With("demo.sleep") is "task.run.demo.sleep".
func (c Command[Req, Resp]) With(tokens ...string) Command[Req, Resp] {
	return NewCommand[Req, Resp](c.name + "." + strings.Join(tokens, "."))
}

func (c Command[Req, Resp]) subject() string { return commandPrefix + c.name }

type replyMsg struct {
	Data  json.RawMessage `json:"data,omitempty"`
	Error string          `json:"error,omitempty"`
}

// Request sends a command and waits for its reply. It fails with ErrNoHandler
// when nobody serves the command, and with a *RemoteError when the handler
// fails. Without a deadline in ctx, it gives up after 10s.
func Request[Req, Resp any](ctx context.Context, c *Conn, cmd Command[Req, Resp], req Req) (Resp, error) {
	var resp Resp
	data, err := json.Marshal(req)
	if err != nil {
		return resp, fmt.Errorf("bus: encode %s: %w", cmd.name, err)
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultRequestTimeout)
		defer cancel()
	}

	msg, err := c.nc.RequestWithContext(ctx, cmd.subject(), data)
	if errors.Is(err, nats.ErrNoResponders) {
		return resp, fmt.Errorf("%w: %s", ErrNoHandler, cmd.name)
	}
	if err != nil {
		return resp, fmt.Errorf("bus: request %s: %w", cmd.name, err)
	}
	var r replyMsg
	if err := json.Unmarshal(msg.Data, &r); err != nil {
		return resp, fmt.Errorf("bus: decode reply to %s: %w", cmd.name, err)
	}
	if r.Error != "" {
		return resp, &RemoteError{Command: cmd.name, Message: r.Error}
	}
	if len(r.Data) > 0 {
		if err := json.Unmarshal(r.Data, &resp); err != nil {
			return resp, fmt.Errorf("bus: decode reply to %s: %w", cmd.name, err)
		}
	}
	return resp, nil
}

// Incoming is a received command. The receiver must call Reply or Fail
// exactly once, or the requester times out.
type Incoming[Req, Resp any] struct {
	Req Req
	msg *nats.Msg
}

// Reply answers the command successfully.
func (in Incoming[Req, Resp]) Reply(resp Resp) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return in.Fail(fmt.Errorf("encode reply: %w", err))
	}
	return in.respond(replyMsg{Data: data})
}

// Fail answers the command with an error, which the requester receives as a
// *RemoteError.
func (in Incoming[Req, Resp]) Fail(err error) error {
	return in.respond(replyMsg{Error: err.Error()})
}

func (in Incoming[Req, Resp]) respond(r replyMsg) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return in.msg.Respond(b)
}

// ServeOption customizes Serve.
type ServeOption func(*serveOptions)

type serveOptions struct{ queue string }

// Queue load-balances a command across every server in the same queue group:
// each request reaches exactly one of them.
func Queue(name string) ServeOption {
	return func(o *serveOptions) { o.queue = name }
}

// Serve delivers incoming requests for cmd on the returned channel until ctx
// is cancelled. Requests that cannot be decoded are failed without being
// delivered. The command is served when Serve returns.
func Serve[Req, Resp any](ctx context.Context, c *Conn, cmd Command[Req, Resp], opts ...ServeOption) (<-chan Incoming[Req, Resp], error) {
	var o serveOptions
	for _, opt := range opts {
		opt(&o)
	}
	out, err := subscribeChan(ctx, c, cmd.subject(), o.queue, func(m *nats.Msg) (Incoming[Req, Resp], bool) {
		in := Incoming[Req, Resp]{msg: m}
		if err := json.Unmarshal(m.Data, &in.Req); err != nil {
			in.Fail(fmt.Errorf("bad request: %w", err))
			return in, false
		}
		return in, true
	}, func(m *nats.Msg) {
		Incoming[Req, Resp]{msg: m}.Fail(ErrHandlerStopped)
	})
	if err != nil {
		return nil, fmt.Errorf("bus: serve %s: %w", cmd.name, err)
	}
	return out, nil
}
