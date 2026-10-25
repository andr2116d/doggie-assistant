package main

import (
	"bytes"
	"context"
	"fmt"
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
		utterances, err := bus.SubscribeEnvelopes(ctx, brainConn, events.UserUtterance.Type()+".*")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			for env := range utterances {
				u, _, err := bus.DecodeUnder(env, events.UserUtterance)
				if err == nil {
					answer(ctx, brainConn, u)
				}
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

// Saying something on a channel the connection may not speak for fails at
// once with an explanation, instead of hanging.
func TestSayOnAForbiddenChannelFailsFast(t *testing.T) {
	cfg := testConfig(t, "127.0.0.1:-1")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := startBus(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	token, _ := bus.LoadToken(tokenPath(cfg))
	sat, err := bus.Dial(srv.ClientURL(), bus.DialOptions{Name: "say", Token: token, CAFile: caFile("", cfg), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer sat.Close()
	start := time.Now()
	err = say(context.Background(), sat, io.Discard, "abre la puerta", sayOptions{channel: "voice:living_room", thread: "x", wait: time.Second, replyTimeout: time.Minute})
	if err == nil || !strings.Contains(err.Error(), "allowed to speak for that channel") {
		t.Errorf("err = %v", err)
	}
	if took := time.Since(start); took > 8*time.Second {
		t.Errorf("took %s", took)
	}
	if err := say(context.Background(), sat, io.Discard, "hola", sayOptions{channel: "bad channel", thread: "x"}); err == nil {
		t.Error("an invalid channel was accepted")
	}
}

func TestReplyViewStreams(t *testing.T) {
	type step struct {
		delta bool
		cause string
		block int
		text  string
	}
	d := func(c string, b int, s string) step { return step{true, c, b, s} }
	e := func(c string, b int, s string) step { return step{false, c, b, s} }
	for name, tt := range map[string]struct {
		steps []step
		want  string
	}{
		"pieces then the block": {[]step{d("u", 0, "Ya "), d("u", 0, "voy."), e("u", 0, "Ya voy.")}, "doggie> Ya voy.\n"},
		"no pieces":             {[]step{e("u", 0, "Hola")}, "doggie> Hola\n"},
		"two blocks":            {[]step{d("u", 0, "Uno"), e("u", 0, "Uno"), d("u", 1, "Dos"), e("u", 1, "Dos")}, "doggie> Uno\ndoggie> Dos\n"},
		"a lost last piece":     {[]step{d("u", 0, "Ya "), e("u", 0, "Ya voy.")}, "doggie> Ya voy.\n"},
		"a lost piece inside":   {[]step{d("u", 0, "Ya "), d("u", 0, "."), e("u", 0, "Ya voy.")}, "doggie> Ya .\ndoggie> Ya voy.\n"},
		// The block overtakes its last pieces, which come late.
		"block before its end": {[]step{d("u", 0, "Un "), d("u", 0, "chiste "), e("u", 0, "Un chiste corto."), d("u", 0, "corto.")}, "doggie> Un chiste corto.\n"},
		// The next block's pieces overtake the end of the previous one.
		"overtaken": {[]step{d("u", 0, "Uno, "), d("u", 1, "Dos"), d("u", 0, "más"), e("u", 0, "Uno, más"), e("u", 1, "Dos")},
			"doggie> Uno, \ndoggie> Dos\ndoggie> Uno, más\n"},
		"leading whitespace": {[]step{d("u", 0, "  "), d("u", 0, "\nHola"), e("u", 0, "\nHola")}, "doggie> Hola\n"},
		"blank piece only":   {[]step{d("u", 0, "   "), e("u", 1, "Hola")}, "doggie> Hola\n"},
		"another turn":       {[]step{d("u1", 0, "A"), d("u2", 0, "B"), e("u1", 0, "A"), e("u2", 0, "B")}, "doggie> A\ndoggie> B\n"},
		// Pieces travel apart from the log: one may come after its block.
		"late piece": {[]step{d("u", 0, "Un "), e("u", 0, "Un chiste."), d("u", 0, "chiste."), e("u", 1, "Otro.")},
			"doggie> Un chiste.\ndoggie> Otro.\n"},
		"late piece of a block shown whole": {[]step{e("u", 0, "Hola."), d("u", 0, "Hola.")}, "doggie> Hola.\n"},
	} {
		var out strings.Builder
		v := newReplyView(&out)
		for _, s := range tt.steps {
			if s.delta {
				v.delta(blockKey{s.cause, s.block}, s.text)
			} else {
				v.block(blockKey{s.cause, s.block}, s.text)
			}
		}
		if out.String() != tt.want {
			t.Errorf("%s: printed %q, want %q", name, out.String(), tt.want)
		}
	}
}

// Blocks whose end never comes (the brain stopped mid-block) don't pile up.
func TestReplyViewIsBounded(t *testing.T) {
	v := newReplyView(io.Discard)
	for i := range 10 * maxOpenBlocks {
		v.delta(blockKey{fmt.Sprint(i), 0}, "x")
		v.delta(blockKey{fmt.Sprint(i), 1}, " ")
		v.block(blockKey{fmt.Sprint(i), 2}, "y")
	}
	if n := len(v.printed) + len(v.pending); n > 2*maxOpenBlocks {
		t.Errorf("%d blocks kept", n)
	}
	if len(v.done.set) > maxRecent {
		t.Errorf("%d done blocks kept", len(v.done.set))
	}
}

func TestRecentIDsForgetsTheOldest(t *testing.T) {
	var r recentIDs
	for i := range maxRecent + 10 {
		if !r.add(fmt.Sprint(i)) {
			t.Fatalf("%d not new", i)
		}
	}
	if r.has("9") || !r.has("10") || r.add("20") || len(r.set) != maxRecent || len(r.order) != maxRecent {
		t.Errorf("kept %d ids, %d in order", len(r.set), len(r.order))
	}
	if !r.add("5") {
		t.Error("a forgotten id is not new again")
	}
}
