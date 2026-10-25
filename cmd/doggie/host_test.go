//go:build unix

package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/config"
)

// fastTakeover makes chats take over at once.
func fastTakeover(t *testing.T) {
	delay, jit := takeoverDelay, takeoverJitter
	takeoverDelay, takeoverJitter = 200*time.Millisecond, 0
	t.Cleanup(func() { takeoverDelay, takeoverJitter = delay, jit })
}

// hostedChat is a chat that may run DOGGIE, as doggie chat on this machine.
type hostedChat struct {
	ui   *fakeUI
	host *hosting
	conn *bus.Conn
	done chan error
}

// startHostedChat runs a chat connected to the DOGGIE that client finds.
func startHostedChat(t *testing.T, client clientFlags) *hostedChat {
	t.Helper()
	ui := newFakeUI(true)
	states := make(chan connState, 64)
	conn, _, err := dialChat(client, ui, states)
	if err != nil {
		t.Fatal(err)
	}
	h := &hostedChat{ui: ui, host: newHosting(*client.config, noBrain(t)), conn: conn, done: make(chan error, 1)}
	o := testChatOptions()
	o.host = h.host
	ctx, cancel := context.WithCancel(context.Background())
	go func() { h.done <- chat(ctx, conn, ui, states, o) }()
	t.Cleanup(func() {
		cancel()
		<-h.done
		h.host.close()
		conn.Close()
	})
	return h
}

// works checks the chat's connection reaches a running DOGGIE.
func (h *hostedChat) works(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, err := bus.Request(ctx, h.conn, events.ListTasks, events.TaskListRequest{})
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("DOGGIE doesn't answer: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// hostDoggie runs DOGGIE as a chat would, outside any chat loop.
func hostDoggie(t *testing.T, client clientFlags) *hosting {
	t.Helper()
	h := newHosting(*client.config, noBrain(t))
	if err := h.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.close)
	return h
}

// crash stops a DOGGIE without a word, as kill -9 would.
func crash(h *hosting) {
	own := h.release()
	own.cancel()
	<-own.Done()
	own.logFile.Close()
}

func dataDir(t *testing.T, client clientFlags) string {
	t.Helper()
	cfg, err := config.Load(*client.config)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.DataDir
}

func TestChatTakesOverADoggieThatDied(t *testing.T) {
	fastTakeover(t)
	client := localDoggie(t)
	a := hostDoggie(t, client)
	b := startHostedChat(t, client)
	b.works(t)
	crash(a)
	b.ui.waitPrinted(t, "· lost the connection to DOGGIE; reconnecting...")
	b.ui.waitPrinted(t, "· DOGGIE was gone: started it here")
	b.ui.waitPrinted(t, "· connected again")
	b.works(t)
}

func TestChatTakesOverWhenItsHostCloses(t *testing.T) {
	fastTakeover(t)
	client := localDoggie(t)
	a := hostDoggie(t, client)
	b := startHostedChat(t, client)
	b.works(t)
	a.close()
	b.ui.waitPrinted(t, "· the chat that ran DOGGIE closed")
	b.ui.waitPrinted(t, "· DOGGIE was gone: started it here")
	b.works(t)
}

func TestChatLeavesADeliberateStopAlone(t *testing.T) {
	fastTakeover(t)
	client := localDoggie(t)
	cfg, err := config.Load(*client.config)
	if err != nil {
		t.Fatal(err)
	}
	run, err := startCore(context.Background(), cfg, slog.New(slog.DiscardHandler), noBrain(t), roleRun)
	if err != nil {
		t.Fatal(err)
	}
	b := startHostedChat(t, client)
	b.works(t)
	run.Stop(events.StopOnPurpose)
	b.ui.waitPrinted(t, "· DOGGIE was stopped; it's back when someone starts it")
	time.Sleep(5 * takeoverDelay)
	if strings.Contains(b.ui.printed(), "started it here") {
		t.Fatalf("took over a DOGGIE stopped on purpose: %q", b.ui.printed())
	}
	unlock, err := lockDataDir(cfg.DataDir, roleRun)
	if err != nil {
		t.Fatalf("someone runs DOGGIE: %v", err)
	}
	unlock()
	if strings.Contains(b.ui.printed(), "lost the connection") {
		t.Errorf("a deliberate stop was told as a lost connection: %q", b.ui.printed())
	}
}

// Two chats lose their DOGGIE: one runs it again, the other reconnects.
func TestOnlyOneChatTakesOver(t *testing.T) {
	fastTakeover(t)
	client := localDoggie(t)
	a := hostDoggie(t, client)
	b := startHostedChat(t, client)
	c := startHostedChat(t, client)
	b.works(t)
	c.works(t)
	crash(a)
	b.works(t)
	c.works(t)
	took := 0
	for _, h := range []*hostedChat{b, c} {
		h.ui.waitPrinted(t, "· connected again")
		if strings.Contains(h.ui.printed(), "started it here") {
			took++
		}
	}
	if took != 1 {
		t.Errorf("%d chats took DOGGIE over", took)
	}
}

// doggie run takes DOGGIE over from the chat that runs it; the chat stays,
// connected to the new one, and doesn't take it back.
func TestDoggieRunTakesOverFromAChat(t *testing.T) {
	fastTakeover(t)
	client := localDoggie(t)
	ui := newFakeUI(true)
	states := make(chan connState, 64)
	host := newHosting(*client.config, noBrain(t))
	conn, _, err := startHere(context.Background(), ui, client, states, host)
	if err != nil {
		t.Fatal(err)
	}
	b := &hostedChat{ui: ui, host: host, conn: conn, done: make(chan error, 1)}
	o := testChatOptions()
	o.host = host
	ctx, cancel := context.WithCancel(context.Background())
	go func() { b.done <- chat(ctx, conn, ui, states, o) }()
	defer func() {
		cancel()
		<-b.done
		host.close()
		conn.Close()
	}()
	b.works(t)

	cfg, err := config.Load(*client.config)
	if err != nil {
		t.Fatal(err)
	}
	run, err := startCore(context.Background(), cfg, slog.New(slog.DiscardHandler), noBrain(t), roleRun)
	if err != nil {
		t.Fatalf("doggie run: %v", err)
	}
	defer run.Stop(events.StopOnPurpose)
	ui.waitPrinted(t, "· doggie run is taking DOGGIE over; handing it over...")
	ui.waitPrinted(t, "· connected again")
	b.works(t)
	if pid, role := lockHolder(cfg.DataDir); role != roleRun || pid == 0 {
		t.Errorf("lock held by %d %q", pid, role)
	}
	time.Sleep(5 * takeoverDelay)
	if strings.Contains(ui.printed(), "DOGGIE was gone") {
		t.Errorf("the chat took DOGGIE back: %q", ui.printed())
	}
}

// Two explicit DOGGIEs never take over from each other.
func TestDoggieRunDoesNotTakeOverFromDoggieRun(t *testing.T) {
	client := localDoggie(t)
	cfg, err := config.Load(*client.config)
	if err != nil {
		t.Fatal(err)
	}
	first, err := startCore(context.Background(), cfg, slog.New(slog.DiscardHandler), noBrain(t), roleRun)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Stop(events.StopOnPurpose)
	cfg.Bus.Listen = "127.0.0.1:-1"
	_, err = startCore(context.Background(), cfg, slog.New(slog.DiscardHandler), noBrain(t), roleRun)
	var running *runningError
	if !errors.As(err, &running) || running.role != roleRun || running.pid == 0 {
		t.Errorf("second doggie run: %v", err)
	}
}
