package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/config"
)

// eventsCmd connects to a running DOGGIE over TCP and prints every event.
func eventsCmd(args []string, stdout io.Writer) error {
	flags := flag.NewFlagSet("events", flag.ContinueOnError)
	configPath := flags.String("config", "", "config file (default $XDG_CONFIG_HOME/doggie/config.yaml)")
	server := flags.String("server", "", "bus URL (default derived from bus.listen)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	url := *server
	if url == "" {
		if url, err = localBusURL(cfg.Bus.Listen); err != nil {
			return err
		}
	}
	token, err := bus.LoadToken(tokenPath(cfg))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no bus token at %s: start doggie once or set %s", tokenPath(cfg), bus.TokenEnv)
	}
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	conn, err := bus.Dial(url, token, "events-viewer", log)
	if err != nil {
		return err
	}
	defer conn.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	all, err := bus.SubscribeAll(ctx, conn)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "watching events on %s (Ctrl+C to stop)\n", url)
	for env := range all {
		printEnvelope(stdout, env)
	}
	return nil
}

// localBusURL turns bus.listen into a URL reachable from this machine.
func localBusURL(listen string) (string, error) {
	if listen == "" {
		return "", errors.New("bus.listen is empty: the bus only accepts in-process connections")
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("bus.listen %q: %w", listen, err)
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return "nats://" + net.JoinHostPort(host, port), nil
}

func printEnvelope(w io.Writer, env bus.Envelope) {
	cause := ""
	if env.CausedBy != "" {
		cause = " cause=" + env.CausedBy
	}
	fmt.Fprintf(w, "%s %-20s src=%s%s %s\n",
		env.Time.Local().Format("15:04:05.000"), env.Type, env.Source, cause, env.Data)
}
