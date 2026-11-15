// Package llm is the interface of DOGGIE's LLM providers. DOGGIE builds the
// whole context of every turn, so providers are interchangeable: they only
// run one turn at a time with the input they are given.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/mcp"
)

// ErrToolsUnavailable means the LLM could not reach DOGGIE's tools.
var ErrToolsUnavailable = errors.New("llm: DOGGIE's tools are not available to the LLM")

// ErrTurnTimeout means the turn took longer than allowed.
var ErrTurnTimeout = errors.New("llm: turn timed out")

// ErrNotLoggedIn means the provider has no valid login: the owner has to
// log in again.
var ErrNotLoggedIn = errors.New("llm: not logged in")

// UsageLimitError means the provider's usage limit is spent: it can't
// answer until the limit renews.
type UsageLimitError struct {
	// Renews is when the limit renews, zero if unknown.
	Renews time.Time
	// Detail is what the provider said about it.
	Detail string
}

func (e *UsageLimitError) Error() string {
	return "llm: usage limit reached: " + e.Detail
}

// Brain is an LLM provider.
type Brain interface {
	Name() string
	// Generate runs one turn. It returns at once, and the channel delivers
	// the turn's output in order, ending with a Done, and is then closed.
	// The caller must read it until it is closed, or cancel ctx.
	Generate(ctx context.Context, req Request) (<-chan Output, error)
}

// Request is one turn.
type Request struct {
	// Turn is who the LLM's tool calls act for.
	Turn mcp.Turn
	// Input is the whole user message: the context DOGGIE built plus what
	// just happened.
	Input string
	// Continue, if set, is what is new since the thread's last turn. A
	// provider that still holds the thread's conversation from that turn
	// sends it instead of Input, so the conversation isn't sent again; one
	// that doesn't sends Input. Empty asks for Input always.
	Continue string
	// Stream asks for Delta outputs too, as the text is written.
	Stream bool
}

// Output is one piece of a turn. Exactly one field is set.
type Output struct {
	// Delta is a piece of a text block as it is written, with
	// Request.Stream. The whole block follows as Text.
	Delta string
	// Text is a complete block of text the LLM wrote for the user.
	Text string
	// Tool is a tool the LLM called.
	Tool *ToolCall
	// Done ends the turn.
	Done *Done
}

// ToolCall is a call to one of DOGGIE's tools.
type ToolCall struct {
	// Name is the tool's name, e.g. "start_task".
	Name  string
	Input json.RawMessage
}

// Done is how a turn ended.
type Done struct {
	// Err is nil when the turn completed.
	Err error
	// Model is the model that answered, if known.
	Model string
	// FirstText is the time from sending the input to the first text, or 0
	// if there was none. Request is the time from sending the input to the
	// provider's first request to its model, or 0 if unknown: the rest of
	// FirstText is the model's.
	FirstText time.Duration
	Request   time.Duration
	// Duration is the time from sending the input to the end of the turn.
	Duration time.Duration
	// Usage is how much of the provider's usage limits is spent, if known.
	Usage *Usage
	// Tokens is what the turn used, if known.
	Tokens *Tokens
}

// Tokens is what a turn used, summed over all its calls to the model. The
// input is split three ways: CacheRead was read from the provider's prompt
// cache, CacheWrite was written to it, and Input is the rest.
type Tokens struct {
	Input      int
	CacheRead  int
	CacheWrite int
	Output     int
	// CostUSD is what the turn would cost at the provider's API list
	// prices, or 0 if unknown. It weighs the four counts in one figure:
	// reading the cache is far cheaper than writing it.
	CostUSD float64
}

// Usage is the share (0-1) of a subscription's usage windows already spent,
// and when each renews (zero if unknown).
type Usage struct {
	FiveHour       float64
	SevenDay       float64
	FiveHourRenews time.Time
	SevenDayRenews time.Time
}
