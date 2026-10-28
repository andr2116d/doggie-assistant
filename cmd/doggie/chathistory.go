package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/console"
)

// defaultHistory is how many turns of its conversation a chat shows when it
// opens.
const defaultHistory = 5

// historyWindows are how far back a chat looks in the log for its
// conversation, widening while it finds too few turns; zero is the whole
// log.
var historyWindows = []time.Duration{time.Hour, 24 * time.Hour, 0}

// showHistory shows the last turns of the chat's conversation logged before
// it opened, oldest first. Later ones arrive live. It asks the brain, which
// keeps them in memory, and reads the log if the brain can't tell.
func (c *chatState) showHistory(ctx context.Context, conn *bus.Conn) {
	turns, err := c.historyFromBrain(ctx, conn)
	if err != nil {
		turns, err = c.historyFromLog(ctx, conn)
	}
	if err != nil {
		c.say(console.NoticeBad, "could not read the conversation so far: %v", err)
		return
	}
	type shown struct {
		at    time.Time
		lines []string
	}
	var show []shown
	for _, t := range turns {
		if t.Turn.ThreadID != c.o.thread || !c.beforeOpening(t) {
			continue
		}
		if lines := c.turnLines(t.Turn); len(lines) > 0 {
			show = append(show, shown{t.At, lines})
		}
	}
	if len(show) > c.o.history {
		show = show[len(show)-c.o.history:]
	}
	if len(show) == 0 {
		return
	}
	if c.view.screen != nil {
		c.historyOnScreen(turns, len(show))
		return
	}
	c.say(console.NoticeInfo, "earlier in this conversation, from %s ago:", ago(time.Since(show[0].at)))
	// Not marked as seen: a catch-up reads the log from the last event seen,
	// and skips what came before the chat opened anyway.
	for _, t := range show {
		for _, line := range t.lines {
			c.view.note(line)
		}
	}
	c.view.note("· ───")
}

// beforeOpening reports whether a turn was logged before the chat opened,
// by its position in the log when both are known: clocks may differ.
func (c *chatState) beforeOpening(t events.PastTurn) bool {
	if t.Seq != 0 && c.openSeq != 0 {
		return t.Seq <= c.openSeq
	}
	return t.At.Before(c.started)
}

// historyFromBrain asks the brain for the conversation's last turns. It
// fails when the brain isn't there or doesn't know the conversation.
func (c *chatState) historyFromBrain(ctx context.Context, conn *bus.Conn) ([]events.PastTurn, error) {
	bctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	r, err := bus.Request(bctx, conn, events.BrainHistory, events.HistoryRequest{ThreadID: c.o.thread, Limit: c.o.history})
	if err != nil {
		return nil, err
	}
	if !r.Known {
		return nil, errors.New("the brain doesn't know the conversation")
	}
	return r.Turns, nil
}

// historyFromLog reads the conversation's last turns from the log, looking
// further back only while it finds too few.
func (c *chatState) historyFromLog(ctx context.Context, conn *bus.Conn) ([]events.PastTurn, error) {
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var turns []events.PastTurn
	for _, window := range historyWindows {
		var since time.Time
		if window > 0 {
			since = time.Now().Add(-window)
		}
		envs, err := bus.History(hctx, conn, since, events.BrainTurn.Type())
		if err != nil {
			return nil, err
		}
		turns = turns[:0]
		for _, env := range envs {
			ev, err := bus.Decode(env, events.BrainTurn)
			if err != nil || ev.Data.ThreadID != c.o.thread {
				continue
			}
			turns = append(turns, events.PastTurn{ID: ev.ID, At: ev.Time, Seq: ev.Seq, Turn: ev.Data})
		}
		if len(turns) >= c.o.history {
			break
		}
	}
	return turns, nil
}

// turnLines is how a past turn looks: what came in, then the replies.
func (c *chatState) turnLines(t events.BrainTurnData) []string {
	var lines []string
	for _, in := range t.Inputs {
		switch {
		case in.Task != nil:
			lines = append(lines, "· "+taskNotice(c.wordsOr().tr, in.Task))
		case in.Said != "":
			who := "you"
			if in.Channel != "" && in.Channel != c.o.channel {
				who = in.Channel
			}
			lines = append(lines, fmt.Sprintf("%s> %s", who, oneLineText(in.Said)))
		}
	}
	for _, r := range t.Replies {
		if r = strings.TrimSpace(r); r != "" {
			lines = append(lines, "doggie> "+r)
		}
	}
	if why := failure(c.wordsOr().tr, t); why != "" {
		lines = append(lines, "· "+c.wordsOr().tr.f("the turn failed: %s", why))
	}
	return lines
}

// taskNotice is a task event a turn answered, in a few words.
func taskNotice(tr translator, t *events.TurnTask) string {
	what := t.TaskID
	if t.Description != "" {
		what += " (" + truncate(oneLineText(t.Description), 50) + ")"
	}
	switch t.Type {
	case events.TaskDone.Type():
		return tr.f("%s finished", what)
	case events.TaskFailed.Type():
		return tr.f("%s failed: %s", what, truncate(oneLineText(t.Reason), 80))
	case events.TaskCancelled.Type():
		return tr.f("%s was cancelled", what)
	case events.TaskNeedsInput.Type():
		if t.Question == "" {
			return tr.f("%s asked a sensitive question", what)
		}
		return tr.f("%s asked: %s", what, truncate(oneLineText(t.Question), 80))
	}
	return tr.f("%s changed", what)
}

// ago is a rough age, e.g. "5m" or "3d".
func ago(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return age(max(d, 0))
}

// failure tells why a turn failed, or "" if it didn't or DOGGIE said why
// itself.
func failure(tr translator, t events.BrainTurnData) string {
	switch {
	case t.Error == "" || t.Failure == events.FailureUsageLimit:
		return ""
	case t.Failure == events.FailureTimeout:
		return tr.f("it took too long")
	case t.Failure == events.FailureNoTools:
		return tr.f("DOGGIE's tools didn't answer")
	case t.Failure == events.FailureNotLoggedIn:
		return tr.f(`its LLM isn't logged in; log in with claude, or run "doggie login" for the backup`)
	}
	return t.Error
}

// historyOnScreen shows the last n turns of the thread from before the
// chat opened, as the screen shows them live.
func (c *chatState) historyOnScreen(turns []events.PastTurn, n int) {
	var mine []events.PastTurn
	for _, t := range turns {
		if t.Turn.ThreadID == c.o.thread && c.beforeOpening(t) && len(c.turnLines(t.Turn)) > 0 {
			mine = append(mine, t)
		}
	}
	for i, t := range mine[max(len(mine)-n, 0):] {
		c.turnOnScreen(fmt.Sprintf("past:%d", i), t.Turn)
	}
}
