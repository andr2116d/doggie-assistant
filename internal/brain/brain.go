// Package brain is the brain component: it turns what people say, and what
// happens to their tasks, into replies and actions.
//
// Each conversation thread has a queue. A turn takes everything queued for
// its thread, builds the whole context (internal/brain/prompt), runs the LLM
// provider with DOGGIE's MCP tools bound to the thread, says each text block
// with out.speak and records the turn with brain.turn. Whatever arrives
// during a turn waits for the next one. When a task requested by a thread
// ends or needs an answer, the event joins that thread's queue, so the LLM
// tells the user in context.
//
// The brain is an actor: all state lives in the Run goroutine. Turns and
// task lookups run in goroutines that send their outcome back to the loop.
package brain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/andr2116d/doggie-assistant/internal/brain/llm"
	"github.com/andr2116d/doggie-assistant/internal/brain/prompt"
	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/mcp"
)

// Bounds on what satellites can make the brain hold.
const (
	maxIDBytes     = 64
	maxThreads     = 100
	maxPending     = 50
	maxCachedTasks = 1000
)

const (
	// lookupTimeout bounds a request to the task manager.
	lookupTimeout = 3 * time.Second
	// publishTimeout bounds publishing a turn's events, which happens even
	// while DOGGIE stops.
	publishTimeout = 5 * time.Second
)

// catchUpWindow is how far back the brain looks, on start, for task events
// no turn answered. A variable so tests can shrink it.
var catchUpWindow = 10 * time.Minute

// Provider starts the LLM provider for one run of the brain, with DOGGIE's
// tools. stop releases it.
type Provider func(tools *mcp.Server) (brain llm.Brain, stop func(), err error)

// Options configures the brain.
type Options struct {
	NewProvider Provider
	// Location is the owner's time zone. Nil means the system's.
	Location *time.Location
	// HistoryTurns is how many past turns of a thread go into a prompt.
	HistoryTurns int
	// MaxTurns is how many turns may run at once. Zero means 2.
	MaxTurns int
}

// Brain is the brain component.
type Brain struct {
	conn *bus.Conn
	log  *slog.Logger
	opts Options

	// State of the current run, owned by the Run goroutine.
	provider llm.Brain
	threads  map[string]*thread
	order    []*thread // threads waiting for a free turn slot, FIFO
	running  int
	tasks    map[string]events.TaskInfo // tasks seen being created
	ended    map[string]bool            // tasks whose end was already queued
	lookups  map[string][]bus.Envelope  // task events waiting for a lookup, by task
	seen     map[string]bool            // task events read from the log on start
	results  chan func()                // outcomes of turns and lookups
	turns    sync.WaitGroup
	tick     uint64 // logical clock for thread recency
}

type thread struct {
	id      string
	channel string // where replies go: the channel the user last spoke on
	history []prompt.Turn
	pending []events.TurnInput
	busy    bool
	waiting bool // in Brain.order
	used    uint64
	// cancelled are tasks the LLM cancelled itself: their task.cancelled
	// event needs no announcement.
	cancelled map[string]bool
}

// New returns the brain component.
func New(conn *bus.Conn, log *slog.Logger, opts Options) *Brain {
	if opts.Location == nil {
		opts.Location = time.Local
	}
	if opts.MaxTurns <= 0 {
		opts.MaxTurns = 2
	}
	return &Brain{conn: conn, log: log, opts: opts}
}

// Name implements supervisor.Component.
func (b *Brain) Name() string { return "brain" }

