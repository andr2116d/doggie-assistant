package brain

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/brain/llm"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

// Once the usage limit is spent, DOGGIE says so itself, in its language,
// and nothing of what the provider said reaches the user.
func TestSpentUsageIsToldByDOGGIE(t *testing.T) {
	lima := time.FixedZone("Lima", -5*3600)
	h := newHarness(t, Options{Language: "es", Location: lima})
	renews := time.Now().Add(2 * time.Hour).Truncate(time.Minute)
	h.fake.set(func(f *fake) {
		f.reply = func(context.Context, llm.Request) []llm.Output {
			return []llm.Output{{Done: &llm.Done{
				Err:   &llm.UsageLimitError{Renews: renews, Detail: "You've hit your session limit (API status 429)"},
				Usage: &llm.Usage{FiveHour: 1, SevenDay: 0.95},
			}}}
		}
	})
	id := h.say("th", "terminal", "¿Qué hora es?")
	s := h.nextSpeak()
	want := "Ahora no puedo pensar: se me acabó la cuota y se renueva "
	if !strings.HasPrefix(s.Data.Text, want) || !strings.Contains(s.Data.Text, clockOf(renews.In(lima))) || s.CausedBy != id || s.Data.Block != 0 {
		t.Errorf("said %+v, want %q... at %s", s.Data, want, clockOf(renews.In(lima)))
	}
	d := h.nextTurn().Data
	if d.Failure != events.FailureUsageLimit || !strings.Contains(d.Error, "session limit") || len(d.Replies) != 1 || d.Replies[0] != s.Data.Text {
		t.Errorf("turn = %+v", d)
	}
	// Running low is not news once it's spent.
	if len(d.LowUsage) != 0 {
		t.Errorf("warned of low usage too: %+v", d.LowUsage)
	}
	// The LLM sees, next time, what DOGGIE told the user.
	h.fake.set(func(f *fake) { f.reply = nil })
	h.say("th", "terminal", "¿Ya puedes?")
	h.nextTurn()
	mustContain(t, h.fake.requests()[1].Input, "<reply>Ahora no puedo pensar")
}

// The usage limit is spent before the turn even starts.
func TestSpentUsageBeforeTheTurn(t *testing.T) {
	h := newHarness(t, Options{Language: "en"})
	h.fake.set(func(f *fake) { f.fail = fmt.Errorf("start: %w", &llm.UsageLimitError{Detail: "spent"}) })
	h.say("th", "terminal", "hi")
	if s := h.nextSpeak(); s.Data.Text != "I can't think right now: I've run out of quota for a while. Tasks already running carry on." {
		t.Errorf("said %q", s.Data.Text)
	}
	if d := h.nextTurn().Data; d.Failure != events.FailureUsageLimit || len(d.Replies) != 1 {
		t.Errorf("turn = %+v", d)
	}
}

func TestFailuresAreClassified(t *testing.T) {
	h := newHarness(t, Options{})
	for _, tc := range []struct {
		err  error
		want string
	}{
		{llm.ErrTurnTimeout, events.FailureTimeout},
		{fmt.Errorf("x: %w", llm.ErrToolsUnavailable), events.FailureNoTools},
		{fmt.Errorf("claude: Please run /login: %w", llm.ErrNotLoggedIn), events.FailureNotLoggedIn},
		{errBoom, events.FailureOther},
	} {
		h.fake.set(func(f *fake) {
			f.reply = func(context.Context, llm.Request) []llm.Output { return []llm.Output{{Done: &llm.Done{Err: tc.err}}} }
		})
		h.say("th", "terminal", "hola")
		if d := h.nextTurn().Data; d.Failure != tc.want || d.Error != tc.err.Error() || len(d.Replies) != 0 {
			t.Errorf("%v: turn = %+v", tc.err, d)
		}
	}
	h.drainSpeaks()
	h.fake.set(func(f *fake) { f.reply = nil })
	h.say("th", "terminal", "ok")
	if d := h.nextTurn().Data; d.Failure != "" || d.Error != "" {
		t.Errorf("a good turn = %+v", d)
	}
}

