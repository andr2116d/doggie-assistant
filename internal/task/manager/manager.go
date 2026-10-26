// Package manager is the task manager component: the source of truth for the
// state of every task.
//
// It serves the task commands (start, cancel, answer, list, get), dispatches
// each task to a worker of its kind, takes the reports of the worker that
// accepted it, enforces timeouts and cancels subtasks with their parent. It
// is the only one that publishes task events, so they are logged in the
// order it applied them. On start it rebuilds its state from the event log
// and fails the tasks a restart interrupted.
//
// The tasks are shared among shards, actors that run apart, one per CPU by
// default: a task and its subtasks always belong to the same one. The
// manager gives out task IDs and hands every request to the shard that owns
// its task. Slow work (round trips to workers) runs in goroutines that send
// their outcome back to the shard as a closure.
package manager

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/task"
)

// controlTimeout bounds round trips to a worker (cancel, answer).
const controlTimeout = 5 * time.Second

// List limits: the reply must fit in one NATS message (1 MB by default).
const (
	defaultListLimit = 100
	maxListLimit     = 500
)

// maxShards bounds the shards the default makes on a machine with many CPUs.
const maxShards = 16

// Options configures the manager.
type Options struct {
	// DefaultTimeout applies to tasks that don't set their own.
	DefaultTimeout time.Duration
	// MaxTimeout caps the timeout a task may ask for. It must not exceed
	// Retention, so a running task is always within the recovery window.
	MaxTimeout time.Duration
	// DispatchTimeout bounds how long a worker may take to accept a task.
	DispatchTimeout time.Duration
	// Retention is how long finished tasks are kept, and how far back the
	// event log is read on start.
	Retention time.Duration
	// HeartbeatInterval is how often workers report each running task. A
	// task silent for heartbeatTolerance intervals fails: its worker died.
	HeartbeatInterval time.Duration
	// Shards is how many actors share the tasks. Zero means one per CPU, up
	// to 16.
	Shards int
}

// heartbeatTolerance is how many heartbeat intervals a task may miss before
// its worker is considered dead.
const heartbeatTolerance = 3

// DefaultOptions returns production settings.
func DefaultOptions() Options {
	return Options{
		DefaultTimeout:    10 * time.Minute,
		MaxTimeout:        24 * time.Hour,
		DispatchTimeout:   10 * time.Second,
		Retention:         24 * time.Hour,
		HeartbeatInterval: 5 * time.Second,
		Shards:            min(runtime.GOMAXPROCS(0), maxShards),
	}
}

// Manager is the task manager component.
type Manager struct {
	conn *bus.Conn
	log  *slog.Logger
	opts Options

	// State of the current run.
	shards []*shard
	next   atomic.Int64 // the number of the last task ID given out
	mu     sync.RWMutex
	routes map[string]*shard // the shard that owns each task
}

// maxResult bounds a task's result: the task.done that carries it must fit
// in one message, with room for the envelope.
const maxResult = 900 << 10

// withDefaults fills zero fields with the defaults and keeps MaxTimeout
// within Retention.
func (o Options) withDefaults() Options {
	d := DefaultOptions()
	if o.DefaultTimeout <= 0 {
		o.DefaultTimeout = d.DefaultTimeout
	}
	if o.MaxTimeout <= 0 {
		o.MaxTimeout = d.MaxTimeout
	}
	if o.DispatchTimeout <= 0 {
		o.DispatchTimeout = d.DispatchTimeout
	}
	if o.Retention <= 0 {
		o.Retention = d.Retention
	}
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = d.HeartbeatInterval
	}
	if o.Shards <= 0 {
		o.Shards = d.Shards
	}
	o.MaxTimeout = min(o.MaxTimeout, o.Retention)
	o.DefaultTimeout = min(o.DefaultTimeout, o.MaxTimeout)
	return o
}

// New returns a task manager publishing and serving on conn. Zero fields in
// opts take their default value.
func New(conn *bus.Conn, log *slog.Logger, opts Options) *Manager {
	return &Manager{conn: conn, log: log, opts: opts.withDefaults()}
}

// Name implements supervisor.Component.
func (m *Manager) Name() string { return "tasks" }