// Run implements supervisor.Component.
func (b *Brain) Run(ctx context.Context) error {
	if b.opts.NewProvider == nil {
		return errors.New("brain: no provider")
	}
	b.threads = make(map[string]*thread)
	b.order = nil
	b.running = 0
	b.tasks = make(map[string]events.TaskInfo)
	b.ended = make(map[string]bool)
	b.lookups = make(map[string][]bus.Envelope)
	b.seen = make(map[string]bool)
	b.results = make(chan func())

	tools, err := mcp.Listen(b.conn, b.log)
	if err != nil {
		return err
	}
	// The tools outlive every turn: they stop after the turns have ended.
	toolsCtx, stopTools := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := tools.Serve(toolsCtx); err != nil && !errors.Is(err, context.Canceled) {
			b.log.Error("brain: MCP server stopped", "err", err)
		}
	}()
	defer func() {
		stopTools()
		<-served
	}()

	provider, stopProvider, err := b.opts.NewProvider(tools)
	if err != nil {
		return fmt.Errorf("brain: provider: %w", err)
	}
	b.provider = provider
	defer stopProvider()

	// Subscribe before reading the history, so nothing falls in between.
	// Every channel publishes under its own name, which the bus enforces.
	utterances, err := bus.SubscribeEnvelopes(ctx, b.conn, events.UserUtterance.Type()+".*")
	if err != nil {
		return err
	}
	taskEvents, err := bus.SubscribeEnvelopes(ctx, b.conn, "task.>")
	if err != nil {
		return err
	}
	if err := b.recover(ctx); err != nil {
		return err
	}
	b.log.Info("brain ready", "provider", provider.Name(), "threads", len(b.threads))
	// What recover caught up on runs now, not when the next event comes.
	b.schedule(ctx)

	// Turns end on their own once ctx is cancelled; wait for them so their
	// last events are published before the tools and the provider stop.
	defer b.turns.Wait()
	for {
		select {
		case <-ctx.Done():
			b.drain()
			return ctx.Err()
		case env, ok := <-utterances:
			if !ok {
				b.drain()
				return ctx.Err()
			}
			if b.seen[env.ID] {
				continue // already read from the log
			}
			b.onUtterance(env, false)
		case env, ok := <-taskEvents:
			if !ok {
				b.drain()
				return ctx.Err()
			}
			if b.seen[env.ID] {
				continue // already read from the log
			}
			b.onTaskEvent(ctx, env)
		case f := <-b.results:
			f()
		}
		b.schedule(ctx)
	}
}

// drain runs the outcomes of turns still in flight, so they don't block on
// sending them, until all have ended.
func (b *Brain) drain() {
	done := make(chan struct{})
	go func() {
		b.turns.Wait()
		close(done)
	}()
	for {
		select {
		case f := <-b.results:
			f()
		case <-done:
			return
		}
	}
}

// do sends f to the Run loop, unless the run is over.
func (b *Brain) do(ctx context.Context, results chan func(), f func()) {
	select {
	case results <- f:
	case <-ctx.Done():
		// drain still reads results while turns end, so a turn's outcome
		// is not lost; a lookup's outcome may be.
		select {
		case results <- f:
		case <-time.After(publishTimeout):
		}
	}
}

// recover rebuilds the threads' history from the brain.turn events in the
// log, then catches up on the task events no turn answered.
func (b *Brain) recover(ctx context.Context) error {
	logged, err := bus.History(ctx, b.conn, time.Time{}, events.BrainTurn.Type())
	if err != nil {
		return fmt.Errorf("brain: read history: %w", err)
	}
	answered := make(map[string]bool)
	for _, env := range logged {
		ev, err := bus.Decode(env, events.BrainTurn)
		if err != nil {
			b.log.Warn("brain: skipping undecodable turn", "id", env.ID, "err", err)
			continue
		}
		if !validID(ev.Data.ThreadID) {
			continue
		}
		th := b.thread(ev.Data.ThreadID)
		if th == nil {
			continue
		}
		if ev.Data.Channel != "" {
			th.channel = ev.Data.Channel
		}
		b.remember(th, ev.Time, ev.Data)
		// A turn that failed without saying or doing anything answered
		// nothing, so its inputs are caught up again (no action can repeat).
		// Only once: a sentence that keeps failing, or crashing the brain,
		// must not be retried on every restart.
		if ev.Data.Error != "" && len(ev.Data.Replies) == 0 && len(ev.Data.Actions) == 0 {
			for _, in := range ev.Data.Inputs {
				if in.Resumed {
					answered[in.EventID] = true
				}
			}
			continue
		}
		for _, in := range ev.Data.Inputs {
			answered[in.EventID] = true
			if in.Task != nil && in.Task.Type != events.TaskNeedsInput.Type() {
				b.ended[in.Task.TaskID] = true
			}
		}
	}
	return b.catchUp(ctx, answered)
}

