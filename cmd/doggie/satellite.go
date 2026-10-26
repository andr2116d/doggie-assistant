package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/config"
)

const satelliteUsage = `Usage:
  doggie satellite add [-channel <channel> ...] [-kind <kind> ...] <name>
  doggie satellite list
  doggie satellite remove <name>

A satellite (a voice satellite, the WhatsApp bridge...) gets its own token,
and the bus only lets it speak for its own channels and run its own kinds of
tasks, as a worker named after it. It needs at least one of the two. The
token is shown once
and saved for this machine in $XDG_DATA_HOME/doggie/satellites/<name>.token;
connect with -as <name> or DOGGIE_BUS_USER=<name> plus DOGGIE_BUS_TOKEN.
A running DOGGIE applies changes within a few seconds: a removed satellite
is disconnected.

The shared bus token (doggie events, task, say) may only speak for the
"terminal" channel.

All subcommands also take -config.
`

// sharedChannels are the channels the shared bus token may speak for.
var sharedChannels = []string{"terminal"}

// satelliteEntry is a satellite in the registry. Only a hash of its token is
// kept.
type satelliteEntry struct {
	Name      string   `json:"name"`
	TokenHash string   `json:"token_hash"`
	Channels  []string `json:"channels,omitempty"`
	// Kinds are the task kinds it runs as a worker.
	Kinds []string  `json:"kinds,omitempty"`
	Added time.Time `json:"added"`
}

func satellitesFile(cfg config.Config) string {
	return filepath.Join(cfg.DataDir, "satellites.json")
}

func satelliteTokenFile(cfg config.Config, name string) string {
	return filepath.Join(cfg.DataDir, "satellites", name+".token")
}

// loadSatellites reads the registry. A missing registry is empty; an invalid
// one is an error, so DOGGIE doesn't start with permissions it can't check.
func loadSatellites(path string) ([]satelliteEntry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("satellites: %w", err)
	}
	var list []satelliteEntry
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("satellites: parse %s: %w", path, err)
	}
	seen := map[string]bool{}
	for _, s := range list {
		if !bus.ValidSatelliteName(s.Name) || seen[s.Name] {
			return nil, fmt.Errorf("satellites: invalid or repeated name %q in %s", s.Name, path)
		}
		seen[s.Name] = true
		if len(s.Channels) == 0 && len(s.Kinds) == 0 {
			return nil, fmt.Errorf("satellites: %s has no channels or kinds", s.Name)
		}
		for _, c := range s.Channels {
			if !events.ValidChannel(c) {
				return nil, fmt.Errorf("satellites: %s has an invalid channel %q", s.Name, c)
			}
		}
		for _, k := range s.Kinds {
			if !bus.ValidName(k) {
				return nil, fmt.Errorf("satellites: %s has an invalid task kind %q", s.Name, k)
			}
		}
	}
	return list, nil
}

