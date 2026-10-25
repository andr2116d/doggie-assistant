package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/config"
)

// clientFlags are the flags of commands that talk to a running DOGGIE over
// TCP.
type clientFlags struct {
	config, server, ca *string
}

func addClientFlags(flags *flag.FlagSet) clientFlags {
	return clientFlags{
		config: flags.String("config", "", "config file (default $XDG_CONFIG_HOME/doggie/config.yaml)"),
		server: flags.String("server", "", "bus URL (default derived from bus.listen)"),
		ca:     flags.String("ca", "", "bus CA certificate (default $DOGGIE_BUS_CA, or the local one when bus.tls is enabled)"),
	}
}

// dial connects to the bus of a running DOGGIE and returns the connection
// and its URL.
func (f clientFlags) dial(name string) (*bus.Conn, string, error) {
	cfg, err := config.Load(*f.config)
	if err != nil {
		return nil, "", err
	}
	url := *f.server
	if url == "" {
		if url, err = localBusURL(cfg.Bus.Listen); err != nil {
			return nil, "", err
		}
	}
	token, err := bus.LoadToken(tokenPath(cfg))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, "", fmt.Errorf("no bus token at %s: start doggie once or set %s", tokenPath(cfg), bus.TokenEnv)
	}
	if err != nil {
		return nil, "", err
	}
	conn, err := bus.Dial(url, bus.DialOptions{
		Name:   name,
		Token:  token,
		CAFile: caFile(*f.ca, cfg),
		Log:    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	return conn, url, err
}

// caFile picks the CA a client trusts: the flag, then $DOGGIE_BUS_CA, then the
// local CA when this machine runs the bus with TLS. Empty means plain TCP.
func caFile(flag string, cfg config.Config) string {
	if flag != "" {
		return flag
	}
	if env := os.Getenv(bus.CAEnv); env != "" {
		return env
	}
	if cfg.Bus.TLS.Enabled {
		return bus.TLSFilesIn(tlsDir(cfg)).CA
	}
	return ""
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
