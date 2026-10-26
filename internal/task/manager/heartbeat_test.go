package manager

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/task/worker"
)

func fastBeats() Options {
	o := testOptions()
	o.HeartbeatInterval = 50 * time.Millisecond
	return o
}

func TestSilentWorkerFailsTheTask(t *testing.T) {
	h := newHarness(t, fastBeats(), nil)
	f := newFakeWorker(h)
	cancelled := make(chan string, 1)
	f.serveRuns("test.dies", func(in bus.Incoming[events.TaskInfo, events.Ack]) {
		if in.Req.HeartbeatMS != 50 {
			t.Errorf("run request HeartbeatMS = %d, want 50", in.Req.HeartbeatMS)
		}
		f.serveControl(in.Req.ID, func(c bus.Incoming[events.TaskControl, events.Ack]) {
			if c.Req.Action == events.ControlCancel {
				cancelled <- in.Req.ID
			}
			c.Reply(events.Ack{})
		})
		f.accept(in) // accepts, then dies: never beats again
	})

	id := h.mustStart(events.TaskSpec{Kind: "test.dies"})
	info := h.waitStatus(id, events.StatusFailed)
	if !strings.Contains(info.Error, "no heartbeat") {
		t.Errorf("Error = %q, want a heartbeat failure", info.Error)
	}
	if got := recv(t, cancelled, "the silent worker to be told to stop"); got != id {
		t.Errorf("cancelled %q, want %q", got, id)
	}
}

func TestBeatingWorkerStaysAlive(t *testing.T) {
	b := newBlocker()
	h := newHarness(t, fastBeats(), map[string]worker.Func{"test.block": b.run})
	id := h.mustStart(events.TaskSpec{Kind: "test.block"})
	recv(t, b.started, "task start")

	time.Sleep(500 * time.Millisecond) // 10 intervals, well past the tolerance
	if info := h.get(id); info.Status != events.StatusRunning {
		t.Fatalf("a worker that keeps beating lost its task: %s (%q)", info.Status, info.Error)
	}
}

func TestWaitingTaskStaysAlive(t *testing.T) {
	h := newHarness(t, fastBeats(), map[string]worker.Func{
		"test.ask": func(ctx context.Context, t *worker.Task) (any, error) {
			return t.Ask(ctx, "still there?")
		},
	})
	id := h.mustStart(events.TaskSpec{Kind: "test.ask"})
	h.waitQuestion(id, "still there?")
	time.Sleep(500 * time.Millisecond) // nobody answers for a while
	if info := h.get(id); info.Status != events.StatusWaitingInput {
		t.Fatalf("a task waiting for its user was failed: %s (%q)", info.Status, info.Error)
	}
}

// Beats from anyone but the task's worker don't keep it alive.
func TestOthersBeatsDontKeepATaskAlive(t *testing.T) {
	h := newHarness(t, fastBeats(), nil)
	f := newFakeWorker(h)
	f.serveRuns("test.dies", func(in bus.Incoming[events.TaskInfo, events.Ack]) {
		f.accept(in) // and dies
	})
	impostor := connect(t, h.srv, "impostor")
	id := h.mustStart(events.TaskSpec{Kind: "test.dies"})
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
				bus.Emit(impostor, events.TaskAlive.With(impostor.Identity()), events.TaskAliveData{TaskID: id})
			}
		}
	}()
	if info := h.waitStatus(id, events.StatusFailed); !strings.Contains(info.Error, "no heartbeat") {
		t.Errorf("Error = %q, want a heartbeat failure", info.Error)
	}
}
