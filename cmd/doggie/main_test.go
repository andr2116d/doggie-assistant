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
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/config"
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
	// A satellite may run tasks as a worker...
	if _, err := bus.Serve(ctx, sat, events.RunTask.With("reminder.set")); err != nil {
		t.Errorf("satellite cannot serve as a worker: %v", err)
	}
	// ...but not impersonate the task manager.
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
	if err := satelliteSub(cfg, &out, "add", []string{"voice-room"}, []string{"voice:living_room"}); err != nil {
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
