package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/config"
	"github.com/andr2116d/doggie-assistant/internal/task/manager"
	"github.com/andr2116d/doggie-assistant/internal/task/worker"
)

func testConfig(t *testing.T, listen string) config.Config {
	t.Helper()
	t.Setenv(bus.TokenEnv, "")
	t.Setenv(bus.CAEnv, "")
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Bus.Listen = listen
	return cfg
}

func TestStartBusWithTLS(t *testing.T) {
	cfg := testConfig(t, "127.0.0.1:-1")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := startBus(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	token, err := bus.LoadToken(tokenPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	url := srv.ClientURL()
	if _, err := bus.Dial(url, bus.DialOptions{Name: "plain", Token: token, Log: log}); err == nil {
		t.Error("dial without the CA succeeded although TLS is enabled")
	}
	c, err := bus.Dial(url, bus.DialOptions{Name: "viewer", Token: token, CAFile: caFile("", cfg), Log: log})
	if err != nil {
		t.Fatalf("dial with the local CA: %v", err)
	}
	c.Close()
}

func TestStartBusInProcessOnly(t *testing.T) {
	cfg := testConfig(t, "")
	srv, err := startBus(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	if url := srv.ClientURL(); url != "" {
		t.Errorf("ClientURL = %q, want empty", url)
	}
	if _, err := os.Stat(tlsDir(cfg)); !os.IsNotExist(err) {
		t.Error("TLS files were created although the bus does not listen on TCP")
	}
}

func TestStartBusRestrictsSatellites(t *testing.T) {
	cfg := testConfig(t, "127.0.0.1:-1")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := startBus(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	token, _ := bus.LoadToken(tokenPath(cfg))
	sat, err := bus.Dial(srv.ClientURL(), bus.DialOptions{Name: "satellite", Token: token, CAFile: caFile("", cfg), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer sat.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// The shared token runs no tasks, forges no task events and can't
	// impersonate the task manager.
	if _, err := bus.Serve(ctx, sat, events.RunTask.With("reminder.set")); err == nil {
		t.Error("the shared token could serve as a worker")
	}
	if published(sat, events.TaskDone, events.TaskDoneData{TaskID: "t1"}) {
		t.Error("satellite could publish task.done")
	}
	if _, err := bus.Serve(ctx, sat, events.StartTask); err == nil {
		t.Error("satellite could impersonate the task manager")
	}
	// The shared token speaks for the terminal...
	if !canSay(sat, "terminal") {
		t.Error("the shared token cannot speak for the terminal")
	}
	// ...but for no other channel, and not for the brain.
	for _, ch := range []string{"voice:living_room", "whatsapp"} {
		if canSay(sat, ch) {
			t.Errorf("the shared token could speak for %s", ch)
		}
	}
	if published(sat, events.OutSpeak, events.SpeakData{Channel: "terminal", Text: "forged"}) {
		t.Error("satellite could publish out.speak")
	}
	if published(sat, events.BrainTurn, events.BrainTurnData{ThreadID: "terminal"}) {
		t.Error("satellite could publish brain.turn")
	}
	if published(sat, events.UserUtterance, events.UtteranceData{Channel: "terminal", Text: "no channel subject"}) {
		t.Error("satellite could publish an utterance outside a channel subject")
	}

	// It cancels tasks from its own channel only, never as DOGGIE does.
	core, err := srv.Connect("tasks")
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	direct, _ := bus.ServeUnder(ctx, core, events.CancelTaskFrom)
	plain, _ := bus.Serve(ctx, core, events.CancelTask)
	go func() {
		for {
			select {
			case in := <-direct:
				in.Reply(events.Ack{})
			case in := <-plain:
				in.Reply(events.Ack{})
			case <-ctx.Done():
				return
			}
		}
	}()
	canCancel := func(cmd bus.Command[events.TaskCancelRequest, events.Ack]) bool {
		rctx, rcancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer rcancel()
		_, err := bus.Request(rctx, sat, cmd, events.TaskCancelRequest{TaskID: "t1"})
		return err == nil
	}
	fromTerminal, _ := events.CancelTaskOn("terminal")
	fromWhatsApp, _ := events.CancelTaskOn("whatsapp")
	if !canCancel(fromTerminal) {
		t.Error("the shared token cannot cancel from the terminal")
	}
	if canCancel(fromWhatsApp) {
		t.Error("the shared token could cancel from whatsapp")
	}
	if canCancel(events.CancelTask) {
		t.Error("satellite could cancel as DOGGIE does")
	}

	// It may ask the brain for a conversation's last turns.
	histories, _ := bus.Serve(ctx, core, events.BrainHistory)
	go func() {
		if in, ok := <-histories; ok {
			in.Reply(events.HistoryReply{Known: true})
		}
	}()
	if r, err := bus.Request(ctx, sat, events.BrainHistory, events.HistoryRequest{ThreadID: "terminal"}); err != nil || !r.Known {
		t.Errorf("satellite cannot ask for the history: %+v, %v", r, err)
	}
}

// A satellite added to the registry speaks for its own channels only, with
// its own token.
func TestRegisteredSatelliteSpeaksForItsChannels(t *testing.T) {
	cfg := testConfig(t, "127.0.0.1:-1")
	var out strings.Builder
	if err := satelliteSub(cfg, &out, "add", []string{"voice-room"}, []string{"voice:living_room"}, nil); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := startBus(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	token, err := bus.LoadToken(satelliteTokenFile(cfg, "voice-room"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), token) {
		t.Error("the token was not shown")
	}
	voice, err := bus.Dial(srv.ClientURL(), bus.DialOptions{Name: "voice", User: "voice-room", Token: token, CAFile: caFile("", cfg), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer voice.Close()
	if !canSay(voice, "voice:living_room") {
		t.Error("satellite cannot speak for its channel")
	}
	if canSay(voice, "terminal") || canSay(voice, "voice:kitchen") {
		t.Error("satellite spoke for another channel")
	}
	// Its token is no use as the shared one, and the shared one is no use
	// as its.
	shared, _ := bus.LoadToken(tokenPath(cfg))
	if c, err := bus.Dial(srv.ClientURL(), bus.DialOptions{Name: "x", Token: token, CAFile: caFile("", cfg), Log: log}); err == nil {
		c.Close()
		t.Error("the satellite token works as the shared token")
	}
	if c, err := bus.Dial(srv.ClientURL(), bus.DialOptions{Name: "x", User: "voice-room", Token: shared, CAFile: caFile("", cfg), Log: log}); err == nil {
		c.Close()
		t.Error("the shared token works for the satellite")
	}
}

func canSay(c *bus.Conn, channel string) bool {
	s, err := events.UtteranceOn(channel)
	if err != nil {
		return false
	}
	return published(c, s, events.UtteranceData{Channel: channel, Text: "hi"})
}

func published[T any](c *bus.Conn, s bus.Subject[T], data T) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, err := bus.Publish(ctx, c, s, data)
	return err == nil
}

// Plain doggie talks: flags without a command are the chat's.
func TestFlagsAloneAreTheChats(t *testing.T) {
	err := run([]string{"-bogus"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "flag provided but not defined: -bogus") {
		t.Errorf("err = %v, want the chat's flag error", err)
	}
	if err := run([]string{"-h"}, io.Discard); err != nil {
		t.Errorf("-h: %v", err)
	}
}

func TestBuildVersion(t *testing.T) {
	setting := func(kv ...string) []debug.BuildSetting {
		var s []debug.BuildSetting
		for i := 0; i < len(kv); i += 2 {
			s = append(s, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
		}
		return s
	}
	for _, tt := range []struct {
		info debug.BuildInfo
		want string
	}{
		{debug.BuildInfo{Main: debug.Module{Version: "v0.3.0"}}, "v0.3.0"},
		{debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: setting("vcs.revision", "fcf37c6a1b2c", "vcs.modified", "false")}, "fcf37c6"},
		{debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: setting("vcs.revision", "fcf37c6a1b2c", "vcs.modified", "true")}, "fcf37c6-dirty"},
		{debug.BuildInfo{Settings: setting("vcs.revision", "abc")}, "abc"},
		{debug.BuildInfo{}, "dev"},
	} {
		if got := buildVersion(&tt.info); got != tt.want {
			t.Errorf("buildVersion(%+v) = %q, want %q", tt.info, got, tt.want)
		}
	}
}

// A satellite with task kinds runs those kinds, and only those, as a worker
// named after it, through the real task manager.
func TestWorkerSatelliteRunsOnlyItsKinds(t *testing.T) {
	cfg := testConfig(t, "127.0.0.1:-1")
	for name, kinds := range map[string][]string{"lights": {"lights.set"}, "other": {"other.do"}} {
		if err := satelliteSub(cfg, io.Discard, "add", []string{name}, nil, kinds); err != nil {
			t.Fatal(err)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := startBus(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	dial := func(name string) *bus.Conn {
		token, err := bus.LoadToken(satelliteTokenFile(cfg, name))
		if err != nil {
			t.Fatal(err)
		}
		c, err := bus.Dial(srv.ClientURL(), bus.DialOptions{Name: name, User: name, Token: token, CAFile: caFile("", cfg), Log: log})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	lights, other := dial("lights"), dial("other")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	core, err := srv.Connect("tasks")
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()
	go manager.New(core, log, manager.Options{}).Run(ctx)
	wait, err := worker.Start(ctx, lights, log, map[string]worker.Func{
		"lights.set": func(ctx context.Context, task *worker.Task) (any, error) {
			if err := task.Progress(ctx, "dimming", 50); err != nil {
				return nil, err
			}
			return map[string]bool{"on": true}, nil
		},
	})
	if err != nil {
		t.Fatalf("the satellite can't serve its kind: %v", err)
	}
	defer func() { cancel(); wait() }()

	client, err := srv.Connect("client")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var ref events.TaskRef
	for deadline := time.Now().Add(5 * time.Second); ; {
		if ref, err = bus.Request(ctx, client, events.StartTask, events.TaskSpec{Kind: "lights.set"}); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond) // the manager may not serve yet
	}
	if err != nil {
		t.Fatal(err)
	}
	var info events.TaskInfo
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if info, _ = bus.Request(ctx, client, events.GetTask, ref); info.Status == events.StatusDone {
			break
		}
	}
	if info.Status != events.StatusDone || info.Worker != "lights" || string(info.Result) != `{"on":true}` || info.Progress != "dimming" {
		t.Fatalf("task = %+v, want done by lights", info)
	}

	// Another satellite can't take its kind, its controls or its reports.
	if _, err := bus.Serve(ctx, other, events.RunTask.With("lights.set")); err == nil {
		t.Error("a satellite served another's kind")
	}
	if _, err := bus.Serve(ctx, other, events.ControlTask.With("lights", ref.TaskID)); err == nil {
		t.Error("a satellite served another worker's controls")
	}
	rctx, rcancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer rcancel()
	if _, err := bus.Request(rctx, other, events.WorkerDone.With("lights"), events.TaskDoneData{TaskID: ref.TaskID}); err == nil {
		t.Error("a satellite reported as another worker")
	}
	if published(other, events.TaskDone, events.TaskDoneData{TaskID: ref.TaskID}) {
		t.Error("a worker satellite could publish task.done")
	}
}