// catchUp routes the recent events no turn answered, in the order they
// happened: task events (e.g. the tasks a restart interrupted, which the task
// manager fails while the brain is starting) and sentences that came while
// the brain was down or was stopped before answering them, so DOGGIE never
// goes silent on someone. The turn that died may have acted on a sentence
// already, so those are marked resumed: the LLM checks the tasks before
// acting again.
func (b *Brain) catchUp(ctx context.Context, answered map[string]bool) error {
	since := time.Now().Add(-catchUpWindow)
	tasks, err := bus.History(ctx, b.conn, since, "task.>")
	if err != nil {
		return fmt.Errorf("brain: read recent task events: %w", err)
	}
	said, err := bus.History(ctx, b.conn, since, events.UserUtterance.Type()+".*")
	if err != nil {
		return fmt.Errorf("brain: read recent utterances: %w", err)
	}
	recent := append(tasks, said...)
	slices.SortStableFunc(recent, func(x, y bus.Envelope) int { return x.Time.Compare(y.Time) })
	for _, env := range recent {
		b.seen[env.ID] = true
		switch {
		case answered[env.ID]:
		case strings.HasPrefix(env.Type, events.UserUtterance.Type()+"."):
			b.onUtterance(env, true)
		default:
			b.onTaskEvent(ctx, env)
		}
	}
	return nil
}

// thread returns the thread with id, creating it. With too many threads it
// forgets the least recently used idle one; if none is idle, it returns nil:
// a flood of new threads can't grow the brain's memory.
func (b *Brain) thread(id string) *thread {
	b.tick++
	if th, ok := b.threads[id]; ok {
		th.used = b.tick
		return th
	}
	if len(b.threads) >= maxThreads {
		var oldest *thread
		for _, th := range b.threads {
			if !th.busy && len(th.pending) == 0 && (oldest == nil || th.used < oldest.used) {
				oldest = th
			}
		}
		if oldest == nil {
			return nil
		}
		delete(b.threads, oldest.id)
	}
	th := &thread{id: id, channel: id, used: b.tick, cancelled: make(map[string]bool)}
	b.threads[id] = th
	return th
}

// remember adds a finished turn to its thread's history.
func (b *Brain) remember(th *thread, at time.Time, rec events.BrainTurnData) {
	for _, a := range rec.Actions {
		if a.Tool == "cancel_task" {
			var in struct {
				TaskID string `json:"task_id"`
			}
			if json.Unmarshal(a.Input, &in) == nil && in.TaskID != "" {
				th.cancelled[in.TaskID] = true
			}
		}
	}
	// A turn that failed without doing anything left no trace for the LLM.
	if rec.Error != "" && len(rec.Replies) == 0 && len(rec.Actions) == 0 {
		return
	}
	t := prompt.Turn{At: at, Replies: rec.Replies}
	for _, in := range rec.Inputs {
		t.Inputs = append(t.Inputs, promptInput(in))
	}
	for _, a := range rec.Actions {
		t.Actions = append(t.Actions, prompt.Action{Tool: a.Tool, Input: a.Input})
	}
	th.history = append(th.history, t)
	if max := max(b.opts.HistoryTurns, 1); len(th.history) > max {
		th.history = append([]prompt.Turn(nil), th.history[len(th.history)-max:]...)
	}
	// Bound the cancellations remembered: they only matter for a while.
	if len(th.cancelled) > maxPending {
		clear(th.cancelled)
	}
}

