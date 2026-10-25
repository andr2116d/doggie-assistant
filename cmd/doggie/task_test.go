package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/demo"
	"github.com/andr2116d/doggie-assistant/internal/task/manager"
)

// taskEnv runs a bus with the task manager and the demo worker, and returns
// a client connection.
func taskEnv(t *testing.T) *bus.Conn {
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
	go demo.New(connect("demo"), log).Run(ctx)

	client := connect("cli")
	deadline := time.Now().Add(5 * time.Second)
	for {
		rctx, rcancel := context.WithTimeout(ctx, 500*time.Millisecond)
		_, err := bus.Request(rctx, client, events.ListTasks, events.TaskListRequest{})
		rcancel()
		if err == nil {
			return client
		}
		if time.Now().After(deadline) {
			t.Fatalf("manager not ready: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sub runs a task subcommand and returns its output.
func sub(t *testing.T, c *bus.Conn, name string, args []string, o taskOptions) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := taskSub(context.Background(), c, &out, name, args, o)
	return out.String(), err
}

func waitStatus(t *testing.T, c *bus.Conn, id string, want events.TaskStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err := sub(t, c, "get", []string{id}, taskOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var info events.TaskInfo
		if err := json.Unmarshal([]byte(out), &info); err != nil {
			t.Fatalf("get output is not JSON: %v\n%s", err, out)
		}
		if info.Status == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s is %s, want %s", id, info.Status, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTaskCLIFlow(t *testing.T) {
	c := taskEnv(t)
	waitForWorker(t, c)

	out, err := sub(t, c, "start", []string{"demo.ask", `{"question":"¿Qué perfil?","options":["Andreé","Invitado"]}`}, taskOptions{desc: "pick a profile", thread: "cli"})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(out)
	waitStatus(t, c, id, events.StatusWaitingInput)

	out, err = sub(t, c, "list", nil, taskOptions{active: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{id, "demo.ask", "waiting_input", `"¿Qué perfil?" [Andreé | Invitado]`} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}

	if out, err = sub(t, c, "answer", []string{id, "Andreé"}, taskOptions{}); err != nil || !strings.Contains(out, "answered "+id) {
		t.Fatalf("answer: %q, %v", out, err)
	}
	waitStatus(t, c, id, events.StatusDone)
	out, _ = sub(t, c, "list", nil, taskOptions{})
	if !strings.Contains(out, `{"answer":"Andreé"}`) {
		t.Errorf("list does not show the result:\n%s", out)
	}
	if out, _ = sub(t, c, "list", nil, taskOptions{active: true}); strings.Contains(out, id+" ") {
		t.Errorf("finished task %s still in the active list:\n%s", id, out)
	}
}

func TestTaskCLICancel(t *testing.T) {
	c := taskEnv(t)
	waitForWorker(t, c)
	out, err := sub(t, c, "start", []string{"demo.sleep", `{"seconds":60}`}, taskOptions{timeout: 1500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSpace(out)
	if out, err := sub(t, c, "get", []string{id}, taskOptions{}); err != nil || !strings.Contains(out, `"timeout_s": 2`) {
		t.Errorf("timeout not rounded up to 2s: %v\n%s", err, out)
	}
	if out, err := sub(t, c, "cancel", []string{id}, taskOptions{reason: "changed my mind"}); err != nil || !strings.Contains(out, "cancelled "+id) {
		t.Fatalf("cancel: %q, %v", out, err)
	}
	waitStatus(t, c, id, events.StatusCancelled)
	// The person asked for it, so the brain doesn't tell them.
	ev, ok, err := bus.LastEvent(context.Background(), c, events.TaskCancelled)
	if err != nil || !ok || ev.Data.TaskID != id || !ev.Data.ByUser {
		t.Errorf("task.cancelled = %+v, %v, %v; want %s by the user", ev.Data, ok, err, id)
	}
}

func TestTaskCLIErrors(t *testing.T) {
	c := taskEnv(t)
	for _, tt := range []struct {
		name string
		args []string
		want string
	}{
		{"start", nil, "usage"},
		{"start", []string{"demo.sleep", "{not json"}, "valid JSON"},
		{"start", []string{"nobody.home"}, "no worker"},
		{"get", nil, "usage"},
		{"get", []string{"t99"}, "unknown task"},
		{"answer", []string{"t1"}, "usage"},
		{"cancel", nil, "usage"},
		{"explode", nil, "unknown task subcommand"},
	} {
		_, err := sub(t, c, tt.name, tt.args, taskOptions{})
		if err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s %v: err = %v, want %q", tt.name, tt.args, err, tt.want)
		}
	}
}

func TestDetailAndAge(t *testing.T) {
	if got := detail(events.TaskInfo{Status: events.StatusRunning, Progress: "2/5 s"}); got != "2/5 s" {
		t.Errorf("running detail = %q", got)
	}
	if got := detail(events.TaskInfo{Status: events.StatusFailed, Error: "service unreachable"}); got != "service unreachable" {
		t.Errorf("failed detail = %q", got)
	}
	for d, want := range map[time.Duration]string{5 * time.Second: "5s", 3 * time.Minute: "3m", 2 * time.Hour: "2h"} {
		if got := age(d); got != want {
			t.Errorf("age(%s) = %q, want %q", d, got, want)
		}
	}
	if got := truncate("ñandú y más texto", 6); got != "ñandú…" {
		t.Errorf("truncate = %q", got)
	}
}

// waitForWorker waits until the demo worker serves its kinds.
func waitForWorker(t *testing.T, c *bus.Conn) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := sub(t, c, "start", []string{"demo.sleep", `{"seconds":1}`}, taskOptions{})
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("demo worker not ready: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestParseTaskFlagOrder(t *testing.T) {
	args, o, _, err := parseTask("start", []string{"-desc", "esperar un rato", "-timeout", "30s", "demo.sleep", `{"seconds":2}`})
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 2 || args[0] != "demo.sleep" || o.desc != "esperar un rato" || o.timeout != 30*time.Second {
		t.Errorf("args=%v opts=%+v", args, o)
	}

	for _, bad := range [][]string{
		{"demo.sleep", "-desc", "x"},
		{"demo.sleep", "--timeout=5s"},
	} {
		if _, _, _, err := parseTask("start", bad); err == nil || !strings.Contains(err.Error(), "must come before") {
			t.Errorf("parseTask(%v): err = %v, want a flag-order hint", bad, err)
		}
	}
	// A negative JSON number is an argument, not a flag.
	if args, _, _, err := parseTask("start", []string{"demo.x", "-5"}); err != nil || args[1] != "-5" {
		t.Errorf("negative input: args=%v err=%v", args, err)
	}
	if _, _, _, err := parseTask("explode", nil); err == nil {
		t.Error("unknown subcommand accepted")
	}
}
