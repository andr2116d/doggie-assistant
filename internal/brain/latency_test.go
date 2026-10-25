//go:build unix

package brain

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/brain/llm"
	"github.com/andr2116d/doggie-assistant/internal/brain/llm/claudecode"
	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/demo"
	"github.com/andr2116d/doggie-assistant/internal/mcp"
	"github.com/andr2116d/doggie-assistant/internal/task/manager"
	"github.com/andr2116d/doggie-assistant/prompts"
)

// TestRealLatency measures what the owner feels, on the real CLI: from a
// sentence to the first thing DOGGIE says, with and without a tool call,
// and from a task's end to its notice; cold and with a warm process. It
// spends the CLI's subscription, so it only runs with DOGGIE_REAL_CLAUDE=1
// (DOGGIE_LATENCY_RUNS sets the runs per case, default 3).
func TestRealLatency(t *testing.T) {
	if os.Getenv("DOGGIE_REAL_CLAUDE") != "1" {
		t.Skip("set DOGGIE_REAL_CLAUDE=1 to measure on the real claude CLI")
	}
	runs := 3
	fmt.Sscan(os.Getenv("DOGGIE_LATENCY_RUNS"), &runs)
	system, err := prompts.System(prompts.Persona{AssistantName: "DOGGIE", OwnerName: "Alex", Language: "es"})
	if err != nil {
		t.Fatal(err)
	}
	for _, warm := range []int{0, 1} {
		t.Run(fmt.Sprintf("warm=%d", warm), func(t *testing.T) {
			r := newRealStack(t, system, warm)
			var chat, action, notice, model, firstBlock samples
			for i := range runs {
				thread := fmt.Sprintf("lat%d", i)
				time.Sleep(2 * time.Second) // let a warm process come back
				// Plain conversation.
				s, turn := r.measure(thread, "Dime un dato curioso muy corto sobre los perros.")
				chat.add(s)
				model.add(time.Duration(turn.FirstTextMS) * time.Millisecond)
				firstBlock.add(s - time.Duration(turn.FirstTextMS)*time.Millisecond)
				time.Sleep(2 * time.Second)
				// An order and conversation: one tool call.
				s, _ = r.measure(thread, "Abre Netflix en la tele (demo.sleep de 2 segundos) y cuéntame un chiste corto.")
				action.add(s)
				// The notice when that task ends.
				notice.add(r.notice(thread))
			}
			t.Logf("warm=%d, %d runs (median / min / max):", warm, runs)
			t.Logf("  sentence → first words, chat:          %s", chat)
			t.Logf("  sentence → first words, order + tool:  %s", action)
			t.Logf("  task done → notice (whole block):      %s", notice)
			t.Logf("  model's first token (chat):            %s", model)
			t.Logf("  rest of chat (pipeline, warm-up):      %s", firstBlock)
			t.Logf("  first whole block, both cases:         %s", r.blocks)
		})
	}
}

type realStack struct {
	t      *testing.T
	client *bus.Conn
	speaks <-chan bus.Event[events.SpeakData]
	turns  <-chan bus.Event[events.BrainTurnData]
	dones  <-chan bus.Event[events.TaskDoneData]
	deltas <-chan events.SpeakDeltaData
	// blocks are the times to the first whole block (out.speak).
	blocks samples
}

func newRealStack(t *testing.T, system string, warm int) *realStack {
	srv, err := bus.Start(context.Background(), bus.Options{Token: "t", StoreDir: t.TempDir(), MaxAge: time.Hour, MaxBytes: 64 << 20, Log: discardLog()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	goRun(t, manager.New(connect(t, srv, "tasks"), discardLog(), manager.Options{}).Run)
	goRun(t, demo.New(connect(t, srv, "demo"), discardLog()).Run)
	dir := filepath.Join(t.TempDir(), "cc")
	b := New(connect(t, srv, "brain"), discardLog(), Options{HistoryTurns: 20, NewProvider: func(tools *mcp.Server) (llm.Brain, func(), error) {
		p, err := claudecode.New(claudecode.Options{Command: "claude", Model: "sonnet", System: system, Dir: dir, Tools: tools, Warm: warm, Log: discardLog()})
		if err != nil {
			return nil, nil, err
		}
		return p, p.Close, nil
	}})
	goRun(t, b.Run)
	r := &realStack{t: t, client: connect(t, srv, "client")}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.speaks, _ = bus.Subscribe(ctx, r.client, events.OutSpeak)
	r.turns, _ = bus.Subscribe(ctx, r.client, events.BrainTurn)
	r.dones, _ = bus.Subscribe(ctx, r.client, events.TaskDone)
	r.deltas, _ = bus.SubscribeSignal(ctx, r.client, events.OutSpeakDelta)
	time.Sleep(3 * time.Second) // components up, warm process connected
	return r
}

// measure says text and returns the time to the first streamed piece of the
// reply, and the turn's record (with the time to the first whole block).
func (r *realStack) measure(thread, text string) (time.Duration, events.BrainTurnData) {
	r.t.Helper()
	start := time.Now()
	var piece time.Duration
	meta, err := bus.Publish(context.Background(), r.client, events.UserUtterance.With("terminal"), events.UtteranceData{ThreadID: thread, Text: text})
	if err != nil {
		r.t.Fatal(err)
	}
	var first time.Duration
	deadline := time.After(2 * time.Minute)
	for {
		select {
		case d := <-r.deltas:
			if d.ThreadID == thread && piece == 0 {
				piece = time.Since(start)
			}
		case s := <-r.speaks:
			if s.Data.ThreadID == thread && first == 0 {
				first = s.Time.Sub(meta.Time) // both stamped on this machine
			}
		case turn := <-r.turns:
			if turn.Data.ThreadID != thread {
				continue
			}
			if turn.Data.Error != "" {
				r.t.Fatalf("turn failed: %s", turn.Data.Error)
			}
			r.blocks.add(first)
			return piece, turn.Data
		case <-deadline:
			r.t.Fatal("no turn")
		}
	}
}

// notice waits for the next task.done and the notice that follows in thread,
// and returns the time between them (both stamped on this machine).
func (r *realStack) notice(thread string) time.Duration {
	r.t.Helper()
	deadline := time.After(2 * time.Minute)
	var done, said time.Time
	for {
		select {
		case ev := <-r.dones:
			done = ev.Time
		case s := <-r.speaks:
			if s.Data.ThreadID == thread && !done.IsZero() && said.IsZero() {
				said = s.Time
			}
		case turn := <-r.turns:
			if turn.Data.ThreadID == thread && !said.IsZero() {
				return said.Sub(done)
			}
		case <-deadline:
			r.t.Fatal("no notice")
		}
	}
}

type samples []time.Duration

func (s *samples) add(d time.Duration) { *s = append(*s, d) }

func (s samples) String() string {
	if len(s) == 0 {
		return "n/a"
	}
	sorted := slices.Sorted(slices.Values(s))
	r := func(d time.Duration) string { return d.Round(10 * time.Millisecond).String() }
	return strings.Join([]string{r(sorted[len(sorted)/2]), r(sorted[0]), r(sorted[len(sorted)-1])}, " / ")
}
