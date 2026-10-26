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
	"runtime/debug"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/config"
	"github.com/andr2116d/doggie-assistant/internal/console"
)

// version is set at build time with -ldflags "-X main.version=..."; without
// it, it comes from the build info, e.g. with go install.
var version = ""

const usage = `doggie: a self-hosted, Jarvis-style personal assistant.

Usage:
  doggie [flags]          talk with DOGGIE, starting it if needed: same as doggie chat
  doggie run [flags]      run DOGGIE; in a terminal, with its chat (see "doggie run -h")
  doggie events [flags]   watch bus events live (see "doggie events -h")
  doggie task <cmd>       start, list, get, cancel or answer tasks (see "doggie task help")
  doggie chat [flags]     talk with DOGGIE, starting it if needed (see "doggie chat -h")
  doggie say <text>       say something to DOGGIE and print its replies (see "doggie say -h")
  doggie satellite <cmd>  add, list or remove satellites (see "doggie satellite help")
  doggie login            give DOGGIE a backup Claude login (see "doggie login -h")
  doggie logout           forget it
  doggie version          print the version
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		if !errors.Is(err, flag.ErrHelp) && !errors.Is(err, errShown) {
			fmt.Fprintln(os.Stderr, "doggie:", err)
		}
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	// Like most chat tools, plain doggie talks: doggie -history 3 too.
	if len(args) == 0 || (strings.HasPrefix(args[0], "-") && !isHelp(args[0])) {
		return chatCmd(args)
	}
	switch args[0] {
	case "run":
		return runCmd(args[1:])
	case "events":
		return eventsCmd(args[1:], stdout)
	case "task":
		return taskCmd(args[1:], stdout)
	case "say":
		return sayCmd(args[1:], stdout)
	case "chat":
		return chatCmd(args[1:])
	case "satellite":
		return satelliteCmd(args[1:], stdout)
	case "login", "logout":
		return loginCmd(args[0], args[1:], os.Stdin, stdout, os.Stderr, realLogin)
	case "version":
		fmt.Fprintln(stdout, versionString())
		return nil
	case "help", "-h", "-help", "--help":
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
	noChat := fs.Bool("no-chat", false, "in a terminal, print the logs instead of opening the chat")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
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

	// In a terminal, DOGGIE opens its chat there, and the logs go to a file.
	if !*noChat && console.IsTerminal(os.Stdin, os.Stdout) && slices.ContainsFunc(selected, func(f factory) bool { return f.name == "brain" }) {
		return runWithChat(ctx, cfg, selected, clientFlagsFor(*configPath))
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel()}))
	if cfg.UsingDefaults {
		log.Warn("no config file, using defaults", "expected", cfg.File)
	}
	c, err := startCore(ctx, cfg, log, selected, roleRun)
	if err != nil {
		return err
	}
	select {
	case <-c.Done():
		return c.err
	case <-ctx.Done():
		return c.Stop(events.StopOnPurpose)
	}
}

func startBus(ctx context.Context, cfg config.Config, log *slog.Logger) (*bus.Server, error) {
	token, err := bus.LoadOrCreateToken(tokenPath(cfg))
	if err != nil {
		return nil, err
	}
	satellites, err := loadSatellites(satellitesFile(cfg))
	if err != nil {
		return nil, err
	}
	opts := bus.Options{
		Listen:               cfg.Bus.Listen,
		Token:                token,
		SatellitePermissions: events.SatellitePermissions(sharedChannels...),
		Satellites:           busSatellites(satellites),
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

func isHelp(arg string) bool {
	return arg == "-h" || arg == "-help" || arg == "--help"
}

// versionString is the version set at build time, or else the module's
// version, or the commit it was built from.
func versionString() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	return buildVersion(info)
}

func buildVersion(info *debug.BuildInfo) string {
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "dev"
	}
	rev = rev[:min(len(rev), 7)]
	if dirty {
		rev += "-dirty"
	}
	return rev
}
