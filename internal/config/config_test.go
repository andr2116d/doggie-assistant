package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		{name: "unknown key", content: "langauge: es\n", wantErr: "langauge"},
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
