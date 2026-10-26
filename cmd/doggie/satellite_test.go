package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
)

func TestSatelliteAddListRemove(t *testing.T) {
	cfg := testConfig(t, "")
	var out strings.Builder
	if err := satelliteSub(cfg, &out, "add", []string{"voice-room"}, []string{"voice:living_room", "voice:kitchen", "voice:living_room"}, nil); err != nil {
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
	if err := satelliteSub(cfg, &out, "list", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "(shared token)  terminal") || !strings.Contains(out.String(), "voice-room") || strings.Contains(out.String(), "$2") {
		t.Errorf("list:\n%s", out.String())
	}

	if err := satelliteSub(cfg, io.Discard, "remove", []string{"voice-room"}, nil, nil); err != nil {
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
	if err := satelliteSub(cfg, io.Discard, "add", []string{"voice-room"}, []string{"voice:room"}, nil); err != nil {
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
		if err := satelliteSub(cfg, io.Discard, "add", c.args, c.channels, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if list, _ := loadSatellites(satellitesFile(cfg)); len(list) != 1 {
		t.Errorf("rejected adds changed the registry: %+v", list)
	}
	if err := satelliteSub(cfg, io.Discard, "remove", []string{"nobody"}, nil, nil); err == nil {
		t.Error("removed a satellite that doesn't exist")
	}
	if err := satelliteSub(cfg, io.Discard, "fly", nil, nil, nil); err == nil {
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

// reloads tells when watchSatellites has acted on a registry change, from
// its log.
type reloads struct{ applied, refused chan struct{} }

func (r reloads) Enabled(context.Context, slog.Level) bool { return true }
func (r reloads) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r reloads) WithGroup(string) slog.Handler            { return r }
func (r reloads) Handle(_ context.Context, rec slog.Record) error {
	switch {
	case strings.HasPrefix(rec.Message, "satellites: applied"):
		r.applied <- struct{}{}
	case strings.HasPrefix(rec.Message, "satellites: registry changed but is invalid"):
		r.refused <- struct{}{}
	}
	return nil
}

func (r reloads) wait(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for the registry to be %s", what)
	}
}

// doggie run applies registry changes without a restart, and keeps the
// satellites as they were when the registry turns invalid.
//
// The test waits for each reload, from the watcher's log, so it checks the
// result of each change.
func TestRegistryChangesApplyWhileRunning(t *testing.T) {
	cfg := testConfig(t, "127.0.0.1:-1")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := startBus(context.Background(), cfg, quiet)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	r := reloads{applied: make(chan struct{}, 10), refused: make(chan struct{}, 10)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		watchSatellites(ctx, srv, satellitesFile(cfg), slog.New(r), 20*time.Millisecond)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	dial := func(token string) (*bus.Conn, error) {
		return bus.Dial(srv.ClientURL(), bus.DialOptions{Name: "voice", User: "voice-room", Token: token, CAFile: caFile("", cfg), Log: quiet})
	}
	connects := func(token string) bool {
		c, err := dial(token)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}
	if connects("no-token-yet") {
		t.Fatal("connected before being added")
	}
	satelliteSub(cfg, io.Discard, "add", []string{"voice-room"}, []string{"voice:living_room"}, nil)

	r.wait(t, r.applied, "applied")
	token, err := bus.LoadToken(satelliteTokenFile(cfg, "voice-room"))
	if err != nil {
		t.Fatal(err)
	}
	if !connects(token) {
		t.Fatal("added satellite refused")
	}

	// A broken registry keeps it working.
	good, _ := os.ReadFile(satellitesFile(cfg))
	os.WriteFile(satellitesFile(cfg), []byte("{broken"), 0o600)
	r.wait(t, r.refused, "refused")
	if !connects(token) {
		t.Error("an invalid registry dropped the satellites")
	}
	os.WriteFile(satellitesFile(cfg), good, 0o600)
	r.wait(t, r.applied, "applied again")

	// Removed: refused, and the live connection loses its rights.
	live, err := dial(token)
	if err != nil {
		t.Fatal(err)
	}
	satelliteSub(cfg, io.Discard, "remove", []string{"voice-room"}, nil, nil)
	r.wait(t, r.applied, "applied without it")
	if connects(token) {
		t.Error("removed satellite connected")
	}
	if canSay(live, "voice:living_room") {
		t.Error("the live connection kept its rights")
	}
	// It keeps trying to reconnect: close it before the next reload.
	live.Close()

	// A deleted registry means no named satellites.
	os.Remove(satellitesFile(cfg))
	r.wait(t, r.applied, "applied once deleted")
}

// A satellite may only run tasks, with kinds and no channels.
func TestSatelliteWithKinds(t *testing.T) {
	cfg := testConfig(t, "")
	var out strings.Builder
	if err := satelliteSub(cfg, &out, "add", []string{"lights"}, nil, []string{"lights.set", "lights.off", "lights.set"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "task kinds: lights.off, lights.set") {
		t.Errorf("add said:\n%s", out.String())
	}
	list, err := loadSatellites(satellitesFile(cfg))
	if err != nil || len(list) != 1 || strings.Join(list[0].Kinds, ",") != "lights.off,lights.set" || len(list[0].Channels) != 0 {
		t.Fatalf("registry = %+v, %v", list, err)
	}
	out.Reset()
	if err := satelliteSub(cfg, &out, "list", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "lights          -         lights.off, lights.set") {
		t.Errorf("list:\n%s", out.String())
	}
	for name, kinds := range map[string][]string{
		"none":        nil,
		"spaced kind": {"lights set"},
		"wildcard":    {"lights.*"},
		"empty kind":  {""},
	} {
		if err := satelliteSub(cfg, io.Discard, "add", []string{"x-" + strings.ReplaceAll(name, " ", "-")}, nil, kinds); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A registry edited by hand is checked too.
	os.WriteFile(satellitesFile(cfg), []byte(`[{"name":"x","token_hash":"$2a$x","kinds":["a b"]}]`), 0o600)
	if _, err := loadSatellites(satellitesFile(cfg)); err == nil || !strings.Contains(err.Error(), "invalid task kind") {
		t.Errorf("hand-edited kind: %v", err)
	}
	os.WriteFile(satellitesFile(cfg), []byte(`[{"name":"x","token_hash":"$2a$x"}]`), 0o600)
	if _, err := loadSatellites(satellitesFile(cfg)); err == nil || !strings.Contains(err.Error(), "no channels or kinds") {
		t.Errorf("entry with nothing: %v", err)
	}
}
