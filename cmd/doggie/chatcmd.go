package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

const chatHelp = `· commands, run by the chat itself, not by DOGGIE's brain:
  /tasks               tasks in progress; /tasks all adds the last ones that ended
  /answer <id> [text]  answer a task's question yourself, the only way for
                       the ones DOGGIE may not answer for you; without text,
                       type it hidden
  /cancel <id> [why]   cancel a task
  /help                this list
  //text               say text that starts with "/"`

// commandTimeout bounds each request a command makes.
const commandTimeout = 5 * time.Second

// listAllLimit is how many tasks /tasks all shows.
const listAllLimit = 20

// command is a slash command typed in the chat.
type command struct {
	name string
	// id is the task it acts on; text the rest of the line.
	id, text string
	// asked is the task /answer found waiting.
	asked events.TaskInfo
}

// parseLine tells a sentence for DOGGIE from a command. A line starting with
// "//" is a sentence starting with "/".
func parseLine(line string) (sentence string, cmd *command) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "/") {
		return line, nil
	}
	if strings.HasPrefix(line, "//") {
		return line[1:], nil
	}
	name, rest := cutWord(line)
	cmd = &command{name: name}
	switch name {
	case "/answer", "/cancel":
		cmd.id, cmd.text = cutWord(rest)
	default:
		cmd.text = rest
	}
	return "", cmd
}

// cutWord splits s at its first run of spaces.
func cutWord(s string) (word, rest string) {
	s = strings.TrimSpace(s)
	i := strings.IndexFunc(s, unicode.IsSpace)
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i:])
}

// check reports what is wrong with a command before running it, if
// anything.
func (cmd *command) check() string {
	switch cmd.name {
	case "/help":
	case "/tasks":
		if cmd.text != "" && cmd.text != "all" {
			return "· usage: /tasks [all]"
		}
	case "/answer":
		if cmd.id == "" || cmd.text == "" {
			return "· usage: /answer <id> <text>, e.g. /answer t3 yes"
		}
	case "/cancel":
		if cmd.id == "" {
			return "· usage: /cancel <id> [why]"
		}
	default:
		return fmt.Sprintf("· unknown command %s; /help lists them, and //text says text starting with \"/\"", cmd.name)
	}
	return ""
}

// run runs a checked command against DOGGIE, other than /help, and returns
// what to show and whether it worked. thread and channel are the chat's.
func (cmd *command) run(ctx context.Context, conn *bus.Conn, thread, channel string) commandResult {
	out, ok := cmd.exec(ctx, conn, thread, channel)
	r := commandResult{out: out, ok: ok}
	if ok && cmd.name == "/answer" {
		r.answered = &cmd.asked
	}
	return r
}

func (cmd *command) exec(ctx context.Context, conn *bus.Conn, thread, channel string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	switch cmd.name {
	case "/tasks":
		req := events.TaskListRequest{ActiveOnly: true}
		if cmd.text == "all" {
			req = events.TaskListRequest{Limit: listAllLimit}
		}
		list, err := bus.Request(ctx, conn, events.ListTasks, req)
		if err != nil {
			return "· could not list the tasks: " + commandError(err), false
		}
		return taskTable(list, req.ActiveOnly, thread, time.Now()), true
	case "/cancel":
		cancel, err := events.CancelTaskOn(channel)
		if err == nil {
			_, err = bus.Request(ctx, conn, cancel, events.TaskCancelRequest{TaskID: cmd.id, Reason: cmd.text})
		}
		if err != nil {
			return fmt.Sprintf("· could not cancel %s: %s", cmd.id, commandError(err)), false
		}
		return "· cancelled " + cmd.id, true
	case "/answer":
		info, err := bus.Request(ctx, conn, events.GetTask, events.TaskRef{TaskID: cmd.id})
		if err != nil {
			return fmt.Sprintf("· could not answer %s: %s", cmd.id, commandError(err)), false
		}
		cmd.asked = info
		if info.Status != events.StatusWaitingInput {
			return fmt.Sprintf("· could not answer %s: it isn't waiting for an answer (it is %s)", cmd.id, info.Status), false
		}
		if _, err := bus.Request(ctx, conn, events.AnswerTask, events.TaskAnswer{TaskID: cmd.id, Answer: cmd.text}); err != nil {
			return fmt.Sprintf("· could not answer %s: %s", cmd.id, commandError(err)), false
		}
		if len(info.Options) > 0 && !hasOption(info.Options, cmd.text) {
			return fmt.Sprintf("· answered %s, though not with one of its options (%s)", cmd.id, strings.Join(info.Options, " | ")), true
		}
		return "· answered " + cmd.id, true
	}
	return "", false
}