// DOGGIE warns once per usage window, after the reply, and remembers it
// across a restart.
func TestLowUsageIsWarnedOncePerWindow(t *testing.T) {
	h := newHarness(t, Options{Language: "es"})
	usage := &llm.Usage{FiveHour: 0.5, SevenDay: 0.3}
	h.fake.set(func(f *fake) {
		f.reply = func(_ context.Context, req llm.Request) []llm.Output {
			if req.Turn.ThreadID == probe {
				return nil
			}
			f.mu.Lock()
			u := *usage
			f.mu.Unlock()
			return []llm.Output{{Text: "Claro."}, {Done: &llm.Done{Usage: &u}}}
		}
	})
	setUsage := func(u llm.Usage) { h.fake.set(func(*fake) { *usage = u }) }
	turn := func(thread string) events.BrainTurnData {
		h.say(thread, "terminal", "hola")
		d := h.nextTurn().Data
		h.drainSpeaks()
		return d
	}

	if d := turn("th"); len(d.LowUsage) != 0 || len(d.Replies) != 1 {
		t.Fatalf("warned at 50%%: %+v", d)
	}
	renews := time.Now().Add(3 * time.Hour)
	setUsage(llm.Usage{FiveHour: 0.9, FiveHourRenews: renews, SevenDay: 0.3})
	h.say("th", "terminal", "hola")
	if s := h.nextSpeak(); s.Data.Text != "Claro." || s.Data.Block != 0 {
		t.Errorf("first said %+v", s.Data)
	}
	if s := h.nextSpeak(); !strings.HasPrefix(s.Data.Text, "Ojo: me queda poca cuota para pensar. Se renueva ") || s.Data.Block != 1 {
		t.Errorf("then said %+v", s.Data)
	}
	d := h.nextTurn().Data
	if len(d.LowUsage) != 1 || d.LowUsage[0].Window != "five_hour" || !d.LowUsage[0].Renews.Equal(renews) || len(d.Replies) != 2 {
		t.Errorf("turn = %+v", d)
	}
	// Once only, in this thread or any other.
	setUsage(llm.Usage{FiveHour: 0.97, FiveHourRenews: renews, SevenDay: 0.3})
	if d := turn("other"); len(d.LowUsage) != 0 || len(d.Replies) != 1 {
		t.Errorf("warned again: %+v", d)
	}
	// Nor after a restart: a new brain starts with nothing in memory.
	h.stop()
	h.startBrain(Options{Language: "es"})
	if d := turn("th"); len(d.LowUsage) != 0 {
		t.Errorf("warned again after a restart: %+v", d)
	}
	// Another window is news.
	setUsage(llm.Usage{FiveHour: 0.97, FiveHourRenews: renews, SevenDay: 0.91})
	if d := turn("th"); len(d.LowUsage) != 1 || d.LowUsage[0].Window != "seven_day" {
		t.Errorf("didn't warn of the other window: %+v", d)
	}
}

// A turn cut in the middle of a block: the warning is a block of its own,
// not the end of the one left unfinished.
func TestLowUsageAfterAnUnfinishedBlock(t *testing.T) {
	h := newHarness(t, Options{Language: "es"})
	h.fake.set(func(f *fake) {
		f.reply = func(_ context.Context, req llm.Request) []llm.Output {
			if req.Turn.ThreadID == probe {
				return nil
			}
			return []llm.Output{{Text: "Claro."}, {Delta: "Y ade"}, {Done: &llm.Done{Err: llm.ErrTurnTimeout, Usage: &llm.Usage{FiveHour: 0.95}}}}
		}
	})
	h.say("th", "terminal", "hola")
	if s := h.nextSpeak(); s.Data.Block != 0 {
		t.Errorf("first said %+v", s.Data)
	}
	if s := h.nextSpeak(); !strings.HasPrefix(s.Data.Text, "Ojo:") || s.Data.Block != 2 {
		t.Errorf("warned in block %d: %q, want block 2", s.Data.Block, s.Data.Text)
	}
}

// A turn cut short because DOGGIE stops doesn't spend the warning: the next
// one gives it.
func TestLowUsageIsNotSpentWhileStopping(t *testing.T) {
	h := newHarness(t, Options{Language: "es"})
	low := &llm.Usage{FiveHour: 0.95}
	h.fake.set(func(f *fake) {
		f.reply = func(ctx context.Context, req llm.Request) []llm.Output {
			if req.Turn.ThreadID == probe {
				return nil
			}
			<-ctx.Done()
			return []llm.Output{{Done: &llm.Done{Err: ctx.Err(), Usage: low}}}
		}
	})
	h.say("th", "terminal", "hola")
	h.waitRequest("hola")
	h.stop()
	if d := h.nextTurn().Data; len(d.LowUsage) != 0 || len(d.Replies) != 0 {
		t.Errorf("warned while stopping: %+v", d)
	}
	h.fake.set(func(f *fake) {
		f.reply = func(_ context.Context, req llm.Request) []llm.Output {
			if req.Turn.ThreadID == probe {
				return nil
			}
			return []llm.Output{{Text: "Hola."}, {Done: &llm.Done{Usage: low}}}
		}
	})
	h.startBrain(Options{Language: "es"})
	// The unanswered sentence comes back first: either turn may warn, once.
	h.say("th2", "terminal", "otra vez")
	warned := 0
	for {
		d := h.nextTurn().Data
		warned += len(d.LowUsage)
		if d.ThreadID == "th2" {
			break
		}
	}
	if warned != 1 {
		t.Errorf("warned %d times after the restart, want 1", warned)
	}
}

// clockOf is a time as DOGGIE tells it: "9:05", "21:10".
func clockOf(t time.Time) string { return fmt.Sprintf("%d:%02d", t.Hour(), t.Minute()) }
