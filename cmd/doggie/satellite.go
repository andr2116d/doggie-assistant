package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
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
  doggie satellite add -channel <channel> [-channel ...] <name>
  doggie satellite list
  doggie satellite remove <name>

A satellite (a voice satellite, the WhatsApp bridge...) gets its own token,
and the bus only lets it speak for its own channels. The token is shown once
and saved for this machine in $XDG_DATA_HOME/doggie/satellites/<name>.token;
connect with -as <name> or DOGGIE_BUS_USER=<name> plus DOGGIE_BUS_TOKEN.
Changes apply when DOGGIE restarts.

The shared bus token (doggie events, task, say) may only speak for the
"terminal" channel.

All subcommands also take -config.
`

// sharedChannels are the channels the shared bus token may speak for.
var sharedChannels = []string{"terminal"}

// satelliteEntry is a satellite in the registry. Only a hash of its token is
// kept.
type satelliteEntry struct {
	Name      string    `json:"name"`
	TokenHash string    `json:"token_hash"`
	Channels  []string  `json:"channels"`
	Added     time.Time `json:"added"`
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
		if len(s.Channels) == 0 {
			return nil, fmt.Errorf("satellites: %s has no channels", s.Name)
		}
		for _, c := range s.Channels {
			if !events.ValidChannel(c) {
				return nil, fmt.Errorf("satellites: %s has an invalid channel %q", s.Name, c)
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
		out = append(out, bus.Satellite{Name: s.Name, TokenHash: s.TokenHash, Permissions: events.SatellitePermissions(s.Channels...)})
	}
	return out
}

// channelFlags collects repeated -channel flags.
type channelFlags []string

func (c *channelFlags) String() string     { return strings.Join(*c, ",") }
func (c *channelFlags) Set(v string) error { *c = append(*c, v); return nil }

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
	var channels channelFlags
	if sub == "add" {
		flags.Var(&channels, "channel", "a channel the satellite speaks for (repeatable), e.g. voice:living_room")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	return satelliteSub(cfg, stdout, sub, flags.Args(), channels)
}

func satelliteSub(cfg config.Config, w io.Writer, sub string, args []string, channels []string) error {
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
		fmt.Fprintln(tw, "NAME\tCHANNELS\tADDED")
		fmt.Fprintf(tw, "(shared token)\t%s\t\n", strings.Join(sharedChannels, ", "))
		for _, s := range list {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Name, strings.Join(s.Channels, ", "), s.Added.Local().Format("2006-01-02 15:04"))
		}
		return tw.Flush()

	case "add":
		if len(args) != 1 {
			return errors.New("usage: doggie satellite add -channel <channel> [-channel ...] <name>")
		}
		name := args[0]
		if !bus.ValidSatelliteName(name) {
			return fmt.Errorf("invalid satellite name %q: use one word without dots or spaces (not core or satellite)", name)
		}
		if slices.ContainsFunc(list, func(s satelliteEntry) bool { return s.Name == name }) {
			return fmt.Errorf("satellite %q already exists: remove it first to issue a new token", name)
		}
		if len(channels) == 0 {
			return errors.New("a satellite needs at least one -channel")
		}
		for _, c := range channels {
			if _, err := events.UtteranceOn(c); err != nil {
				return err
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
		list = append(list, satelliteEntry{Name: name, TokenHash: hash, Channels: slices.Compact(slices.Sorted(slices.Values(channels))), Added: time.Now().UTC()})
		if err := saveSatellites(path, list); err != nil {
			os.Remove(tokenFile)
			return err
		}
		fmt.Fprintf(w, "added satellite %s for %s\ntoken: %s\nsaved in %s\nrestart doggie to apply\n", name, strings.Join(channels, ", "), token, tokenFile)
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
		fmt.Fprintf(w, "removed satellite %s\nrestart doggie to apply\n", args[0])
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
