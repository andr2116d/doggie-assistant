package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

// defaultHistory is how many turns of its conversation a chat shows when it
// opens.
const defaultHistory = 5

// showHistory shows the last turns of the chat's conversation logged before
// it opened, oldest first. Later ones arrive live.
func (c *chatState) showHistory(ctx context.Context, conn *bus.Conn) {
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	envs, err := bus.History(hctx, conn, time.Time{}, events.BrainTurn.Type())
	if err != nil {
		c.view.note("· could not read the conversation so far: " + err.Error())
		return
	}
	type shown struct {
		meta  bus.Meta
		lines []string
	}
	var turns []shown
	for _, env := range envs {
		ev, err := bus.Decode(env, events.BrainTurn)
		if err != nil || ev.Data.ThreadID != c.o.thread || !ev.Time.Before(c.started) {
			continue
		}
		if lines := c.turnLines(ev.Data); len(lines) > 0 {
			turns = append(turns, shown{ev.Meta, lines})
		}
		if len(turns) > c.o.history {
			turns = turns[1:]
		}
	}
	if len(turns) == 0 {
		return
	}
	c.view.note(fmt.Sprintf("· earlier in this conversation, from %s ago:", ago(time.Since(turns[0].meta.Time))))
	// Not marked as seen: a catch-up reads the log from the last event seen,
	// and skips what came before the chat opened anyway.
	for _, t := range turns {
		for _, line := range t.lines {
			c.view.note(line)
		}
	}
	c.view.note("· ───")
}

// turnLines is how a past turn looks: what came in, then the replies.
func (c *chatState) turnLines(t events.BrainTurnData) []string {
	var lines []string
	for _, in := range t.Inputs {
		switch {
		case in.Task != nil:
			lines = append(lines, "· "+taskNotice(in.Task))
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
	if t.Error != "" {
		lines = append(lines, "· the turn failed: "+t.Error)
	}
	return lines
}

// taskNotice is a task event a turn answered, in a few words.
func taskNotice(t *events.TurnTask) string {
	what := t.TaskID
	if t.Description != "" {
		what += " (" + truncate(oneLineText(t.Description), 50) + ")"
	}
	switch t.Type {
	case events.TaskDone.Type():
		return what + " finished"
	case events.TaskFailed.Type():
		return what + " failed: " + truncate(oneLineText(t.Reason), 80)
	case events.TaskCancelled.Type():
		return what + " was cancelled"
	case events.TaskNeedsInput.Type():
		return what + " asked: " + truncate(oneLineText(t.Question), 80)
	}
	return what + " changed"
}

// ago is a rough age, e.g. "5m" or "3d".
func ago(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	return age(max(d, 0))
}
