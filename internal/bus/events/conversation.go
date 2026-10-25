package events

import (
	"fmt"
	"strings"

	"github.com/andr2116d/doggie-assistant/internal/bus"
)

// Conversation events: what people say to DOGGIE and what DOGGIE says back.
// Every message belongs to a conversation thread and comes from, or goes to,
// a channel.

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
	// Block numbers the text blocks of a turn, from 0, so a channel can
	// match them with the OutSpeakDelta pieces it already showed.
	Block int `json:"block"`
}

// SpeakDeltaData is a piece of a text block as the LLM writes it.
type SpeakDeltaData struct {
	ThreadID string `json:"thread_id,omitempty"`
	Channel  string `json:"channel"`
	Text     string `json:"text"`
	Block    int    `json:"block"`
	// CausedBy is the event the turn answers, as in the out.speak that
	// completes the block.
	CausedBy string `json:"caused_by"`
}

// OutSpeakDelta streams a text block while it is written, so a channel can
// show or say it at once. It is a signal: pieces stay out of the event log
// and may be lost, so the out.speak that completes the block is the truth.
var OutSpeakDelta = bus.NewSignal[SpeakDeltaData]("out.speak.delta")

var (
	// UserUtterance is a complete sentence from a person, published by the
	// channel that heard or received it (terminal, voice, WhatsApp) under
	// its own name: UserUtterance.With(channel), see UtteranceOn. The bus
	// only lets each satellite publish for its own channels, so the channel
	// in the subject can be trusted; the one in the payload can't.
	UserUtterance = bus.NewSubject[UtteranceData]("user.utterance")
	// OutSpeak asks a channel to say something, through text-to-speech or
	// as a chat message. The brain publishes it, caused by the event it
	// answers.
	OutSpeak = bus.NewSubject[SpeakData]("out.speak")
)

// MaxUtteranceRunes bounds what a person can say in one utterance; the brain
// ignores longer ones.
const MaxUtteranceRunes = 8000

// maxChannel bounds a channel name.
const maxChannel = 64

// ValidChannel reports whether c can name a channel: one subject token,
// e.g. "terminal" or "voice:living_room", with no dots, spaces or
// wildcards.
func ValidChannel(c string) bool {
	return c != "" && len(c) <= maxChannel && !strings.Contains(c, ".") && bus.ValidName(c)
}

// UtteranceOn is the subject a channel publishes its utterances on.
func UtteranceOn(channel string) (bus.Subject[UtteranceData], error) {
	if !ValidChannel(channel) {
		return bus.Subject[UtteranceData]{}, fmt.Errorf("invalid channel %q: use one word without dots or spaces, e.g. voice:living_room", channel)
	}
	return UserUtterance.With(channel), nil
}