// onUtterance queues a sentence for its thread. The channel comes from the
// event's subject, which the bus only lets each satellite use for its own
// channels; a payload claiming another channel is a forgery.
func (b *Brain) onUtterance(env bus.Envelope, resumed bool) {
	ev, channel, err := bus.DecodeUnder(env, events.UserUtterance)
	if err != nil {
		b.log.Warn("brain: dropping undecodable utterance", "id", env.ID, "err", err)
		return
	}
	u := ev.Data
	if u.Channel != "" && u.Channel != channel {
		b.log.Warn("brain: dropping an utterance that claims another channel", "id", env.ID, "subject", channel, "claimed", u.Channel, "source", env.Source)
		return
	}
	u.Channel = channel
	text := strings.TrimSpace(u.Text)
	threadID := u.ThreadID
	if threadID == "" {
		threadID = u.Channel
	}
	switch {
	case !events.ValidChannel(u.Channel) || !validID(threadID):
		b.log.Warn("brain: ignoring utterance with an invalid channel or thread", "id", ev.ID)
		return
	case text == "" || !utf8.ValidString(text):
		b.log.Warn("brain: ignoring empty or invalid utterance", "id", ev.ID)
		return
	case utf8.RuneCountInString(text) > events.MaxUtteranceRunes:
		b.log.Warn("brain: ignoring a too long utterance", "id", ev.ID)
		return
	}
	th := b.thread(threadID)
	if th == nil {
		b.log.Warn("brain: too many busy threads, ignoring an utterance for a new one", "thread", threadID)
		return
	}
	th.channel = u.Channel
	b.enqueue(th, events.TurnInput{EventID: ev.ID, At: ev.Time, Said: text, Channel: u.Channel, Resumed: resumed})
}

// onTaskEvent routes a task event to the thread that asked for the task.
func (b *Brain) onTaskEvent(ctx context.Context, env bus.Envelope) {
	switch env.Type {
	case events.TaskCreated.Type():
		ev, err := bus.Decode(env, events.TaskCreated)
		if err != nil {
			return
		}
		if len(b.tasks) >= maxCachedTasks {
			clear(b.tasks) // stale entries are looked up again if needed
		}
		b.tasks[ev.Data.ID] = ev.Data
		return
	case events.TaskDone.Type(), events.TaskFailed.Type(), events.TaskCancelled.Type(), events.TaskNeedsInput.Type():
	default:
		return // running, progress: the next prompt shows them
	}
	var ref events.TaskRef
	if err := json.Unmarshal(env.Data, &ref); err != nil || !validID(ref.TaskID) {
		return
	}
	// Keep the events of a task in order: one waiting for a lookup holds
	// back the ones behind it.
	if q, ok := b.lookups[ref.TaskID]; ok {
		b.lookups[ref.TaskID] = append(q, env)
		return
	}
	if info, ok := b.tasks[ref.TaskID]; ok {
		b.taskEvent(info, env)
		return
	}
	b.lookups[ref.TaskID] = []bus.Envelope{env}
	results := b.results
	go func() {
		lctx, cancel := context.WithTimeout(ctx, lookupTimeout)
		defer cancel()
		info, err := bus.Request(lctx, b.conn, events.GetTask, ref)
		b.do(ctx, results, func() { b.looked(ref.TaskID, info, err) })
	}()
}

// looked handles the answer to a task lookup.
func (b *Brain) looked(id string, info events.TaskInfo, err error) {
	queued := b.lookups[id]
	delete(b.lookups, id)
	if err != nil {
		b.log.Warn("brain: could not look up a task, dropping its events", "task", id, "err", err)
		return
	}
	for _, env := range queued {
		b.taskEvent(info, env)
	}
}

