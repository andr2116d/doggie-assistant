// Package prompt is the context builder: it turns what DOGGIE
// knows into the message the LLM receives on each turn. DOGGIE owns the
// context, so every turn is built whole, in layers:
//
//	<world>         the current time and the active tasks
//	<conversation>  the recent turns of the thread, oldest first
//	<input>         what just happened: what the user said and task events
//
// v0 keeps it simple: no memory, no summaries, and budgets by count and
// size. Every dynamic text is escaped, so nothing can forge the structure,
// and what tasks report is wrapped in <data>: the system prompt tells the
// LLM it is information, never instructions.
package prompt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

// Limits that keep a prompt bounded whatever the inputs.
const (
	// maxConversation bounds the conversation layer, in bytes. The oldest
	// turns are dropped first.
	maxConversation = 48 << 10
	// maxHistoryText bounds one text of a past turn, in runes.
	maxHistoryText = 2000
	// maxInputText bounds one text of the current input, in runes.
	maxInputText = 8000
	// maxDataText bounds what a task reports (result, error, question), in
	// runes.
	maxDataText = 1000
	// maxTasks bounds the active tasks listed.
	maxTasks = 20
)

// Context is everything a turn's message is built from.
type Context struct {
	Now time.Time
	// Location is the owner's time zone. Nil means UTC.
	Location *time.Location
	// ThreadID is the thread the turn belongs to.
	ThreadID string
	// Tasks are the active tasks.
	Tasks []events.TaskInfo
	// History are the thread's past turns, oldest first.
	History []Turn
	// MaxTurns is how many past turns may be included.
	MaxTurns int
	// Inputs is what happened since the last turn, oldest first.
	Inputs []Input
}

// Turn is one past turn of a thread.
type Turn struct {
	At      time.Time
	Inputs  []Input
	Actions []Action
	Replies []string
}

// Action is a tool the LLM called in a turn.
type Action struct {
	Tool  string
	Input json.RawMessage
}

// Input is something that happened: a sentence from the user, or a task
// event. Exactly one of Said or Event is set.
type Input struct {
	At time.Time
	// Said is what the user said, on Channel.
	Said    string
	Channel string
	Event   *TaskEvent
}

// TaskEvent is a change in a task the LLM should tell the user about.
type TaskEvent struct {
	// Type is the event type: task.done, task.failed, task.cancelled or
	// task.needs_input.
	Type string
	Task events.TaskInfo
	// Result, for task.done. Hidden for sensitive tasks.
	Result json.RawMessage
	// Reason is the error of a failed task or why it was cancelled.
	Reason string
	// Question and Options, for task.needs_input.
	Question string
	Options  []string
}

// Build returns the message for a turn.
func Build(c Context) string {
	loc := c.Location
	if loc == nil {
		loc = time.UTC
	}
	var b strings.Builder
	b.WriteString("<world>\n")
	fmt.Fprintf(&b, "Now: %s\n", c.Now.In(loc).Format("Monday, 2 January 2006, 15:04 MST"))
	writeTasks(&b, c.Tasks, c.ThreadID)
	b.WriteString("</world>\n")

	if conv := conversation(c.History, c.MaxTurns, loc); conv != "" {
		b.WriteString("\n<conversation>\n")
		b.WriteString(conv)
		b.WriteString("</conversation>\n")
	}

	b.WriteString("\n<input>\n")
	for _, in := range c.Inputs {
		writeInput(&b, in, loc, maxInputText)
	}
	b.WriteString("</input>\n")
	return b.String()
}

func writeTasks(b *strings.Builder, tasks []events.TaskInfo, thread string) {
	var active []events.TaskInfo
	for _, t := range tasks {
		if !t.Status.Terminal() {
			active = append(active, t)
		}
	}
	if len(active) == 0 {
		b.WriteString("Active tasks: none\n")
		return
	}
	// The thread's own tasks first, then the most recent.
	slices.SortStableFunc(active, func(x, y events.TaskInfo) int {
		if mx, my := x.ThreadID == thread, y.ThreadID == thread; mx != my {
			if mx {
				return -1
			}
			return 1
		}
		return y.CreatedAt.Compare(x.CreatedAt)
	})
	b.WriteString("Active tasks:\n")
	for i, t := range active {
		if i == maxTasks {
			fmt.Fprintf(b, "- and %d more (use list_tasks)\n", len(active)-maxTasks)
			break
		}
		fmt.Fprintf(b, "- %s %s", esc(t.ID), esc(t.Kind))
		if t.Description != "" {
			fmt.Fprintf(b, " <data>%s</data>", esc(cut(t.Description, maxDataText)))
		}
		fmt.Fprintf(b, ": %s", status(t))
		if t.ThreadID == thread && thread != "" {
			b.WriteString(", asked in this conversation")
		}
		b.WriteString("\n")
	}
}

