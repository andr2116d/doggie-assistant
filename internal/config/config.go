// Package config loads DOGGIE's configuration.
//
// Personal settings live outside the repository, following the XDG base
// directory spec: the config file defaults to
// $XDG_CONFIG_HOME/doggie/config.yaml (~/.config/doggie/config.yaml) and
// runtime data to $XDG_DATA_HOME/doggie (~/.local/share/doggie).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"go.yaml.in/yaml/v3"
)

// Config is DOGGIE's configuration.
type Config struct {
	Owner     Owner     `yaml:"owner"`
	Assistant Assistant `yaml:"assistant"`
	// Language is the language DOGGIE speaks and understands (BCP 47, e.g. "en", "es").
	Language string `yaml:"language"`
	Log      Log    `yaml:"log"`
	Bus      Bus    `yaml:"bus"`
	Brain    Brain  `yaml:"brain"`

	// File is the resolved config file path.
	File string `yaml:"-"`
	// UsingDefaults is true when File did not exist and defaults were used.
	UsingDefaults bool `yaml:"-"`
	// DataDir is where DOGGIE keeps its runtime data.
	DataDir string `yaml:"-"`
}

// maxProfileText bounds the free-text profile fields, which go into every
// prompt.
const maxProfileText = 8 << 10

// Owner describes the person DOGGIE works for.
type Owner struct {
	Name string `yaml:"name"`
	// About is free text DOGGIE always knows about the owner: where they
	// live, what they like. It may be written in any language.
	About string `yaml:"about"`
}

// Assistant is who DOGGIE is.
type Assistant struct {
	Name string `yaml:"name"`
	// Persona is free text describing how DOGGIE talks and behaves. It may
	// be written in any language.
	Persona string `yaml:"persona"`
}

// Brain configures the LLM brain.
type Brain struct {
	// Provider is the LLM provider. Only "claudecode" exists for now.
	Provider string `yaml:"provider"`
	// Model is the provider's model name or alias, e.g. "sonnet".
	Model string `yaml:"model"`
	// HistoryTurns is how many recent turns of a thread go into the prompt.
	HistoryTurns int `yaml:"history_turns"`
	// TurnTimeout bounds one turn of the LLM, tool calls included.
	TurnTimeout time.Duration `yaml:"turn_timeout"`
	ClaudeCode  ClaudeCode    `yaml:"claudecode"`
}

// ClaudeCode configures the Claude Code provider.
type ClaudeCode struct {
	// Command is the claude executable, looked up in PATH if it has no
	// slash.
	Command string `yaml:"command"`
}

// Log configures logging.
type Log struct {
	// Level is one of debug, info, warn or error.
	Level string `yaml:"level"`
}

// Bus configures the embedded event bus.
type Bus struct {
	// Listen is the host:port where satellites connect, with a token.
	// Empty means in-process connections only.
	Listen   string   `yaml:"listen"`
	TLS      TLS      `yaml:"tls"`
	EventLog EventLog `yaml:"event_log"`
}

// TLS configures encryption for TCP connections to the bus. DOGGIE issues
// the certificates itself from its own CA.
type TLS struct {
	Enabled bool `yaml:"enabled"`
	// ExtraHosts are names or IPs the server certificate must cover beyond
	// the detected ones (localhost, hostname, interface IPs), e.g. a
	// Tailscale MagicDNS name.
	ExtraHosts []string `yaml:"extra_hosts"`
}

// EventLog bounds the persistent log of bus events. Older events are
// discarded first.
type EventLog struct {
	MaxAge    time.Duration `yaml:"max_age"`
	MaxSizeMB int64         `yaml:"max_size_mb"`
}

// Default returns the configuration used when no file exists.
func Default() Config {
	return Config{
		Assistant: Assistant{Name: "DOGGIE"},
		Language:  "en",
		Log:       Log{Level: "info"},
		Bus: Bus{
			Listen: "127.0.0.1:4222",
			TLS:    TLS{Enabled: true},
			EventLog: EventLog{
				MaxAge:    7 * 24 * time.Hour,
				MaxSizeMB: 512,
			},
		},
		Brain: Brain{
			Provider:     "claudecode",
			Model:        "sonnet",
			HistoryTurns: 20,
			TurnTimeout:  90 * time.Second,
			ClaudeCode:   ClaudeCode{Command: "claude"},
		},
	}
}

