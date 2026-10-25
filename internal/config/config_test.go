package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDefaultLocationMissingUsesDefaults(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", "/data")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.UsingDefaults {
		t.Error("UsingDefaults = false, want true")
	}
	if cfg.Language != "en" || cfg.Log.Level != "info" {
		t.Errorf("got language=%q level=%q, want defaults", cfg.Language, cfg.Log.Level)
	}
	if want := filepath.Join("/data", "doggie"); cfg.DataDir != want {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, want)
	}
}

func TestLoadReadsDefaultLocation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "doggie"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "doggie"), "language: es\n")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.UsingDefaults || cfg.Language != "es" {
		t.Errorf("got UsingDefaults=%v language=%q, want false and es", cfg.UsingDefaults, cfg.Language)
	}
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr string
		check   func(t *testing.T, cfg Config)
	}{
		{
			name:    "full file",
			content: "owner:\n  name: Alex\nlanguage: es\nlog:\n  level: debug\n",
			check: func(t *testing.T, cfg Config) {
				if cfg.Owner.Name != "Alex" || cfg.Language != "es" || cfg.Log.Level != "debug" {
					t.Errorf("unexpected config: %+v", cfg)
				}
			},
		},
		{
			name:    "partial file keeps defaults",
			content: "owner:\n  name: Alex\n",
			check: func(t *testing.T, cfg Config) {
				if cfg.Language != "en" || cfg.Log.Level != "info" {
					t.Errorf("defaults lost: %+v", cfg)
				}
			},
		},
		{
			name:    "empty file",
			content: "",
			check: func(t *testing.T, cfg Config) {
				if cfg.Language != "en" {
					t.Errorf("language = %q, want en", cfg.Language)
				}
			},
		},
		{
			name:    "bus settings",
			content: "bus:\n  listen: \"0.0.0.0:4222\"\n  event_log:\n    max_age: 24h\n    max_size_mb: 64\n",
			check: func(t *testing.T, cfg Config) {
				if cfg.Bus.Listen != "0.0.0.0:4222" || cfg.Bus.EventLog.MaxAge != 24*time.Hour || cfg.Bus.EventLog.MaxSizeMB != 64 {
					t.Errorf("unexpected bus config: %+v", cfg.Bus)
				}
			},
		},
		{
			name:    "bus in-process only",
			content: "bus:\n  listen: \"\"\n",
			check: func(t *testing.T, cfg Config) {
				if cfg.Bus.Listen != "" || cfg.Bus.EventLog.MaxSizeMB != 512 {
					t.Errorf("unexpected bus config: %+v", cfg.Bus)
				}
			},
		},
		{
			name:    "tls extra hosts keep tls enabled",
			content: "bus:\n  tls:\n    extra_hosts: [\"doggie.tailnet.ts.net\"]\n",
			check: func(t *testing.T, cfg Config) {
				if !cfg.Bus.TLS.Enabled || len(cfg.Bus.TLS.ExtraHosts) != 1 {
					t.Errorf("unexpected tls config: %+v", cfg.Bus.TLS)
				}
			},
		},
		{
			name:    "tls disabled",
			content: "bus:\n  tls:\n    enabled: false\n",
			check: func(t *testing.T, cfg Config) {
				if cfg.Bus.TLS.Enabled {
					t.Error("TLS still enabled")
				}
			},
		},
		{
			name:    "persona and profile",
			content: "owner:\n  name: Alex\n  about: |\n    Lives by the sea.\nassistant:\n  name: Rex\n  persona: Dry humor.\n",
			check: func(t *testing.T, cfg Config) {
				if cfg.Owner.About != "Lives by the sea.\n" || cfg.Assistant.Name != "Rex" || cfg.Assistant.Persona != "Dry humor." {
					t.Errorf("unexpected persona: %+v %+v", cfg.Owner, cfg.Assistant)
				}
			},
		},
		{
			name:    "brain defaults",
			content: "",
			check: func(t *testing.T, cfg Config) {
				b := cfg.Brain
				if cfg.Assistant.Name != "DOGGIE" || b.Provider != "claudecode" || b.Model != "sonnet" ||
					b.HistoryTurns != 20 || b.TurnTimeout != 90*time.Second || b.ClaudeCode.Command != "claude" {
					t.Errorf("unexpected defaults: %+v %+v", cfg.Assistant, b)
				}
			},
		},
		{
			name:    "brain settings",
			content: "brain:\n  model: \"claude-opus-5-5[1m]\"\n  history_turns: 0\n  turn_timeout: 2m\n  claudecode:\n    command: /opt/claude/bin/claude\n",
			check: func(t *testing.T, cfg Config) {
				b := cfg.Brain
				if b.Model != "claude-opus-5-5[1m]" || b.HistoryTurns != 0 || b.TurnTimeout != 2*time.Minute || b.ClaudeCode.Command != "/opt/claude/bin/claude" {
					t.Errorf("unexpected brain config: %+v", b)
				}
			},
		},
		{name: "unknown key", content: "langauge: es\n", wantErr: "langauge"},
		{name: "empty assistant name", content: "assistant:\n  name: \" \"\n", wantErr: "assistant.name"},
		{name: "huge persona", content: "assistant:\n  persona: " + strings.Repeat("x", maxProfileText+1) + "\n", wantErr: "assistant.persona"},
		{name: "huge about", content: "owner:\n  about: " + strings.Repeat("x", maxProfileText+1) + "\n", wantErr: "owner.about"},
		{name: "unknown provider", content: "brain:\n  provider: gpt\n", wantErr: "brain.provider"},
		{name: "empty model", content: "brain:\n  model: \"\"\n", wantErr: "brain.model"},
		{name: "model looks like a flag", content: "brain:\n  model: \"--dangerously-skip-permissions\"\n", wantErr: "brain.model"},
		{name: "model with spaces", content: "brain:\n  model: \"sonnet --tools default\"\n", wantErr: "brain.model"},
		{name: "model with control characters", content: "brain:\n  model: \"sonnet\\x00\"\n", wantErr: "brain.model"},
		{
			name:    "empty brain section keeps defaults",
			content: "brain:\n",
			check: func(t *testing.T, cfg Config) {
				if cfg.Brain.Model != "sonnet" {
					t.Errorf("brain defaults lost: %+v", cfg.Brain)
				}
			},
		},
		{name: "negative history", content: "brain:\n  history_turns: -1\n", wantErr: "history_turns"},
		{name: "huge history", content: "brain:\n  history_turns: 201\n", wantErr: "history_turns"},
		{name: "short turn timeout", content: "brain:\n  turn_timeout: 1s\n", wantErr: "turn_timeout"},
		{name: "long turn timeout", content: "brain:\n  turn_timeout: 11m\n", wantErr: "turn_timeout"},
		{name: "empty claude command", content: "brain:\n  claudecode:\n    command: \"\"\n", wantErr: "claudecode.command"},
		{name: "bad listen", content: "bus:\n  listen: \"4222\"\n", wantErr: "bus.listen"},
		{name: "bad event log", content: "bus:\n  event_log:\n    max_size_mb: 0\n", wantErr: "event_log"},
		{name: "invalid level", content: "log:\n  level: loud\n", wantErr: "log.level"},
		{name: "empty language", content: "language: \"\"\n", wantErr: "language"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Load(writeFile(t, t.TempDir(), tt.content))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tt.check(t, cfg)
		})
	}
}

func TestLoadExplicitMissingIsError(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected an error for a missing explicit config file")
	}
}

// The example shipped with the repo must stay loadable as the config grows.
func TestExampleConfigLoads(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cfg, err := Load(filepath.Join("..", "..", "configs", "doggie.example.yaml"))
	if err != nil {
		t.Fatalf("Load example: %v", err)
	}
	if cfg.Assistant.Persona == "" || cfg.Owner.About == "" || cfg.Brain.Model == "" {
		t.Errorf("example lost its persona or brain settings: %+v", cfg)
	}
}
