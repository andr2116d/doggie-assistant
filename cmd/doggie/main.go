// Command doggie runs the DOGGIE assistant.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/config"
	"github.com/andr2116d/doggie-assistant/internal/supervisor"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `doggie: a self-hosted, Jarvis-style personal assistant.

Usage:
  doggie run [flags]      run DOGGIE (see "doggie run -h")
  doggie events [flags]   watch bus events live (see "doggie events -h")
  doggie task <cmd>       start, list, get, cancel or answer tasks (see "doggie task help")
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
	case "events":
		return eventsCmd(args[1:], stdout)
	case "task":
		return taskCmd(args[1:], stdout)
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
	opts := bus.Options{
		Listen:               cfg.Bus.Listen,
		Token:                token,
		SatellitePermissions: events.SatellitePermissions(),
		StoreDir:             filepath.Join(cfg.DataDir, "nats"),
		MaxAge:               cfg.Bus.EventLog.MaxAge,
		MaxBytes:             cfg.Bus.EventLog.MaxSizeMB << 20,
		Log:                  log,
	}
	tlsOn := cfg.Bus.Listen != "" && cfg.Bus.TLS.Enabled
	if tlsOn {
		files, err := bus.EnsureTLS(tlsDir(cfg), bus.TLSHosts(cfg.Bus.Listen, cfg.Bus.TLS.ExtraHosts), time.Now())
		if err != nil {
			return nil, err
		}
		if opts.TLS, err = bus.ServerTLSConfig(files); err != nil {
			return nil, err
		}
	} else if cfg.Bus.Listen != "" && !isLoopback(cfg.Bus.Listen) {
		log.Warn("bus accepts network connections without TLS: the token travels in clear text", "listen", cfg.Bus.Listen)
	}

	srv, err := bus.Start(ctx, opts)
	if err != nil {
		return nil, err
	}
	listen := srv.ClientURL()
	if listen == "" {
		listen = "in-process only"
	}
	attrs := []any{"listen", listen, "tls", tlsOn, "token_file", tokenPath(cfg)}
	if tlsOn {
		attrs = append(attrs, "ca_file", bus.TLSFilesIn(tlsDir(cfg)).CA)
	}
	log.Info("bus ready", attrs...)
	return srv, nil
}

func isLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func tokenPath(cfg config.Config) string {
	return filepath.Join(cfg.DataDir, "bus.token")
}

func tlsDir(cfg config.Config) string {
	return filepath.Join(cfg.DataDir, "tls")
}
