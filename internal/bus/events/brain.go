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
	// Error is why the turn failed, if it did.
	Error string `json:"error,omitempty"`
	// FiveHourUsage and SevenDayUsage are the share (0-1) of the
	// subscription's usage windows spent, when the provider knows it.
	FiveHourUsage float64 `json:"five_hour_usage,omitempty"`
	SevenDayUsage float64 `json:"seven_day_usage,omitempty"`
}

// TurnInput is something a turn answered: a sentence (Said) or a task event
// (Task).
type TurnInput struct {
	// EventID is the ID of the user.utterance or task event.
	EventID string    `json:"event_id"`
	At      time.Time `json:"at"`
	Said    string    `json:"said,omitempty"`
	Channel string    `json:"channel,omitempty"`
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
	Question    string          `json:"question,omitempty"`
	Options     []string        `json:"options,omitempty"`
}

// TurnAction is a tool call.
type TurnAction struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input,omitempty"`
}

// BrainTurn is published by the brain at the end of every turn.
var BrainTurn = bus.NewSubject[BrainTurnData]("brain.turn")
