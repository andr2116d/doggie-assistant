package manager

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/task"
	"github.com/andr2116d/doggie-assistant/internal/task/worker"
)

// shardAt returns the shard numbered i.
func (m *Manager) shardAt(i int) *shard {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.shards[i]
}

// A shard busy for long doesn't hold up the tasks of the others.
func TestABusyShardDoesntHoldOthersUp(t *testing.T) {
	opts := testOptions()
	opts.Shards = 2
	h := newHarness(t, opts, map[string]worker.Func{"test.echo": echo})
	release := make(chan struct{})
	busy := make(chan struct{})
	h.mgr.shardAt(0).do(context.Background(), func() { close(busy); <-release })
	<-busy
	defer close(release)

	// t1 is shard 1's: it runs and ends while shard 0 is stuck.
	id := h.mustStart(events.TaskSpec{Kind: "test.echo", Input: json.RawMessage(`{"n":1}`)})
	if h.mgr.owner(id) != h.mgr.shardAt(1) {
		t.Fatalf("%s isn't shard 1's", id)
	}
	if info := h.waitStatus(id, events.StatusDone); string(info.Result) != `{"n":1}` {
		t.Errorf("result %s", info.Result)
	}
}

// A subtask belongs to its parent's shard, so cancelling the parent
// cancels it, also after a restart.
func TestSubtasksStayWithTheirParent(t *testing.T) {
	opts := testOptions()
	opts.Shards = 4
	b := newBlocker()
	h := newHarness(t, opts, map[string]worker.Func{"test.block": b.run})
	parent := h.mustStart(events.TaskSpec{Kind: "test.block"})
	child := h.mustStart(events.TaskSpec{Kind: "test.block", ParentID: parent})
	other := h.mustStart(events.TaskSpec{Kind: "test.block"})
	if h.mgr.owner(child) != h.mgr.owner(parent) || h.mgr.owner(other) == h.mgr.owner(parent) {
		t.Errorf("shards: parent %p, child %p, another root %p", h.mgr.owner(parent), h.mgr.owner(child), h.mgr.owner(other))
	}
	if _, err := bus.Request(context.Background(), h.client, events.CancelTask, events.TaskCancelRequest{TaskID: parent}); err != nil {
		t.Fatal(err)
	}
	h.waitStatus(child, events.StatusCancelled)

	h.stopManager()
	h.startManager()
	if h.mgr.owner(child) == nil || h.mgr.owner(child) != h.mgr.owner(parent) {
		t.Error("after a restart, the subtask left its parent's shard")
	}
	if info := h.get(other); info.Status != events.StatusFailed {
		t.Errorf("the other root is %s, want failed by the restart", info.Status)
	}
}

// A start that is refused leaves no route behind, and a subtask of an
// unknown parent is refused at once.
func TestRefusedStartsLeaveNoRoute(t *testing.T) {
	h := newHarness(t, testOptions(), nil)
	if _, err := h.start(events.TaskSpec{Kind: "bad kind"}); err == nil {
		t.Fatal("started a task of an invalid kind")
	}
	if h.mgr.owner("t1") != nil {
		t.Error("the refused task is still routed")
	}
	if _, err := h.start(events.TaskSpec{Kind: "test.x", ParentID: "t99"}); err == nil || !strings.Contains(err.Error(), `unknown task "t99"`) {
		t.Errorf("subtask of an unknown parent: err = %v", err)
	}
}

// Pruning a task forgets its route too.
func TestPruneForgetsRoutes(t *testing.T) {
	m := New(nil, discardLog(), Options{Retention: time.Minute, Shards: 1})
	m.routes = map[string]*shard{}
	s := newShard(m)
	old := time.Now().Add(-time.Hour)
	for id, status := range map[string]events.TaskStatus{"t1": events.StatusDone, "t2": events.StatusRunning} {
		s.tasks[id] = task.FromInfo(events.TaskInfo{ID: id, Status: status, UpdatedAt: old})
		m.setRoute(id, s)
	}
	s.prune()
	if m.owner("t1") != nil || s.tasks["t1"] != nil {
		t.Error("an old finished task survived the pruning")
	}
	if m.owner("t2") != s || s.tasks["t2"] == nil {
		t.Error("an active task was pruned")
	}
}

func TestShardsDefault(t *testing.T) {
	if o := (Options{}).withDefaults(); o.Shards < 1 || o.Shards > maxShards {
		t.Errorf("Shards = %d", o.Shards)
	}
	if o := (Options{Shards: 3}).withDefaults(); o.Shards != 3 {
		t.Errorf("Shards = %d, want 3", o.Shards)
	}
}

// A shard with no room drops beats rather than holding up the router.
func TestTryDoDoesntWait(t *testing.T) {
	m := New(nil, discardLog(), Options{Shards: 1})
	s := newShard(m)
	for range inboxSize {
		if !s.tryDo(func() {}) {
			t.Fatal("refused with room left")
		}
	}
	if s.tryDo(func() {}) {
		t.Error("took work with a full inbox")
	}
}