func saveSatellites(path string, list []satelliteEntry) error {
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Write then rename, so a crash never leaves half a registry.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// busSatellites turns the registry into the bus's named satellites.
func busSatellites(list []satelliteEntry) []bus.Satellite {
	out := make([]bus.Satellite, 0, len(list))
	for _, s := range list {
		out = append(out, bus.Satellite{Name: s.Name, TokenHash: s.TokenHash, Permissions: events.SatellitePermissions(s.Name, s.Channels, s.Kinds)})
	}
	return out
}

// listFlags collects a repeated flag.
type listFlags []string

func (c *listFlags) String() string     { return strings.Join(*c, ",") }
func (c *listFlags) Set(v string) error { *c = append(*c, v); return nil }

// sortedSet is list sorted, without repeats, and nil if empty.
func sortedSet(list []string) []string {
	if len(list) == 0 {
		return nil
	}
	return slices.Compact(slices.Sorted(slices.Values(list)))
}

func orDash(list []string) string {
	if len(list) == 0 {
		return "-"
	}
	return strings.Join(list, ", ")
}

func satelliteCmd(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(stdout, satelliteUsage)
		if len(args) == 0 {
			return errors.New("missing satellite subcommand")
		}
		return nil
	}
	sub := args[0]
	flags := flag.NewFlagSet("satellite "+sub, flag.ContinueOnError)
	configPath := flags.String("config", "", "config file (default $XDG_CONFIG_HOME/doggie/config.yaml)")
	var channels, kinds listFlags
	if sub == "add" {
		flags.Var(&channels, "channel", "a channel the satellite speaks for (repeatable), e.g. voice:living_room")
		flags.Var(&kinds, "kind", "a task kind the satellite runs as a worker (repeatable), e.g. lights.set")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	return satelliteSub(cfg, stdout, sub, flags.Args(), channels, kinds)
}

func satelliteSub(cfg config.Config, w io.Writer, sub string, args []string, channels, kinds []string) error {
	path := satellitesFile(cfg)
	list, err := loadSatellites(path)
	if err != nil {
		return err
	}
	switch sub {
	case "list":
		if len(args) != 0 {
			return errors.New("usage: doggie satellite list")
		}
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tCHANNELS\tKINDS\tADDED")
		fmt.Fprintf(tw, "(shared token)\t%s\t-\t\n", strings.Join(sharedChannels, ", "))
		for _, s := range list {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Name, orDash(s.Channels), orDash(s.Kinds), s.Added.Local().Format("2006-01-02 15:04"))
		}
		return tw.Flush()

	case "add":
		if len(args) != 1 {
			return errors.New("usage: doggie satellite add [-channel <channel> ...] [-kind <kind> ...] <name>")
		}
		name := args[0]
		if !bus.ValidSatelliteName(name) {
			return fmt.Errorf("invalid satellite name %q: use one word without dots or spaces (not core or satellite)", name)
		}
		if slices.ContainsFunc(list, func(s satelliteEntry) bool { return s.Name == name }) {
			return fmt.Errorf("satellite %q already exists: remove it first to issue a new token", name)
		}
		if len(channels) == 0 && len(kinds) == 0 {
			return errors.New("a satellite needs at least one -channel or -kind")
		}
		for _, c := range channels {
			if _, err := events.UtteranceOn(c); err != nil {
				return err
			}
		}
		for _, k := range kinds {
			if !bus.ValidName(k) {
				return fmt.Errorf("invalid task kind %q", k)
			}
		}
		token, err := newToken()
		if err != nil {
			return err
		}
		hash, err := bus.HashToken(token)
		if err != nil {
			return err
		}
		tokenFile := satelliteTokenFile(cfg, name)
		if err := os.MkdirAll(filepath.Dir(tokenFile), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
			return err
		}
		list = append(list, satelliteEntry{Name: name, TokenHash: hash, Channels: sortedSet(channels), Kinds: sortedSet(kinds), Added: time.Now().UTC()})
		if err := saveSatellites(path, list); err != nil {
			os.Remove(tokenFile)
			return err
		}
		fmt.Fprintf(w, "added satellite %s (channels: %s; task kinds: %s)\ntoken: %s\nsaved in %s\na running doggie applies it within a few seconds\n", name, orDash(sortedSet(channels)), orDash(sortedSet(kinds)), token, tokenFile)
		return nil

	case "remove":
		if len(args) != 1 {
			return errors.New("usage: doggie satellite remove <name>")
		}
		i := slices.IndexFunc(list, func(s satelliteEntry) bool { return s.Name == args[0] })
		if i < 0 {
			return fmt.Errorf("no satellite %q", args[0])
		}
		list = slices.Delete(list, i, i+1)
		if err := saveSatellites(path, list); err != nil {
			return err
		}
		os.Remove(satelliteTokenFile(cfg, args[0]))
		fmt.Fprintf(w, "removed satellite %s\na running doggie disconnects it within a few seconds\n", args[0])
		return nil
	}
	return fmt.Errorf("unknown satellite subcommand %q (see \"doggie satellite help\")", sub)
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// satelliteWatchInterval is how often doggie run checks the registry.
const satelliteWatchInterval = 2 * time.Second

// watchSatellites applies registry changes to the running bus, so adding or
// removing a satellite needs no restart. An invalid registry is reported and
// leaves the satellites as they were.
func watchSatellites(ctx context.Context, srv *bus.Server, path string, log *slog.Logger, every time.Duration) {
	last, _ := os.ReadFile(path)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			log.Warn("satellites: cannot read the registry", "err", err)
			continue
		}
		if bytes.Equal(data, last) {
			continue
		}
		last = data
		list, err := loadSatellites(path)
		if err == nil {
			err = srv.SetSatellites(busSatellites(list))
		}
		if err != nil {
			log.Error("satellites: registry changed but is invalid, keeping the previous satellites", "err", err)
			continue
		}
		log.Info("satellites: applied the registry", "satellites", len(list))
	}
}
