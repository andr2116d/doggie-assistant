// Command doggie runs the DOGGIE assistant.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/config"
	"github.com/andr2116d/doggie-assistant/internal/supervisor"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `doggie: a self-hosted, Jarvis-style personal assistant.

Usage:
  doggie run [flags]      run DOGGIE (see "doggie run -h")
  doggie version          print the version
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "doggie:", err)
		}
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("missing command")
	}
	switch args[0] {
	case "run":
		return runCmd(args[1:])
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	}
	return fmt.Errorf("unknown command %q (see \"doggie help\")", args[0])
}

func runCmd(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	configPath := fs.String("config", "", "config file (default $XDG_CONFIG_HOME/doggie/config.yaml)")
	only := fs.String("only", "", "comma-separated list of components to run")
	except := fs.String("except", "", "comma-separated list of components to skip")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel()}))
	if cfg.UsingDefaults {
		log.Warn("no config file, using defaults", "expected", cfg.File)
	}

	selected, err := selectFactories(factories(), *only, *except)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		// After the first signal, restore default handling so a second
		// Ctrl+C kills the process if shutdown hangs.
		<-ctx.Done()
		stop()
	}()

	// The bus starts before the components and stops after them: deferred
	// calls run in reverse, so connections close before the server shuts down.
	srv, err := startBus(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer srv.Shutdown()

	comps, closeConns, err := buildComponents(selected, cfg, log, srv)
	if err != nil {
		return err
	}
	defer closeConns()

	log.Info("doggie starting", "version", version, "components", names(selected))
	err = supervisor.New(log).Run(ctx, comps...)
	log.Info("doggie stopped")
	return err
}

func startBus(ctx context.Context, cfg config.Config, log *slog.Logger) (*bus.Server, error) {
	token, err := bus.LoadOrCreateToken(tokenPath(cfg))
	if err != nil {
		return nil, err
	}
	srv, err := bus.Start(ctx, bus.Options{
		Listen:   cfg.Bus.Listen,
		Token:    token,
		StoreDir: filepath.Join(cfg.DataDir, "nats"),
		MaxAge:   cfg.Bus.EventLog.MaxAge,
		MaxBytes: cfg.Bus.EventLog.MaxSizeMB << 20,
		Log:      log,
	})
	if err != nil {
		return nil, err
	}
	listen := srv.ClientURL()
	if listen == "" {
		listen = "in-process only"
	}
	log.Info("bus ready", "listen", listen, "token_file", tokenPath(cfg))
	return srv, nil
}

func tokenPath(cfg config.Config) string {
	return filepath.Join(cfg.DataDir, "bus.token")
}