// taskEvent queues a task event for the thread that asked for the task.
func (b *Brain) taskEvent(info events.TaskInfo, env bus.Envelope) {
	terminal := env.Type != events.TaskNeedsInput.Type()
	switch {
	case b.ended[info.ID]:
		// A task ends once: a second end (e.g. done racing a timeout) is
		// one the manager rejected.
		return
	case !terminal && info.Status.Terminal():
		return // a question that no longer matters
	case terminal:
		delete(b.tasks, info.ID)
		if len(b.ended) >= maxCachedTasks {
			clear(b.ended)
		}
		b.ended[info.ID] = true
	}
	th, ok := b.threads[info.ThreadID]
	if !ok || info.ThreadID == "" {
		return // nobody DOGGIE talks to asked for it
	}
	in := events.TurnInput{EventID: env.ID, At: env.Time, Task: &events.TurnTask{
		Type: env.Type, TaskID: info.ID, Kind: info.Kind, Description: info.Description, Sensitive: info.Sensitive,
	}}
	switch env.Type {
	case events.TaskDone.Type():
		var d events.TaskDoneData
		json.Unmarshal(env.Data, &d)
		if !info.Sensitive && !d.Redacted {
			in.Task.Result = d.Result
		}
	case events.TaskFailed.Type():
		var d events.TaskFailedData
		json.Unmarshal(env.Data, &d)
		in.Task.Reason = d.Error
	case events.TaskCancelled.Type():
		var d events.TaskCancelledData
		json.Unmarshal(env.Data, &d)
		in.Task.Reason = d.Reason
	case events.TaskNeedsInput.Type():
		var d events.TaskNeedsInputData
		json.Unmarshal(env.Data, &d)
		in.Task.Question, in.Task.Options = d.Question, d.Options
	}
	b.enqueue(th, in)
}

func (b *Brain) enqueue(th *thread, in events.TurnInput) {
	if len(th.pending) >= maxPending {
		b.log.Warn("brain: thread queue full, dropping the oldest input", "thread", th.id)
		th.pending = th.pending[1:]
	}
	th.pending = append(th.pending, in)
	if !th.busy && !th.waiting {
		th.waiting = true
		b.order = append(b.order, th)
	}
}

// schedule starts turns while there are free slots and threads waiting.
func (b *Brain) schedule(ctx context.Context) {
	for b.running < b.opts.MaxTurns && len(b.order) > 0 && ctx.Err() == nil {
		th := b.order[0]
		b.order = b.order[1:]
		th.waiting = false
		if b.threads[th.id] != th {
			continue // forgotten meanwhile
		}
		inputs := b.relevant(th)
		if len(inputs) == 0 {
			continue
		}
		th.busy = true
		b.running++
		job := turnJob{
			thread:  th.id,
			channel: th.channel,
			inputs:  inputs,
			history: append([]prompt.Turn(nil), th.history...),
			started: time.Now(),
		}
		b.turns.Add(1)
		results := b.results
		go func() {
			defer b.turns.Done()
			rec := b.runTurn(ctx, job)
			b.do(ctx, results, func() { b.turnEnded(th, job, rec) })
		}()
	}
}

// relevant takes a thread's pending inputs, leaving out the cancellations
// the LLM asked for itself.
func (b *Brain) relevant(th *thread) []events.TurnInput {
	var out []events.TurnInput
	for _, in := range th.pending {
		if in.Task != nil && in.Task.Type == events.TaskCancelled.Type() && th.cancelled[in.Task.TaskID] {
			delete(th.cancelled, in.Task.TaskID)
			continue
		}
		out = append(out, in)
	}
	th.pending = nil
	return out
}

func (b *Brain) turnEnded(th *thread, job turnJob, rec events.BrainTurnData) {
	b.running--
	th.busy = false
	b.remember(th, job.started, rec)
	if len(th.pending) > 0 && !th.waiting {
		th.waiting = true
		b.order = append(b.order, th)
	}
}

func validID(s string) bool {
	return s != "" && len(s) <= maxIDBytes && utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsControl(r) || unicode.IsSpace(r)
	})
}

func promptInput(in events.TurnInput) prompt.Input {
	p := prompt.Input{At: in.At, Said: in.Said, Channel: in.Channel, Resumed: in.Resumed}
	if t := in.Task; t != nil {
		p.Event = &prompt.TaskEvent{
			Type: t.Type,
			Task: events.TaskInfo{ID: t.TaskID, TaskSpec: events.TaskSpec{
				Kind: t.Kind, Description: t.Description, Sensitive: t.Sensitive,
			}},
			Result: t.Result, Reason: t.Reason, Question: t.Question, Options: t.Options,
		}
	}
	return p
}
