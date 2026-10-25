package demo

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/task/manager"
)

// setup runs a bus, the task manager and the demo worker (with a 10ms
// "second"), and returns a client connection.
func setup(t *testing.T) *bus.Conn {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := bus.Start(context.Background(), bus.Options{
		Token: "t", StoreDir: t.TempDir(), MaxAge: time.Hour, MaxBytes: 64 << 20, Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	connect := func(name string) *bus.Conn {
		c, err := srv.Connect(name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go manager.New(connect("tasks"), log, manager.DefaultOptions()).Run(ctx)
	d := New(connect("demo"), log)
	d.tick = 10 * time.Millisecond
	go d.Run(ctx)

	client := connect("client")
	deadline := time.Now().Add(5 * time.Second)
	for {
		// Ready when both the manager and the demo worker answer.
		rctx, rcancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, err := bus.Request(rctx, client, events.StartTask, events.TaskSpec{Kind: "demo.sleep", Input: json.RawMessage(`{"seconds":1}`)})
		rcancel()
		if err == nil {
			return client
		}
		if time.Now().After(deadline) {
			t.Fatalf("demo not ready: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func start(t *testing.T, c *bus.Conn, kind, input string) string {
	t.Helper()
	ref, err := bus.Request(context.Background(), c, events.StartTask, events.TaskSpec{Kind: kind, Input: json.RawMessage(input)})
	if err != nil {
		t.Fatal(err)
	}
	return ref.TaskID
}

func waitFor(t *testing.T, c *bus.Conn, id string, status events.TaskStatus) events.TaskInfo {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		info, err := bus.Request(context.Background(), c, events.GetTask, events.TaskRef{TaskID: id})
		if err != nil {
			t.Fatal(err)
		}
		if info.Status == status {
			return info
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is %s (%s), want %s", id, info.Status, info.Error, status)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSleepReportsProgressAndFinishes(t *testing.T) {
	c := setup(t)
	info := waitFor(t, c, start(t, c, "demo.sleep", `{"seconds":3}`), events.StatusDone)
	if string(info.Result) != `{"slept_s":3}` || info.Progress != "3/3 s" || info.Percent != 100 {
		t.Errorf("unexpected task: %+v", info)
	}
}

func TestSleepRejectsBadInput(t *testing.T) {
	c := setup(t)
	// 9e18 s overflows time.Duration: the bound must be checked before converting.
	for _, input := range []string{`{"seconds":0}`, `{"seconds":-3}`, `{"seconds":999999}`, `{"seconds":9000000000000000000}`, `{"seconds":"x"}`} {
		info := waitFor(t, c, start(t, c, "demo.sleep", input), events.StatusFailed)
		if info.Error == "" {
			t.Errorf("%s: failed without a reason", input)
		}
	}
}

func TestAskReturnsTheAnswer(t *testing.T) {
	c := setup(t)
	id := start(t, c, "demo.ask", `{"question":"¿Qué perfil?","options":["Andreé","Invitado"]}`)
	info := waitFor(t, c, id, events.StatusWaitingInput)
	if info.Question != "¿Qué perfil?" || strings.Join(info.Options, ",") != "Andreé,Invitado" {
		t.Errorf("unexpected question: %+v", info)
	}
	if _, err := bus.Request(context.Background(), c, events.AnswerTask, events.TaskAnswer{TaskID: id, Answer: "Andreé"}); err != nil {
		t.Fatal(err)
	}
	if info := waitFor(t, c, id, events.StatusDone); string(info.Result) != `{"answer":"Andreé"}` {
		t.Errorf("Result = %s", info.Result)
	}
}
