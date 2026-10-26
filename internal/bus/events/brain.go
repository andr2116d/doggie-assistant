package events

import (
	"encoding/json"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
)

// BrainTurnData records one turn of the brain: what it answered, what it did
// and said, and how long it took. The brain rebuilds its conversation history
// from these events after a restart.
type BrainTurnData struct {
	ThreadID string `json:"thread_id"`
	// Channel is where the replies went.
	Channel string      `json:"channel"`
	Inputs  []TurnInput `json:"inputs"`
	// Actions are the tools the LLM called, in order.
	Actions []TurnAction `json:"actions,omitempty"`
	// Replies are the texts said on Channel (each one also an out.speak).
	Replies  []string `json:"replies,omitempty"`
	Provider string   `json:"provider"`
	Model    string   `json:"model,omitempty"`
	// FirstTextMS is the time from the LLM receiving the input to its first
	// text, or 0 if it wrote none. DurationMS is the whole turn.
	FirstTextMS int64 `json:"first_text_ms,omitempty"`
	DurationMS  int64 `json:"duration_ms"`
	// Error is why the turn failed, if it did, and Failure what kind of
	// failure it was (Failure*).
	Error   string `json:"error,omitempty"`
	Failure string `json:"failure,omitempty"`
	// FiveHourUsage and SevenDayUsage are the share (0-1) of the
	// subscription's usage windows spent, when the provider knows it.
	FiveHourUsage float64 `json:"five_hour_usage,omitempty"`
	SevenDayUsage float64 `json:"seven_day_usage,omitempty"`
	// LowUsage are the usage windows this turn warned were running low.
	LowUsage []UsageWindow `json:"low_usage,omitempty"`
}

// The kinds of BrainTurnData.Failure.
const (
	// FailureUsageLimit: the LLM's usage limit is spent. DOGGIE says so
	// itself, in one of the Replies.
	FailureUsageLimit = "usage_limit"
	FailureTimeout    = "timeout"
	// FailureNoTools: the LLM could not reach DOGGIE's tools.
	FailureNoTools = "tools_unavailable"
	FailureOther   = "other"
)

// UsageWindow is one of the LLM's usage windows, e.g. "five_hour" or
// "seven_day", and when it renews (zero if unknown).
type UsageWindow struct {
	Window string    `json:"window"`
	Renews time.Time `json:"renews,omitzero"`
}

// TurnInput is something a turn answered: a sentence (Said) or a task event
// (Task).
type TurnInput struct {
	// EventID is the ID of the user.utterance or task event.
	EventID string    `json:"event_id"`
	At      time.Time `json:"at"`
	Said    string    `json:"said,omitempty"`
	Channel string    `json:"channel,omitempty"`
	// Resumed marks a sentence the brain answers after a restart: the turn
	// that died may have acted on it already.
	Resumed bool      `json:"resumed,omitempty"`
	Task    *TurnTask `json:"task,omitempty"`
}

// TurnTask is a task event as the brain saw it.
type TurnTask struct {
	// Type is the event type, e.g. "task.done".
	Type        string          `json:"type"`
	TaskID      string          `json:"task_id"`
	Kind        string          `json:"kind"`
	Description string          `json:"description,omitempty"`
	Sensitive   bool            `json:"sensitive,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	// Channel is where a person cancelled the task directly, if they did.
	Channel  string   `json:"channel,omitempty"`
	Question string   `json:"question,omitempty"`
	Options  []string `json:"options,omitempty"`
}

// TurnAction is a tool call.
type TurnAction struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input,omitempty"`
}

// BrainTurn is published by the brain at the end of every turn.
var BrainTurn = bus.NewSubject[BrainTurnData]("brain.turn")

// PastTurn is a turn as it was logged.
type PastTurn struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
	// Seq is the turn's position in the event log, 0 if unknown.
	Seq  uint64        `json:"seq,omitempty"`
	Turn BrainTurnData `json:"turn"`
}

// HistoryRequest asks for the last turns of a conversation thread.
type HistoryRequest struct {
	ThreadID string `json:"thread_id"`
	// Limit is how many turns, at most; the brain bounds it.
	Limit int `json:"limit"`
}

// HistoryReply is a thread's last turns, oldest first.
type HistoryReply struct {
	Turns []PastTurn `json:"turns"`
	// Known is false when the brain doesn't keep the thread in memory,
	// though the event log may still have it.
	Known bool `json:"known"`
}

// BrainHistory asks the brain for a thread's last turns, which it keeps in
// memory, without reading the event log.
var BrainHistory = bus.NewCommand[HistoryRequest, HistoryReply]("brain.history")