func status(t events.TaskInfo) string {
	switch t.Status {
	case events.StatusWaitingInput:
		s := "waiting for an answer to <data>" + esc(cut(t.Question, maxDataText)) + "</data>"
		if len(t.Options) > 0 {
			s += " (options: <data>" + esc(cut(strings.Join(t.Options, " | "), maxDataText)) + "</data>)"
		}
		return s
	case events.StatusRunning:
		s := "running"
		if t.Percent > 0 {
			s += fmt.Sprintf(", %d%%", t.Percent)
		}
		if t.Progress != "" {
			s += ", <data>" + esc(cut(t.Progress, maxDataText)) + "</data>"
		}
		return s
	}
	return esc(string(t.Status))
}

// conversation renders the most recent turns that fit, oldest first.
func conversation(history []Turn, maxTurns int, loc *time.Location) string {
	if maxTurns <= 0 || len(history) == 0 {
		return ""
	}
	if len(history) > maxTurns {
		history = history[len(history)-maxTurns:]
	}
	var parts []string
	size := 0
	for i := len(history) - 1; i >= 0; i-- {
		s := renderTurn(history[i], loc)
		if size+len(s) > maxConversation {
			break
		}
		size += len(s)
		parts = append(parts, s)
	}
	slices.Reverse(parts)
	return strings.Join(parts, "")
}

func renderTurn(t Turn, loc *time.Location) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<turn at=\"%s\">\n", stamp(t.At, loc))
	for _, in := range t.Inputs {
		writeInput(&b, in, loc, maxHistoryText)
	}
	for _, a := range t.Actions {
		fmt.Fprintf(&b, "<action tool=\"%s\">%s</action>\n", attr(a.Tool), esc(cut(compact(a.Input), maxDataText)))
	}
	for _, r := range t.Replies {
		fmt.Fprintf(&b, "<reply>%s</reply>\n", esc(cut(r, maxHistoryText)))
	}
	b.WriteString("</turn>\n")
	return b.String()
}

func writeInput(b *strings.Builder, in Input, loc *time.Location, maxText int) {
	if in.Event == nil {
		fmt.Fprintf(b, "<said channel=\"%s\" at=\"%s\">%s</said>\n", attr(in.Channel), stamp(in.At, loc), esc(cut(in.Said, maxText)))
		return
	}
	e := in.Event
	fmt.Fprintf(b, "<event type=\"%s\" task=\"%s\" at=\"%s\">", attr(e.Type), attr(e.Task.ID), stamp(in.At, loc))
	fmt.Fprintf(b, "Task %s (%s", esc(e.Task.ID), esc(e.Task.Kind))
	if e.Task.Description != "" {
		fmt.Fprintf(b, ", <data>%s</data>", esc(cut(e.Task.Description, maxDataText)))
	}
	b.WriteString(") ")
	switch e.Type {
	case events.TaskDone.Type():
		b.WriteString("finished.")
		switch {
		case e.Task.Sensitive:
			b.WriteString(" Its result is hidden: it is a sensitive task.")
		case len(e.Result) > 0 && string(e.Result) != "null":
			fmt.Fprintf(b, " Result: <data>%s</data>", esc(cut(compact(e.Result), maxDataText)))
		}
	case events.TaskFailed.Type():
		fmt.Fprintf(b, "failed: <data>%s</data>", esc(cut(e.Reason, maxDataText)))
	case events.TaskCancelled.Type():
		b.WriteString("was cancelled")
		if e.Reason != "" {
			fmt.Fprintf(b, ": <data>%s</data>", esc(cut(e.Reason, maxDataText)))
		}
		b.WriteString(".")
	case events.TaskNeedsInput.Type():
		fmt.Fprintf(b, "needs an answer: <data>%s</data>", esc(cut(e.Question, maxDataText)))
		if len(e.Options) > 0 {
			fmt.Fprintf(b, " Options: <data>%s</data>", esc(cut(strings.Join(e.Options, " | "), maxDataText)))
		}
	default:
		b.WriteString("changed.")
	}
	b.WriteString("</event>\n")
}

// stamp is a local date and time, to the minute.
func stamp(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.In(loc).Format("2006-01-02 15:04")
}

// compact returns JSON on one line, or the raw text if it isn't JSON.
func compact(raw json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	return b.String()
}

var (
	textEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	attrEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "\n", " ", "\r", " ")
)

// esc escapes text so it can't open or close a tag: no text can forge the
// message's structure.
func esc(s string) string { return textEscaper.Replace(strings.ToValidUTF8(s, "\uFFFD")) }

// attr escapes an attribute value, which also must not end its quotes or
// span lines.
func attr(s string) string { return attrEscaper.Replace(strings.ToValidUTF8(s, "\uFFFD")) }

// cut shortens s to maxRunes, marking the cut.
func cut(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	return string([]rune(s)[:maxRunes]) + "… [cut]"
}
