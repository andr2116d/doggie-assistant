package events

import "github.com/andr2116d/doggie-assistant/internal/bus"

// Conversation events: what people say to DOGGIE and what DOGGIE says back.
// Every message belongs to a conversation thread (ADR-0004) and comes from,
// or goes to, a channel.

// UtteranceData is something a person said or typed to DOGGIE.
type UtteranceData struct {
	// ThreadID is the conversation thread. Empty means the channel's own
	// thread.
	ThreadID string `json:"thread_id,omitempty"`
	// Channel is where it came from, e.g. "terminal" or "voice:living_room".
	Channel string `json:"channel"`
	Text    string `json:"text"`
}

// SpeakData is something DOGGIE says on a channel.
type SpeakData struct {
	ThreadID string `json:"thread_id,omitempty"`
	Channel  string `json:"channel"`
	Text     string `json:"text"`
}

var (
	// UserUtterance is a complete sentence from a person, published by the
	// channel that heard or received it (terminal, voice, WhatsApp).
	UserUtterance = bus.NewSubject[UtteranceData]("user.utterance")
	// OutSpeak asks a channel to say something, through text-to-speech or
	// as a chat message. The brain publishes it, caused by the event it
	// answers.
	OutSpeak = bus.NewSubject[SpeakData]("out.speak")
)
