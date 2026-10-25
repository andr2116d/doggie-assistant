package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestSatelliteAddListRemove(t *testing.T) {
	cfg := testConfig(t, "")
	var out strings.Builder
	if err := satelliteSub(cfg, &out, "add", []string{"voice-room"}, []string{"voice:living_room", "voice:kitchen", "voice:living_room"}); err != nil {
		t.Fatal(err)
	}
	list, err := loadSatellites(satellitesFile(cfg))
	if err != nil || len(list) != 1 {
		t.Fatalf("registry = %+v, %v", list, err)
	}
	s := list[0]
	if s.Name != "voice-room" || strings.Join(s.Channels, ",") != "voice:kitchen,voice:living_room" || !strings.HasPrefix(s.TokenHash, "$2") {
		t.Errorf("entry = %+v", s)
	}
	// Only a hash is kept in the registry; the token file is private.
	token, _ := os.ReadFile(satelliteTokenFile(cfg, "voice-room"))
	reg, _ := os.ReadFile(satellitesFile(cfg))
	if len(strings.TrimSpace(string(token))) != 64 || strings.Contains(string(reg), strings.TrimSpace(string(token))) {
		t.Error("the registry holds the token, or the token file is wrong")
	}
	for _, f := range []string{satellitesFile(cfg), satelliteTokenFile(cfg, "voice-room")} {
		if fi, err := os.Stat(f); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: mode %v, %v", f, fi.Mode(), err)
		}
	}

	out.Reset()
	if err := satelliteSub(cfg, &out, "list", nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "(shared token)  terminal") || !strings.Contains(out.String(), "voice-room") || strings.Contains(out.String(), "$2") {
		t.Errorf("list:\n%s", out.String())
	}

	if err := satelliteSub(cfg, io.Discard, "remove", []string{"voice-room"}, nil); err != nil {
		t.Fatal(err)
	}
	if list, _ := loadSatellites(satellitesFile(cfg)); len(list) != 0 {
		t.Errorf("still registered: %+v", list)
	}
	if _, err := os.Stat(satelliteTokenFile(cfg, "voice-room")); !os.IsNotExist(err) {
		t.Error("token file left behind")
	}
}

func TestSatelliteAddRejects(t *testing.T) {
	cfg := testConfig(t, "")
	if err := satelliteSub(cfg, io.Discard, "add", []string{"voice-room"}, []string{"voice:room"}); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		args, channels []string
	}{
		"no name":         {nil, []string{"x"}},
		"two names":       {[]string{"a", "b"}, []string{"x"}},
		"no channel":      {[]string{"voice-x"}, nil},
		"duplicate":       {[]string{"voice-room"}, []string{"x"}},
		"core":            {[]string{"core"}, []string{"x"}},
		"shared user":     {[]string{"satellite"}, []string{"x"}},
		"dotted name":     {[]string{"voice.x"}, []string{"x"}},
		"spaced name":     {[]string{"voice x"}, []string{"x"}},
		"path in name":    {[]string{"../evil"}, []string{"x"}},
		"dotted channel":  {[]string{"voice-y"}, []string{"voice.room"}},
		"spaced channel":  {[]string{"voice-y"}, []string{"voice room"}},
		"wildcard":        {[]string{"voice-y"}, []string{"*"}},
		"empty channel":   {[]string{"voice-y"}, []string{""}},
		"long channel":    {[]string{"voice-y"}, []string{strings.Repeat("c", 65)}},
		"one bad channel": {[]string{"voice-y"}, []string{"ok", "bad channel"}},
	} {
		if err := satelliteSub(cfg, io.Discard, "add", c.args, c.channels); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if list, _ := loadSatellites(satellitesFile(cfg)); len(list) != 1 {
		t.Errorf("rejected adds changed the registry: %+v", list)
	}
	if err := satelliteSub(cfg, io.Discard, "remove", []string{"nobody"}, nil); err == nil {
		t.Error("removed a satellite that doesn't exist")
	}
	if err := satelliteSub(cfg, io.Discard, "fly", nil, nil); err == nil {
		t.Error("unknown subcommand accepted")
	}
}

// DOGGIE must not start with a registry it can't check: it would run with
// permissions nobody meant.
func TestCorruptRegistryStopsTheBus(t *testing.T) {
	for name, content := range map[string]string{
		"not json":       "{",
		"bad name":       `[{"name":"core","token_hash":"$2a$04$x","channels":["terminal"]}]`,
		"dup names":      `[{"name":"a","token_hash":"$2a$04$x","channels":["t"]},{"name":"a","token_hash":"$2a$04$x","channels":["t"]}]`,
		"no channels":    `[{"name":"a","token_hash":"$2a$04$x","channels":[]}]`,
		"bad channel":    `[{"name":"a","token_hash":"$2a$04$x","channels":["a b"]}]`,
		"plain token":    `[{"name":"a","token_hash":"secret","channels":["t"]}]`,
		"wrong shape":    `{"name":"a"}`,
		"dotted channel": `[{"name":"a","token_hash":"$2a$04$x","channels":["voice.room"]}]`,
	} {
		cfg := testConfig(t, "127.0.0.1:-1")
		if err := os.WriteFile(satellitesFile(cfg), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if srv, err := startBus(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil {
			srv.Shutdown()
			t.Errorf("%s: bus started", name)
		}
	}
	// An empty registry is fine.
	cfg := testConfig(t, "127.0.0.1:-1")
	os.WriteFile(satellitesFile(cfg), []byte("[]"), 0o600)
	srv, err := startBus(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.Shutdown()
}

func TestLoadSatellitesChecksEveryEntry(t *testing.T) {
	cfg := testConfig(t, "")
	for name, content := range map[string]string{
		"reserved name": `[{"name":"core","token_hash":"$2a$04$x","channels":["t"]}]`,
		"dotted name":   `[{"name":"a.b","token_hash":"$2a$04$x","channels":["t"]}]`,
		"bad channel":   `[{"name":"a","token_hash":"$2a$04$x","channels":["a.b"]}]`,
	} {
		os.WriteFile(satellitesFile(cfg), []byte(content), 0o600)
		if _, err := loadSatellites(satellitesFile(cfg)); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
}
