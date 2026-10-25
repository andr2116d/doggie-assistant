package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

const taskUsage = `Usage:
  doggie task start [-desc text] [-timeout 30s] [-thread id] [-sensitive] <kind> [input-json]
  doggie task list [-active] [-limit n]
  doggie task get <id>
  doggie task cancel [-reason text] <id>
  doggie task answer <id> <answer...>

All subcommands also take -config, -server and -ca (see "doggie events -h").
`

// taskOptions are the subcommand flags.
type taskOptions struct {
	desc, thread, reason string
	timeout              time.Duration
	active, sensitive    bool
	limit                int
}

// taskCmd manages tasks of a running DOGGIE over TCP.
func taskCmd(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stdout, taskUsage)
		return errors.New("missing task subcommand")
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, taskUsage)
		return nil
	}
	rest, o, client, err := parseTask(args[0], args[1:])
	if err != nil {
		return err
	}
	conn, _, err := client.dial("cli")
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return taskSub(ctx, conn, stdout, args[0], rest, o)
}

// parseTask parses the flags of a task subcommand and returns its
// positional arguments.
func parseTask(sub string, args []string) ([]string, taskOptions, clientFlags, error) {
	flags := flag.NewFlagSet("task "+sub, flag.ContinueOnError)
	client := addClientFlags(flags)
	var o taskOptions
	switch sub {
	case "start":
		flags.StringVar(&o.desc, "desc", "", "human-readable description")
		flags.DurationVar(&o.timeout, "timeout", 0, "task timeout (default: the manager's)")
		flags.StringVar(&o.thread, "thread", "cli", "conversation thread that asks")
		flags.BoolVar(&o.sensitive, "sensitive", false, "keep the input and result out of the event log")
	case "list":
		flags.BoolVar(&o.active, "active", false, "only tasks still in progress")
		flags.IntVar(&o.limit, "limit", 0, "show at most this many recent tasks (default: the manager's)")
	case "cancel":
		flags.StringVar(&o.reason, "reason", "", "why the task is cancelled")
	case "get", "answer":
	default:
		return nil, o, client, fmt.Errorf("unknown task subcommand %q (see \"doggie task help\")", sub)
	}
	if err := flags.Parse(args); err != nil {
		return nil, o, client, err
	}
	// Go stops parsing flags at the first argument, so a flag written after
	// the arguments would silently become an argument.
	for _, a := range flags.Args() {
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if strings.HasPrefix(a, "-") && flags.Lookup(name) != nil {
			return nil, o, client, fmt.Errorf("flag %s must come before the arguments (see \"doggie task help\")", a)
		}
	}
	return flags.Args(), o, client, nil
}

// taskSub runs a task subcommand over conn.
func taskSub(ctx context.Context, conn *bus.Conn, w io.Writer, sub string, args []string, o taskOptions) error {
	switch sub {
	case "start":
		if len(args) < 1 || len(args) > 2 {
			return errors.New("usage: doggie task start [flags] <kind> [input-json]")
		}
		spec := events.TaskSpec{Kind: args[0], Description: o.desc, ThreadID: o.thread, Sensitive: o.sensitive}
		if len(args) == 2 {
			if !json.Valid([]byte(args[1])) {
				return fmt.Errorf("input must be valid JSON, got %s", args[1])
			}
			spec.Input = json.RawMessage(args[1])
		}
		if o.timeout > 0 {
			spec.TimeoutS = int(math.Ceil(o.timeout.Seconds()))
		}
		ref, err := bus.Request(ctx, conn, events.StartTask, spec)
		if err != nil {
			return err
		}
		fmt.Fprintln(w, ref.TaskID)

	case "list":
		list, err := bus.Request(ctx, conn, events.ListTasks, events.TaskListRequest{ActiveOnly: o.active, Limit: o.limit})
		if err != nil {
			return err
		}
		printTasks(w, list.Tasks, time.Now())
		if list.Total > len(list.Tasks) {
			fmt.Fprintf(w, "(showing the %d most recent of %d tasks; use -limit)\n", len(list.Tasks), list.Total)
		}

	case "get":
		if len(args) != 1 {
			return errors.New("usage: doggie task get <id>")
		}
		info, err := bus.Request(ctx, conn, events.GetTask, events.TaskRef{TaskID: args[0]})
		if err != nil {
			return err
		}
		out, err := json.MarshalIndent(info, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(w, string(out))

	case "cancel":
		if len(args) != 1 {
			return errors.New("usage: doggie task cancel [-reason text] <id>")
		}
		if _, err := bus.Request(ctx, conn, events.CancelTask, events.TaskCancelRequest{TaskID: args[0], Reason: o.reason, ByUser: true}); err != nil {
			return err
		}
		fmt.Fprintf(w, "cancelled %s\n", args[0])

	case "answer":
		if len(args) < 2 {
			return errors.New("usage: doggie task answer <id> <answer...>")
		}
		if _, err := bus.Request(ctx, conn, events.AnswerTask, events.TaskAnswer{TaskID: args[0], Answer: strings.Join(args[1:], " ")}); err != nil {
			return err
		}
		fmt.Fprintf(w, "answered %s\n", args[0])

	default:
		return fmt.Errorf("unknown task subcommand %q", sub)
	}
	return nil
}

func printTasks(w io.Writer, tasks []events.TaskInfo, now time.Time) {
	if len(tasks) == 0 {
		fmt.Fprintln(w, "no tasks")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tSTATUS\tAGE\tDETAIL")
	for _, t := range tasks {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", t.ID, t.Kind, t.Status, age(now.Sub(t.CreatedAt)), truncate(detail(t), 70))
	}
	tw.Flush()
}

// detail is the most useful one-line summary of a task for its status.
func detail(t events.TaskInfo) string {
	switch t.Status {
	case events.StatusWaitingInput:
		if len(t.Options) > 0 {
			return fmt.Sprintf("%q [%s]", t.Question, strings.Join(t.Options, " | "))
		}
		return fmt.Sprintf("%q", t.Question)
	case events.StatusRunning:
		return t.Progress
	case events.StatusDone:
		return string(t.Result)
	case events.StatusFailed, events.StatusCancelled:
		return t.Error
	}
	return t.Description
}

func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
