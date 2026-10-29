package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/brain/llm"
	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/mcp"
	"github.com/andr2116d/doggie-assistant/internal/task/manager"
	"github.com/andr2116d/doggie-assistant/internal/task/worker"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fake is a scripted LLM provider.
type fake struct {
	mu   sync.Mutex
	reqs []llm.Request
	// reply decides a turn's outputs; nil echoes the number of the turn.
	reply func(ctx context.Context, req llm.Request) []llm.Output
	// fail makes Generate itself fail.
	fail error
	// gate, if set, holds every turn until it receives (or is closed).
	gate chan struct{}

	running, maxRunning int
}

func (f *fake) Name() string { return "fake" }

func (f *fake) Generate(ctx context.Context, req llm.Request) (<-chan llm.Output, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	n := len(f.reqs)
	fail, gate, reply := f.fail, f.gate, f.reply
	f.mu.Unlock()
	if fail != nil {
		return nil, fail
	}
	out := make(chan llm.Output)
	go func() {
		defer close(out)
		f.mu.Lock()
		f.running++
		f.maxRunning = max(f.maxRunning, f.running)
		f.mu.Unlock()
		defer func() {
			f.mu.Lock()
			f.running--
			f.mu.Unlock()
		}()
		if gate != nil {
			select {
			case <-gate:
			case <-ctx.Done():
				out <- llm.Output{Done: &llm.Done{Err: ctx.Err()}}
				return
			}
		}
		outs := []llm.Output{{Text: fmt.Sprintf("respuesta %d", n)}}
		if reply != nil {
			outs = reply(ctx, req)
		}
		for _, o := range outs {
			out <- o
		}
		if len(outs) == 0 || outs[len(outs)-1].Done == nil {
			out <- llm.Output{Done: &llm.Done{Model: "fake-model", FirstText: 5 * time.Millisecond}}
		}
	}()
	return out, nil
}

func (f *fake) requests() []llm.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []llm.Request
	for _, r := range f.reqs {
		if r.Turn.ThreadID != probe {
			out = append(out, r)
		}
	}
	return out
}

func (f *fake) set(fn func(*fake)) {
	f.mu.Lock()
	fn(f)
	f.mu.Unlock()
}

// harness runs a bus, the task manager, a worker and the brain with a fake
// provider. client plays the channels and watches what the brain says.
type harness struct {
	t      *testing.T
	srv    *bus.Server
	client *bus.Conn
	fake   *fake
	brain  *Brain
	speaks <-chan bus.Event[events.SpeakData]
	turns  <-chan bus.Event[events.BrainTurnData]
	// release ends demo.sleep tasks: one per receive, all when closed.
	release chan struct{}
	stop    func() error
	// early are turns that ended while startBrain waited for its probe.
	early []bus.Event[events.BrainTurnData]
}

