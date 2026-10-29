package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/console"
)

// eventsCmd connects to a running DOGGIE over TCP and prints its events:
// optionally the recent history from the event log, then new events live.
func eventsCmd(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("events", flag.ContinueOnError)
	client := addClientFlags(flags)
	since := flags.Duration("since", 0, "also show logged events from this long ago, e.g. 10m or 2h")
	if err := flags.Parse(args); err != nil {
		return err
	}

	conn, url, err := client.dial("events-viewer")
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var from time.Time
	if *since > 0 {
		from = time.Now().Add(-*since)
	}
	events, err := bus.Replay(ctx, conn, from)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "watching events on %s (Ctrl+C to stop)\n", url)
	for env := range events {
		printEnvelope(stdout, env)
	}
	return nil
}

// printEnvelope prints env on one line. Any satellite chooses its events'
// fields, and JSON may hold line breaks between values, so all of it is
// printed without control characters.
func printEnvelope(w io.Writer, env bus.Envelope) {
	cause := ""
	if env.CausedBy != "" {
		cause = " cause=" + console.OneLine(env.CausedBy)
	}
	fmt.Fprintf(w, "%s %-20s src=%s%s %s\n",
		env.Time.Local().Format("15:04:05.000"), console.OneLine(env.Type), console.OneLine(env.Source), cause, console.OneLine(string(env.Data)))
}