// Load reads the config file at path, or at DefaultFile when path is empty.
// A missing file at the default location is not an error: defaults are used.
// A missing file at an explicit path is.
func Load(path string) (Config, error) {
	cfg := Default()

	explicit := path != ""
	if !explicit {
		p, err := DefaultFile()
		if err != nil {
			return cfg, err
		}
		path = p
	}
	cfg.File = path

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := decode(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", path, err)
		}
	case errors.Is(err, fs.ErrNotExist) && !explicit:
		cfg.UsingDefaults = true
	default:
		return cfg, fmt.Errorf("read config: %w", err)
	}

	dataDir, err := DefaultDataDir()
	if err != nil {
		return cfg, err
	}
	cfg.DataDir = dataDir

	return cfg, cfg.Validate()
}

// decode parses YAML strictly so that typos in keys are reported.
func decode(data []byte, cfg *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// Validate reports the first invalid setting.
func (c Config) Validate() error {
	if c.Language == "" {
		return errors.New("config: language must not be empty")
	}
	if _, err := parseLevel(c.Log.Level); err != nil {
		return err
	}
	if c.Bus.Listen != "" {
		if _, _, err := net.SplitHostPort(c.Bus.Listen); err != nil {
			return fmt.Errorf("config: bus.listen %q must be host:port: %w", c.Bus.Listen, err)
		}
	}
	if c.Bus.EventLog.MaxAge <= 0 || c.Bus.EventLog.MaxSizeMB <= 0 {
		return errors.New("config: bus.event_log.max_age and max_size_mb must be positive")
	}
	if strings.TrimSpace(c.Assistant.Name) == "" {
		return errors.New("config: assistant.name must not be empty")
	}
	if len(c.Owner.About) > maxProfileText || len(c.Assistant.Persona) > maxProfileText {
		return fmt.Errorf("config: owner.about and assistant.persona must be at most %d bytes", maxProfileText)
	}
	return c.Brain.validate()
}

func (b Brain) validate() error {
	if b.Provider != "claudecode" {
		return fmt.Errorf("config: brain.provider %q is unknown (want claudecode)", b.Provider)
	}
	// The model is passed as a command-line argument: it must not look like
	// a flag.
	if b.Model == "" || strings.HasPrefix(b.Model, "-") || strings.ContainsFunc(b.Model, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return fmt.Errorf("config: brain.model %q must be a model name or alias", b.Model)
	}
	if b.HistoryTurns < 0 || b.HistoryTurns > 200 {
		return fmt.Errorf("config: brain.history_turns %d must be between 0 and 200", b.HistoryTurns)
	}
	if b.TurnTimeout < 5*time.Second || b.TurnTimeout > 10*time.Minute {
		return fmt.Errorf("config: brain.turn_timeout %s must be between 5s and 10m", b.TurnTimeout)
	}
	if b.ClaudeCode.Command == "" {
		return errors.New("config: brain.claudecode.command must not be empty")
	}
	return nil
}

// LogLevel returns the configured slog level.
func (c Config) LogLevel() slog.Level {
	l, _ := parseLevel(c.Log.Level)
	return l
}

func parseLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("config: log.level %q must be debug, info, warn or error", s)
}

// DefaultFile returns $XDG_CONFIG_HOME/doggie/config.yaml, falling back to
// ~/.config. XDG is used on every OS (including macOS) so the layout matches
// the Linux host DOGGIE runs on.
func DefaultFile() (string, error) {
	dir, err := xdgDir("XDG_CONFIG_HOME", ".config")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "doggie", "config.yaml"), nil
}

// DefaultDataDir returns $XDG_DATA_HOME/doggie, falling back to ~/.local/share.
func DefaultDataDir() (string, error) {
	dir, err := xdgDir("XDG_DATA_HOME", filepath.Join(".local", "share"))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "doggie"), nil
}

func xdgDir(env, fallback string) (string, error) {
	if v := os.Getenv(env); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, fallback), nil
}
