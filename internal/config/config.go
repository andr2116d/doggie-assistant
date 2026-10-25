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
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v3"
)

// Config is DOGGIE's configuration.
type Config struct {
	Owner Owner `yaml:"owner"`
	// Language is the language DOGGIE speaks and understands (BCP 47, e.g. "en", "es").
	Language string `yaml:"language"`
	Log      Log    `yaml:"log"`

	// File is the resolved config file path.
	File string `yaml:"-"`
	// UsingDefaults is true when File did not exist and defaults were used.
	UsingDefaults bool `yaml:"-"`
	// DataDir is where DOGGIE keeps its runtime data.
	DataDir string `yaml:"-"`
}

// Owner describes the person DOGGIE works for.
type Owner struct {
	Name string `yaml:"name"`
}

// Log configures logging.
type Log struct {
	// Level is one of debug, info, warn or error.
	Level string `yaml:"level"`
}

// Default returns the configuration used when no file exists.
func Default() Config {
	return Config{
		Language: "en",
		Log:      Log{Level: "info"},
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
