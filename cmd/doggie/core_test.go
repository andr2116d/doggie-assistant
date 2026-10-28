package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/andr2116d/doggie-assistant/internal/config"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

// localDoggie prepares a config whose bus listens on a free local port, as a
// DOGGIE on this machine would, and returns the client flags that find it.
func localDoggie(t *testing.T) clientFlags {
	t.Helper()
	t.Setenv(bus.TokenEnv, "")
	t.Setenv(bus.CAEnv, "")
	t.Setenv(userEnv, "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	path := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(path, []byte(fmt.Sprintf("bus:\n  listen: \"127.0.0.1:%d\"\n", port)), 0o600)
	return clientFlagsFor(path)
}

// noBrain are the components a test can run without the claude CLI.
func noBrain(t *testing.T) []factory {
	t.Helper()
	fs, err := selectFactories(factories(), "", "brain")
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestStartHereRunsDoggieUntilStopped(t *testing.T) {
	client := localDoggie(t)
	ui := newFakeUI(true)
	states := make(chan connState, 10)
	// Never ran here: there isn't even a token yet.
	if _, _, err := dialChat(client, ui, states); !errors.Is(err, errNoToken) {
		t.Fatalf("dial before starting: %v", err)
	}
	host := newHosting(*client.config, noBrain(t))
	conn, _, err := startHere(context.Background(), ui, client, states, host, nil)
	own := host.own
	if err != nil || own == nil {
		t.Fatalf("startHere: %v, own %v", err, own)
	}
	// It works: the task manager answers.
	if _, err := bus.Request(context.Background(), conn, events.ListTasks, events.TaskListRequest{}); err != nil {
		t.Errorf("list tasks: %v", err)
	}
	if !strings.Contains(ui.printed(), "started it here") {
		t.Errorf("printed %q", ui.printed())
	}
	conn.Close()
	host.close()
	select {
	case <-own.Done():
	default:
		t.Fatal("not stopped")
	}
	if _, _, err := dialChat(client, ui, states); !errors.Is(err, bus.ErrNoServer) {
		t.Errorf("dial after stopping: %v", err)
	}
	data, _ := os.ReadFile(filepath.Join(os.Getenv("XDG_DATA_HOME"), "doggie", "doggie.log"))
	if !strings.Contains(string(data), "doggie stopped") {
		t.Errorf("log file: %q", data)
	}
}

// Two chats starting at once: one runs DOGGIE, the other talks to it.
func TestStartHereRace(t *testing.T) {
	client := localDoggie(t)
	selected := noBrain(t)
	type res struct {
		host *hosting
		conn *bus.Conn
		err  error
	}
	results := make(chan res, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			host := newHosting(*client.config, selected)
			conn, _, err := startHere(context.Background(), newFakeUI(false), client, make(chan connState, 10), host, nil)
			results <- res{host, conn, err}
		})
	}
	wg.Wait()
	close(results)
	owners := 0
	var all []res
	for r := range results {
		if r.err != nil {
			t.Fatalf("startHere: %v", r.err)
		}
		if r.host.own != nil {
			owners++
		}
		all = append(all, r)
	}
	if owners != 1 {
		t.Errorf("%d processes run DOGGIE", owners)
	}
	for _, r := range all {
		if _, err := bus.Request(context.Background(), r.conn, events.ListTasks, events.TaskListRequest{}); err != nil {
			t.Errorf("list tasks: %v", err)
		}
		r.conn.Close()
	}
	for _, r := range all {
		r.host.close()
	}
}

// The chat of doggie run ends when its DOGGIE stops.
func TestChatEndsWhenItsDoggieStops(t *testing.T) {
	client := localDoggie(t)
	ui := newFakeUI(true)
	states := make(chan connState, 10)
	cfg, err := config.Load(*client.config)
	if err != nil {
		t.Fatal(err)
	}
	own, err := startCore(context.Background(), cfg, slog.New(slog.DiscardHandler), noBrain(t), roleRun)
	if err != nil {
		t.Fatal(err)
	}
	conn, _, err := dialChat(client, ui, states)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	done := make(chan error, 1)
	go func() { done <- chatSession(context.Background(), conn, ui, states, own, testChatOptions()) }()
	time.Sleep(200 * time.Millisecond)
	own.Stop(events.StopOnPurpose)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(ui.printed(), "DOGGIE ended the connection") {
			t.Errorf("err %v, printed %q", err, ui.printed())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("chat kept going after its DOGGIE stopped")
	}
}

func TestOpenLogStartsOverWhenBig(t *testing.T) {
	cfg := testConfig(t, "")
	path := filepath.Join(cfg.DataDir, "doggie.log")
	os.WriteFile(path, make([]byte, maxLogBytes+1), 0o600)
	f, got, err := openLog(cfg)
	if err != nil || got != path {
		t.Fatalf("openLog: %q, %v", got, err)
	}
	f.Close()
	if st, _ := os.Stat(path); st.Size() != 0 {
		t.Errorf("log kept %d bytes", st.Size())
	}
	if st, err := os.Stat(path + ".1"); err != nil || st.Size() != maxLogBytes+1 {
		t.Errorf("previous log: %v", err)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("log mode %v", st.Mode().Perm())
	}
}
