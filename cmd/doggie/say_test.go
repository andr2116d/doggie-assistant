package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

// sayEnv runs a bus and a stand-in brain: for every utterance it calls
// answer, which publishes what the brain would. It returns a client
// connection.
func sayEnv(t *testing.T, answer func(ctx context.Context, c *bus.Conn, u bus.Event[events.UtteranceData])) *bus.Conn {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := bus.Start(context.Background(), bus.Options{Token: "t", StoreDir: t.TempDir(), MaxAge: time.Hour, MaxBytes: 64 << 20, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	brainConn, _ := srv.Connect("brain")
	client, _ := srv.Connect("client")
	t.Cleanup(func() { brainConn.Close(); client.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if answer != nil {
		utterances, err := bus.Subscribe(ctx, brainConn, events.UserUtterance)
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for u := range utterances {
				answer(ctx, brainConn, u)
			}
		}()
	}
	return client
}

func reply(ctx context.Context, c *bus.Conn, thread string, cause string, texts []string, errText string) {
	for _, s := range texts {
		bus.Publish(ctx, c, events.OutSpeak, events.SpeakData{ThreadID: thread, Channel: "terminal", Text: s})
	}
	var inputs []events.TurnInput
	if cause != "" {
		inputs = []events.TurnInput{{EventID: cause}}
	}
	bus.Publish(ctx, c, events.BrainTurn, events.BrainTurnData{ThreadID: thread, Inputs: inputs, Replies: texts, Error: errText})
}

func TestSayPrintsRepliesAndLaterNotices(t *testing.T) {
	client := sayEnv(t, func(ctx context.Context, c *bus.Conn, u bus.Event[events.UtteranceData]) {
		if u.Data.Text != "ábreme netflix" || u.Data.ThreadID != "terminal" || u.Data.Channel != "terminal" {
			t.Errorf("utterance = %+v", u.Data)
		}
		reply(ctx, c, "other", "", []string{"not for you"}, "") // another thread
		reply(ctx, c, "terminal", u.ID, []string{"Ya estoy en eso.", "Un chiste…"}, "")
		time.Sleep(100 * time.Millisecond)
		reply(ctx, c, "terminal", "", []string{"Listo, Netflix abierto."}, "") // a notice
	})
	var out bytes.Buffer
	start := time.Now()
	err := say(context.Background(), client, &out, "ábreme netflix", sayOptions{channel: "terminal", thread: "terminal", wait: 500 * time.Millisecond, replyTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	want := "doggie> Ya estoy en eso.\ndoggie> Un chiste…\ndoggie> Listo, Netflix abierto.\n"
	if out.String() != want {
		t.Errorf("printed %q, want %q", out.String(), want)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("say kept listening for %s", took)
	}
}

func TestSayReportsAFailedTurn(t *testing.T) {
	client := sayEnv(t, func(ctx context.Context, c *bus.Conn, u bus.Event[events.UtteranceData]) {
		reply(ctx, c, u.Data.ThreadID, u.ID, nil, "claudecode: Please run /login")
	})
	var out bytes.Buffer
	if err := say(context.Background(), client, &out, "hola", sayOptions{channel: "terminal", thread: "terminal", wait: 100 * time.Millisecond, replyTimeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "the turn failed: claudecode: Please run /login") {
		t.Errorf("printed %q", out.String())
	}
}

func TestSayWithoutABrain(t *testing.T) {
	client := sayEnv(t, nil)
	err := say(context.Background(), client, io.Discard, "hola", sayOptions{channel: "terminal", thread: "terminal", wait: time.Second, replyTimeout: 300 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "no reply") {
		t.Errorf("err = %v", err)
	}
}

func TestSayStopsOnInterrupt(t *testing.T) {
	client := sayEnv(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	if err := say(ctx, client, io.Discard, "hola", sayOptions{channel: "terminal", thread: "terminal", wait: time.Minute, replyTimeout: time.Minute}); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestSayNeedsText(t *testing.T) {
	for _, args := range [][]string{nil, {""}, {"  ", "\t"}} {
		if err := sayCmd(args, io.Discard); err == nil || !strings.Contains(err.Error(), "nothing to say") {
			t.Errorf("sayCmd(%q) = %v", args, err)
		}
	}
}
