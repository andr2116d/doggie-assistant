package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/brain/llm"
)

// attempt runs the turn on pr, sending it input. A turn that ends well,
// tells what is new since the last one and leaves a conversation that
// fits leaves pr to its thread's next turn; any other stops it.
func (p *Provider) attempt(ctx, tctx context.Context, pr *proc, req llm.Request, input string, out chan<- llm.Output) (*llm.Done, bool) {
	req.Input = input
	d, retry := p.serve(ctx, tctx, pr, req, out)
	if d.Err != nil || req.Continue == "" || !fits(d.Tokens) || !p.keepEpoch(req.Turn.ThreadID, pr) {
		p.discard(pr)
	}
	return d, retry
}

// serving is one turn while a process serves it. Only the goroutine that
// serves the turn touches it.
type serving struct {
	p   *Provider
	pr  *proc
	req llm.Request
	out chan<- llm.Output
	// ctx is the caller's; tctx, derived from it, is the turn's deadline.
	ctx, tctx context.Context
	start     time.Time
	done      *llm.Done
	// started is set once the CLI is up: from then on the model may act.
	started bool
	limit   *rateLimit // the last usage the CLI reported
}

// serve runs one turn on pr, until tctx (the turn's deadline, derived from
// the caller's ctx) ends. It reports whether the turn may be retried: it
// failed before the model started and said nothing. After a turn that ends
// with an error pr is of no use: it was killed, it exited, or it may still
// be answering.
func (p *Provider) serve(ctx, tctx context.Context, pr *proc, req llm.Request, out chan<- llm.Output) (d *llm.Done, retry bool) {
	pr.said = false
	s := &serving{p: p, pr: pr, req: req, out: out, ctx: ctx, tctx: tctx, start: time.Now(), done: &llm.Done{}}
	defer func() { d.Duration = time.Since(s.start) }()
	// Between turns the process's tools act for nobody.
	defer pr.client.Unbind()

	// A turn cancelled before it starts must not reach the tools.
	if tctx.Err() != nil {
		return s.stopped(), false
	}
	if err := pr.client.Bind(req.Turn); err != nil {
		s.done.Err = err
		return s.done, false
	}
	sent := make(chan error, 1)
	go func() { sent <- pr.send(req.Input) }()

	for {
		// The deadline wins over output that is ready at the same time.
		if tctx.Err() != nil {
			return s.stopped(), false
		}
		select {
		case <-tctx.Done():
			return s.stopped(), false
		case err := <-sent:
			sent = nil
			if err != nil {
				// The process is gone; its output explains why.
				p.opts.Log.Debug("claudecode: send input", "err", err)
			}
		case line, ok := <-pr.lines:
			if !ok {
				return s.exited()
			}
			if ended, retry := s.handle(line); ended {
				return s.done, retry
			}
		}
	}
}

// stopped ends a turn that was cancelled or ran out of time.
func (s *serving) stopped() *llm.Done {
	s.pr.kill()
	s.done.Err = llm.ErrTurnTimeout
	if s.ctx.Err() != nil {
		s.done.Err = s.ctx.Err()
	}
	return s.done
}

// exited ends a turn whose process ended without a result. It may be
// retried if the model had not started and nothing was said.
func (s *serving) exited() (d *llm.Done, retry bool) {
	s.done.Err = s.pr.exitError()
	if needsLogin(s.done.Err.Error()) {
		s.done.Err = fmt.Errorf("%w: %w", s.done.Err, llm.ErrNotLoggedIn)
	}
	if s.p.isClosed() {
		return &llm.Done{Err: ErrClosed}, false
	}
	return s.done, !s.started && !s.pr.said
}

// emit gives o to the caller. It reports false if the turn's time ended, or
// the provider closed, before the caller took it.
func (s *serving) emit(o llm.Output) bool {
	select {
	case s.out <- o:
		s.pr.said = true
		return true
	case <-s.tctx.Done():
		return false
	case <-s.p.done:
		// Closing: the caller may never read again.
		s.pr.kill()
		return false
	}
}

// handle takes in one line of the CLI's output. It reports whether the
// turn ended with it, and then whether it may be retried.
func (s *serving) handle(line []byte) (ended, retry bool) {
	var ev event
	if err := json.Unmarshal(line, &ev); err != nil {
		s.p.opts.Log.Debug("claudecode: skipping undecodable line", "err", err)
		return false, false
	}
	switch ev.Type {
	case "system":
		return s.onSystem(&ev)
	case "stream_event":
		s.onStreamEvent(&ev)
	case "assistant":
		s.onAssistant(&ev)
	case "rate_limit_event":
		if ev.RateLimit != nil {
			s.limit = ev.RateLimit
			s.done.Usage = s.limit.usage()
		}
	case "result":
		s.onResult(&ev)
		return true, false
	}
	return false, false
}

// onSystem takes in what the CLI says about itself. A CLI that starts
// without DOGGIE's tools ends the turn, which may be retried if nothing was
// said.
func (s *serving) onSystem(ev *event) (ended, retry bool) {
	if ev.Subtype == "status" && ev.Status == "requesting" && s.done.Request == 0 {
		s.done.Request = time.Since(s.start)
	}
	if ev.Subtype != "init" {
		return false, false
	}
	s.started = true
	s.done.Model = ev.Model
	if ev.toolsConnected() {
		return false, false
	}
	s.pr.kill()
	s.done.Err = llm.ErrToolsUnavailable
	return true, !s.pr.said
}

// onStreamEvent gives out a piece of text as it comes, if the turn streams.
func (s *serving) onStreamEvent(ev *event) {
	if !ev.isTextDelta() {
		return
	}
	if s.done.FirstText == 0 {
		s.done.FirstText = time.Since(s.start)
	}
	if s.req.Stream {
		s.emit(llm.Output{Delta: ev.Event.Delta.Text})
	}
}

// onAssistant gives out the text and the tool calls of a whole message.
func (s *serving) onAssistant(ev *event) {
	// The CLI writes some messages itself, e.g. when the usage limit is
	// spent: the result tells what went wrong.
	if ev.Message == nil || ev.Message.Model == syntheticModel {
		return
	}
	for _, c := range ev.Message.Content {
		var o llm.Output
		switch {
		case c.Type == "text" && strings.TrimSpace(c.Text) != "":
			o.Text = c.Text
		case c.Type == "tool_use":
			o.Tool = &llm.ToolCall{Name: strings.TrimPrefix(c.Name, toolPrefix), Input: c.Input}
		default:
			continue
		}
		if !s.emit(o) {
			return
		}
	}
}

// onResult takes in how the CLI says the turn ended.
func (s *serving) onResult(ev *event) {
	// A failed turn may have used tokens too.
	s.done.Tokens = ev.tokens()
	if t := s.done.Tokens; t != nil && t.CostUSD >= s.pr.cost {
		t.CostUSD, s.pr.cost = t.CostUSD-s.pr.cost, t.CostUSD
	}
	f := ev.failure()
	if f == "" {
		return
	}
	// A 429 the CLI gives up on is the usage limit, even when it said
	// nothing about the limits first.
	switch spent, renews := s.limit.spent(); {
	case spent || ev.apiStatus() == 429:
		s.done.Err = &llm.UsageLimitError{Renews: renews, Detail: f}
	case ev.apiStatus() == 401 || needsLogin(f):
		s.done.Err = fmt.Errorf("claudecode: %s: %w", f, llm.ErrNotLoggedIn)
	default:
		s.done.Err = fmt.Errorf("claudecode: %s", f)
	}
}