func hasOption(options []string, answer string) bool {
	for _, o := range options {
		if strings.EqualFold(strings.TrimSpace(o), answer) {
			return true
		}
	}
	return false
}

// commandError is why a command failed, in the user's terms.
func commandError(err error) string {
	var remote *bus.RemoteError
	switch {
	case errors.As(err, &remote):
		return remote.Message
	case errors.Is(err, bus.ErrNoHandler):
		return "DOGGIE's task manager isn't running"
	case errors.Is(err, context.DeadlineExceeded):
		return "DOGGIE didn't answer in time"
	}
	return err.Error()
}

// taskTable shows a task list, marking the tasks of other conversations.
func taskTable(list events.TaskList, activeOnly bool, thread string, now time.Time) string {
	if len(list.Tasks) == 0 {
		if activeOnly {
			return "· no tasks in progress"
		}
		return "· no tasks"
	}
	var b strings.Builder
	if activeOnly {
		fmt.Fprintf(&b, "· %s in progress:\n", tasksCount(len(list.Tasks)))
	} else {
		fmt.Fprintf(&b, "· the last %s:\n", tasksCount(len(list.Tasks)))
	}
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  ID\tSTATUS\tAGE\tTASK\tDETAIL")
	for _, t := range list.Tasks {
		what := t.Description
		if what == "" {
			what = t.Kind
		}
		more := detail(t)
		if t.Status == events.StatusPending {
			more = "" // its detail is its description
		}
		what = truncate(oneLineText(what), 50)
		if t.ThreadID != thread {
			what += " (" + elsewhere(t.ThreadID) + ")"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\t%s\n", t.ID, t.Status, age(now.Sub(t.CreatedAt)), what, truncate(oneLineText(more), 60))
	}
	tw.Flush()
	table := strings.Split(b.String(), "\n")
	for i, line := range table {
		table[i] = strings.TrimRight(line, " ") // an empty last column
	}
	b.Reset()
	b.WriteString(strings.Join(table, "\n"))
	if list.Total > len(list.Tasks) {
		fmt.Fprintf(&b, "  (and %s before)\n", tasksCount(list.Total-len(list.Tasks)))
	}
	return strings.TrimRight(b.String(), "\n")
}

// elsewhere names the conversation a task belongs to, when it isn't this
// one.
func elsewhere(thread string) string {
	if thread == "" {
		return "no conversation"
	}
	return "conversation " + thread
}

func tasksCount(n int) string {
	if n == 1 {
		return "1 task"
	}
	return fmt.Sprintf("%d tasks", n)
}

// commandResult is what a command run in the background shows.
type commandResult struct {
	out string
	ok  bool
	// answered is the task whose question /answer answered.
	answered *events.TaskInfo
}

// commander runs commands in order, so each sees what the one before did,
// without holding the chat loop.
func commander(ctx context.Context, conn *bus.Conn, thread, channel string, cmds <-chan *command, done chan<- commandResult) {
	for {
		select {
		case cmd := <-cmds:
			select {
			case done <- cmd.run(ctx, conn, thread, channel):
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// isAnswer reports whether a line is an /answer, which may carry a secret
// and so isn't kept for Up and Down.
func isAnswer(line string) bool {
	_, cmd := parseLine(line)
	return cmd != nil && cmd.name == "/answer"
}