// Run implements supervisor.Component. Each run starts from a clean state,
// rebuilt from the event log, so a run restarted by the supervisor doesn't
// mix in stale state.
func (m *Manager) Run(ctx context.Context) error {
	m.mu.Lock()
	m.routes = make(map[string]*shard)
	m.shards = make([]*shard, m.opts.Shards)
	for i := range m.shards {
		m.shards[i] = newShard(m)
	}
	m.mu.Unlock()
	m.next.Store(0)
	// Only the manager publishes task events, so the log holds them all.
	logged, err := m.recover(ctx)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	defer wg.Wait()
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	type counts struct{ tasks, interrupted int }
	ready := make(chan counts, len(m.shards))
	for _, s := range m.shards {
		wg.Go(func() {
			s.run(runCtx, logged[s], func(tasks, interrupted int) { ready <- counts{tasks, interrupted} })
		})
	}
	var total counts
	for range m.shards {
		select {
		case c := <-ready:
			total.tasks += c.tasks
			total.interrupted += c.interrupted
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	m.log.Info("tasks recovered", "tasks", total.tasks, "interrupted", total.interrupted, "next_id", m.next.Load()+1, "shards", len(m.shards))

	starts, err := bus.Serve(runCtx, m.conn, events.StartTask)
	if err != nil {
		return err
	}
	cancels, err := bus.Serve(runCtx, m.conn, events.CancelTask)
	if err != nil {
		return err
	}
	directCancels, err := bus.ServeUnder(runCtx, m.conn, events.CancelTaskFrom)
	if err != nil {
		return err
	}
	answers, err := bus.Serve(runCtx, m.conn, events.AnswerTask)
	if err != nil {
		return err
	}
	lists, err := bus.Serve(runCtx, m.conn, events.ListTasks)
	if err != nil {
		return err
	}
	gets, err := bus.Serve(runCtx, m.conn, events.GetTask)
	if err != nil {
		return err
	}
	// Every worker reports under its own name, which the bus enforces.
	accepts, err := bus.ServeUnder(runCtx, m.conn, events.WorkerAccept)
	if err != nil {
		return err
	}
	progresses, err := bus.ServeUnder(runCtx, m.conn, events.WorkerProgress)
	if err != nil {
		return err
	}
	asks, err := bus.ServeUnder(runCtx, m.conn, events.WorkerAsk)
	if err != nil {
		return err
	}
	dones, err := bus.ServeUnder(runCtx, m.conn, events.WorkerDone)
	if err != nil {
		return err
	}
	failures, err := bus.ServeUnder(runCtx, m.conn, events.WorkerFailed)
	if err != nil {
		return err
	}
	beats, err := bus.SubscribeSignalUnder(runCtx, m.conn, events.TaskAlive)
	if err != nil {
		return err
	}

	// Requests of each kind are handed out in the order they came, so those
	// about one task reach its shard in order.
	wg.Go(func() {
		for in := range starts {
			m.start(runCtx, in)
		}
	})
	wg.Go(func() {
		for in := range lists {
			m.list(runCtx, in)
		}
	})
	wg.Go(func() {
		for b := range beats {
			// A beat is soon followed by another: one a busy shard has no
			// room for is dropped, rather than holding up the others'.
			if s := m.owner(b.Data.TaskID); s != nil {
				s.tryDo(func() { s.beat(b.Data.TaskID, b.Under) })
			}
		}
	})
	wg.Go(func() {
		routed(runCtx, m, cancels, taskOf, func(s *shard, in bus.Incoming[events.TaskCancelRequest, events.Ack]) {
			s.cancel(runCtx, in, "")
		})
	})
	wg.Go(func() {
		routed(runCtx, m, directCancels, taskOf, func(s *shard, in bus.Incoming[events.TaskCancelRequest, events.Ack]) {
			if !events.ValidChannel(in.Under()) {
				in.Fail(fmt.Errorf("invalid channel %q", in.Under()))
				return
			}
			s.cancel(runCtx, in, in.Under())
		})
	})
	wg.Go(func() {
		routed(runCtx, m, answers, func(r events.TaskAnswer) string { return r.TaskID }, func(s *shard, in bus.Incoming[events.TaskAnswer, events.Ack]) {
			s.answer(runCtx, in)
		})
	})
	wg.Go(func() {
		routed(runCtx, m, gets, func(r events.TaskRef) string { return r.TaskID }, (*shard).get)
	})
	wg.Go(func() {
		routed(runCtx, m, accepts, func(r events.TaskClaim) string { return r.TaskID }, func(s *shard, in bus.Incoming[events.TaskClaim, events.Ack]) {
			s.accept(runCtx, in)
		})
	})
	wg.Go(func() {
		routed(runCtx, m, progresses, func(r events.TaskProgressData) string { return r.TaskID }, func(s *shard, in bus.Incoming[events.TaskProgressData, events.Ack]) {
			s.progress(runCtx, in)
		})
	})
	wg.Go(func() {
		routed(runCtx, m, asks, func(r events.TaskNeedsInputData) string { return r.TaskID }, func(s *shard, in bus.Incoming[events.TaskNeedsInputData, events.Ack]) {
			s.ask(runCtx, in)
		})
	})
	wg.Go(func() {
		routed(runCtx, m, dones, func(r events.TaskDoneData) string { return r.TaskID }, func(s *shard, in bus.Incoming[events.TaskDoneData, events.Ack]) {
			s.done(runCtx, in)
		})
	})
	wg.Go(func() {
		routed(runCtx, m, failures, func(r events.TaskFailedData) string { return r.TaskID }, func(s *shard, in bus.Incoming[events.TaskFailedData, events.Ack]) {
			s.failed(runCtx, in)
		})
	})
	<-ctx.Done()
	return ctx.Err()
}

func taskOf(r events.TaskCancelRequest) string { return r.TaskID }

// routed hands each request on ch to the shard that owns its task, as told
// by id. A request about an unknown task fails at once.
func routed[Req, Resp any](ctx context.Context, m *Manager, ch <-chan bus.Incoming[Req, Resp], id func(Req) string, handle func(*shard, bus.Incoming[Req, Resp])) {
	for in := range ch {
		s := m.owner(id(in.Req))
		if s == nil {
			in.Fail(fmt.Errorf("unknown task %q", id(in.Req)))
			continue
		}
		s.do(ctx, func() { handle(s, in) })
	}
}

// recover reads the tasks of the retention window from the event log, and
// returns each shard's events. It also routes their tasks and sets where
// IDs go on.
func (m *Manager) recover(ctx context.Context) (map[*shard][]bus.Envelope, error) {
	history, err := bus.History(ctx, m.conn, time.Now().Add(-m.opts.Retention), "task.>")
	if err != nil {
		return nil, fmt.Errorf("tasks: recover: %w", err)
	}
	logged := make(map[*shard][]bus.Envelope)
	for _, env := range history {
		if s := m.ownerOfLogged(env); s != nil {
			logged[s] = append(logged[s], env)
		}
	}
	// IDs must never be reused, even when the last task is older than the
	// recovery window: continue from the newest task in the whole log.
	last, ok, err := bus.LastEvent(ctx, m.conn, events.TaskCreated)
	if err != nil {
		return nil, fmt.Errorf("tasks: recover: %w", err)
	}
	if n, valid := idNumber(last.Data.ID); ok && valid && int64(n) > m.next.Load() {
		m.next.Store(int64(n))
	}
	return logged, nil
}

// ownerOfLogged returns the shard of a logged event's task, routing the
// task when the event creates it. It returns nil for an event about no
// known task.
func (m *Manager) ownerOfLogged(env bus.Envelope) *shard {
	if env.Type != events.TaskCreated.Type() {
		id, err := task.EventTaskID(env)
		if err != nil {
			return nil
		}
		return m.owner(id)
	}
	ev, err := bus.Decode(env, events.TaskCreated)
	if err != nil {
		m.log.Warn("ignoring task event", "type", env.Type, "err", err)
		return nil
	}
	id := ev.Data.ID
	if s := m.owner(id); s != nil {
		return s
	}
	n, _ := idNumber(id)
	if int64(n) > m.next.Load() {
		m.next.Store(int64(n))
	}
	s := m.owner(ev.Data.ParentID)
	if s == nil {
		s = m.shardFor(n)
	}
	m.setRoute(id, s)
	return s
}

// start gives a new task its ID and hands it to its shard: its parent's, or
// one chosen by its number.
func (m *Manager) start(ctx context.Context, in bus.Incoming[events.TaskSpec, events.TaskRef]) {
	var s *shard
	if parent := in.Req.ParentID; parent != "" {
		if s = m.owner(parent); s == nil {
			in.Fail(fmt.Errorf("unknown task %q", parent))
			return
		}
	}
	n := int(m.next.Add(1))
	if s == nil {
		s = m.shardFor(n)
	}
	id := "t" + strconv.Itoa(n)
	m.setRoute(id, s)
	s.do(ctx, func() { s.start(ctx, in, id) })
}

// list answers task.list with the tasks of every shard.
func (m *Manager) list(ctx context.Context, in bus.Incoming[events.TaskListRequest, events.TaskList]) {
	var all []events.TaskInfo
	for _, s := range m.shards {
		got := make(chan []events.TaskInfo, 1)
		s.do(ctx, func() { got <- s.infos(in.Req.ActiveOnly) })
		select {
		case infos := <-got:
			all = append(all, infos...)
		case <-ctx.Done():
			return
		}
	}
	in.Reply(listOf(all, in.Req.Limit))
}

// listOf returns the most recent of tasks, at most limit (or the default),
// oldest first.
func listOf(tasks []events.TaskInfo, limit int) events.TaskList {
	if limit <= 0 {
		limit = defaultListLimit
	}
	limit = min(limit, maxListLimit)
	slices.SortFunc(tasks, func(a, b events.TaskInfo) int {
		na, _ := idNumber(a.ID)
		nb, _ := idNumber(b.ID)
		return na - nb
	})
	out := events.TaskList{Tasks: tasks, Total: len(tasks)}
	if len(tasks) > limit {
		out.Tasks = tasks[len(tasks)-limit:]
	}
	if out.Tasks == nil {
		out.Tasks = []events.TaskInfo{}
	}
	return out
}

// shardFor is the shard of a root task numbered n.
func (m *Manager) shardFor(n int) *shard {
	return m.shards[max(n, 0)%len(m.shards)]
}

// owner returns the shard that owns task id, or nil.
func (m *Manager) owner(id string) *shard {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.routes[id]
}

func (m *Manager) setRoute(id string, s *shard) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.routes[id] = s
}

// forget drops a task the shard no longer has.
func (m *Manager) forget(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.routes, id)
}

// idNumber parses the number of a task ID such as "t7".
func idNumber(id string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(id, "t"))
	return n, err == nil && strings.HasPrefix(id, "t")
}