func connect(t *testing.T, srv *bus.Server, name string) *bus.Conn {
	t.Helper()
	c, err := srv.Connect(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func goRun(t *testing.T, f func(ctx context.Context) error) func() error {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f(ctx) }()
	var once sync.Once
	var err error
	stop := func() error {
		once.Do(func() {
			cancel()
			select {
			case err = <-done:
			case <-time.After(10 * time.Second):
				t.Error("component did not stop")
			}
		})
		return err
	}
	t.Cleanup(func() { stop() })
	return stop
}

func newHarness(t *testing.T, opts Options) *harness {
	t.Helper()
	srv, err := bus.Start(context.Background(), bus.Options{
		Token: "t", StoreDir: t.TempDir(), MaxAge: time.Hour, MaxBytes: 64 << 20, Log: discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	h := &harness{t: t, srv: srv, client: connect(t, srv, "client"), fake: &fake{}, release: make(chan struct{})}

	m := manager.New(connect(t, srv, "tasks"), discardLog(), manager.Options{})
	goRun(t, m.Run)
	wconn := connect(t, srv, "worker")
	goRun(t, func(ctx context.Context) error { return worker.Serve(ctx, wconn, discardLog(), h.funcs()) })
	h.waitManager()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if h.speaks, err = bus.Subscribe(ctx, h.client, events.OutSpeak); err != nil {
		t.Fatal(err)
	}
	if h.turns, err = bus.Subscribe(ctx, h.client, events.BrainTurn); err != nil {
		t.Fatal(err)
	}
	h.startBrain(opts)
	return h
}

// startBrain runs a brain on its own connection and waits until it listens.
func (h *harness) startBrain(opts Options) {
	h.t.Helper()
	if opts.NewProvider == nil {
		opts.NewProvider = func(*mcp.Server) (llm.Brain, func(), error) { return h.fake, func() {}, nil }
	}
	if opts.HistoryTurns == 0 {
		opts.HistoryTurns = 20
	}
	// Requests from here on belong to this brain.
	h.fake.set(func(f *fake) { f.reqs = nil })
	h.brain = New(connect(h.t, h.srv, fmt.Sprintf("brain-%d", time.Now().UnixNano())), discardLog(), opts)
	h.stop = goRun(h.t, h.brain.Run)
	// The brain is ready once it answers: probe with a throwaway thread.
	deadline := time.Now().Add(5 * time.Second)
	for {
		h.say(probe, "terminal", "ping")
		select {
		case ev := <-h.turns:
			if ev.Data.ThreadID != probe {
				h.early = append(h.early, ev) // a real turn, kept for the test
				continue
			}
			h.drainSpeaks()
			h.fake.set(func(f *fake) {
				var keep []llm.Request
				for _, r := range f.reqs {
					if r.Turn.ThreadID != probe {
						keep = append(keep, r)
					}
				}
				f.reqs = keep
			})
			return
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			h.t.Fatal("brain not ready")
		}
	}
}

func (h *harness) funcs() map[string]worker.Func {
	return map[string]worker.Func{
		events.KindDemoSleep.Name: func(ctx context.Context, t *worker.Task) (any, error) {
			select {
			case <-h.release:
				return map[string]bool{"ok": true}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		events.KindDemoAsk.Name: func(ctx context.Context, t *worker.Task) (any, error) {
			a, err := t.Ask(ctx, "¿Qué lista?", "Compras", "Trabajo")
			return map[string]string{"answer": a}, err
		},
	}
}

func (h *harness) waitManager() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err := bus.Request(ctx, h.client, events.ListTasks, events.TaskListRequest{})
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("task manager not ready: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// say publishes an utterance and returns its event ID.
func (h *harness) say(thread, channel, text string) string {
	h.t.Helper()
	subject, err := events.UtteranceOn(channel)
	if err != nil {
		h.t.Fatal(err)
	}
	meta, err := bus.Publish(context.Background(), h.client, subject, events.UtteranceData{ThreadID: thread, Channel: channel, Text: text})
	if err != nil {
		h.t.Fatal(err)
	}
	return meta.ID
}

// probe is the thread startBrain uses to see the brain is up. The helpers
// below ignore it, since late probes can still be answered during a test.
const probe = "probe"

func (h *harness) nextTurn() bus.Event[events.BrainTurnData] {
	h.t.Helper()
	if len(h.early) > 0 {
		ev := h.early[0]
		h.early = h.early[1:]
		return ev
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-h.turns:
			if ev.Data.ThreadID == probe {
				continue
			}
			return ev
		case <-deadline:
			h.t.Fatal("no brain.turn")
			return bus.Event[events.BrainTurnData]{}
		}
	}
}

func (h *harness) noTurn(d time.Duration) {
	h.t.Helper()
	if len(h.early) > 0 {
		h.t.Fatalf("unexpected turn: %+v", h.early[0].Data)
	}
	deadline := time.After(d)
	for {
		select {
		case ev := <-h.turns:
			if ev.Data.ThreadID != probe {
				h.t.Fatalf("unexpected turn: %+v", ev.Data)
			}
		case <-deadline:
			return
		}
	}
}

func (h *harness) nextSpeak() bus.Event[events.SpeakData] {
	h.t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev := <-h.speaks:
			if ev.Data.ThreadID == probe {
				continue
			}
			return ev
		case <-deadline:
			h.t.Fatal("no out.speak")
			return bus.Event[events.SpeakData]{}
		}
	}
}

func (h *harness) drainSpeaks() {
	for {
		select {
		case <-h.speaks:
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}

// startTask starts a task in thread like the start_task tool would, and
// reports it as a tool call.
func (h *harness) startTask(ctx context.Context, thread, kind, desc string) (string, llm.Output) {
	ref, err := bus.Request(ctx, h.client, events.StartTask, events.TaskSpec{Kind: kind, Description: desc, ThreadID: thread})
	if err != nil {
		h.t.Errorf("start %s: %v", kind, err)
	}
	in, _ := json.Marshal(map[string]string{"kind": kind, "description": desc})
	return ref.TaskID, llm.Output{Tool: &llm.ToolCall{Name: "start_task", Input: in}}
}

func (h *harness) waitStatus(id string, want events.TaskStatus) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := bus.Request(context.Background(), h.client, events.GetTask, events.TaskRef{TaskID: id})
		if err == nil && info.Status == want {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("task %s is %s, want %s", id, info.Status, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func mustContain(t *testing.T, s string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("missing %q in:\n%s", w, s)
		}
	}
}

func mustNotContain(t *testing.T, s string, denies ...string) {
	t.Helper()
	for _, d := range denies {
		if strings.Contains(s, d) {
			t.Errorf("unexpected %q in:\n%s", d, s)
		}
	}
}

var errBoom = errors.New("boom")

// inputOf is the <input> part of a prompt, without the history.
func inputOf(prompt string) string {
	if i := strings.Index(prompt, "<input>"); i >= 0 {
		return prompt[i:]
	}
	return prompt
}

// waitRequest waits until the provider got a turn whose input has text.
func (h *harness) waitRequest(text string) {
	h.t.Helper()
	waitFor(h.t, "a turn with "+text, func() bool {
		for _, r := range h.fake.requests() {
			if strings.Contains(inputOf(r.Input), text) {
				return true
			}
		}
		return false
	})
}
