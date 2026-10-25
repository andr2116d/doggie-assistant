package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

const sayUsage = `Usage:
  doggie say [-channel terminal] [-thread id] [-wait 20s] <text...>

Says text to DOGGIE as if it came from a channel, prints the replies, then
keeps listening for -wait after the last one, for notices about tasks the
reply started. Ctrl+C stops listening.

It also takes -config, -server and -ca (see "doggie events -h").
`

// sayOptions are the flags of doggie say.
type sayOptions struct {
	channel, thread string
	// wait is how long to keep listening after the last activity.
	wait time.Duration
	// replyTimeout bounds the wait for the brain to answer.
	replyTimeout time.Duration
}

func sayCmd(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("say", flag.ContinueOnError)
	flags.Usage = func() { fmt.Fprint(flags.Output(), sayUsage) }
	client := addClientFlags(flags)
	var o sayOptions
	flags.StringVar(&o.channel, "channel", "terminal", "channel the text comes from")
	flags.StringVar(&o.thread, "thread", "", "conversation thread (default: the channel's)")
	flags.DurationVar(&o.wait, "wait", 20*time.Second, "keep listening this long after the last reply")
	if err := flags.Parse(args); err != nil {
		return err
	}
	text := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if text == "" {
		fmt.Fprint(os.Stderr, sayUsage)
		return errors.New("nothing to say")
	}
	if o.thread == "" {
		o.thread = o.channel
	}
	o.replyTimeout = 3 * time.Minute

	conn, _, err := client.dial("say")
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return say(ctx, conn, stdout, text, o)
}

// say publishes text as an utterance and prints what the brain says in the
// thread until the turn has ended and wait has passed with nothing new.
func say(ctx context.Context, conn *bus.Conn, w io.Writer, text string, o sayOptions) error {
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Subscribe first, so no reply is missed.
	speaks, err := bus.Subscribe(sctx, conn, events.OutSpeak)
	if err != nil {
		return err
	}
	turns, err := bus.Subscribe(sctx, conn, events.BrainTurn)
	if err != nil {
		return err
	}
	meta, err := bus.Publish(ctx, conn, events.UserUtterance, events.UtteranceData{ThreadID: o.thread, Channel: o.channel, Text: text})
	if err != nil {
		return err
	}

	answered := false
	idle := time.NewTimer(o.replyTimeout)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-idle.C:
			if !answered {
				return errors.New("no reply: is DOGGIE running with the brain component?")
			}
			return nil
		case ev, ok := <-speaks:
			if !ok {
				return ctx.Err()
			}
			if ev.Data.ThreadID == o.thread {
				fmt.Fprintf(w, "doggie> %s\n", ev.Data.Text)
			}
		case ev, ok := <-turns:
			if !ok {
				return ctx.Err()
			}
			if ev.Data.ThreadID != o.thread {
				continue
			}
			if ev.Data.Error != "" {
				fmt.Fprintf(w, "doggie: the turn failed: %s\n", ev.Data.Error)
			}
			if slices.ContainsFunc(ev.Data.Inputs, func(in events.TurnInput) bool { return in.EventID == meta.ID }) {
				answered = true
			}
			if answered {
				// Keep listening for notices, from now on.
				idle.Reset(o.wait)
			}
		}
	}
}
