package brain

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/brain/llm"
	"github.com/andr2116d/doggie-assistant/internal/brain/prompt"
	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/mcp"
	"github.com/andr2116d/doggie-assistant/prompts"
)

// maxListedTasks is how many active tasks a prompt may list.
const maxListedTasks = 50

// maxActedTasks is how many recent tasks are searched for those resumed
// sentences caused.
const maxActedTasks = 500

// turnJob is what a turn works on: a snapshot taken by the Run loop.
type turnJob struct {
	thread  string
	channel string
	inputs  []events.TurnInput
	history []prompt.Turn
	started time.Time
}

// runTurn runs one turn of the LLM: it says each text block as it comes and
// records the turn, returning where it was logged. It runs outside the Run
// loop and touches no brain state.
func (b *Brain) runTurn(ctx context.Context, job turnJob) (rec events.BrainTurnData, logged bus.Meta) {
	rec = events.BrainTurnData{ThreadID: job.thread, Channel: job.channel, Inputs: loggable(job.inputs), Provider: b.provider.Name()}
	cause := job.inputs[len(job.inputs)-1].EventID
	defer func() {
		pctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
		defer cancel()
		meta, err := bus.Publish(pctx, b.conn, events.BrainTurn, rec, bus.CausedBy(cause))
		if err != nil {
			b.log.Error("brain: could not record a turn", "thread", job.thread, "err", err)
		}
		logged = meta
	}()

	// The task manager is the source of truth for the tasks the prompt
	// lists; without it the turn goes on without them.
	var tasks []events.TaskInfo
	lctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	list, err := bus.Request(lctx, b.conn, events.ListTasks, events.TaskListRequest{ActiveOnly: true, Limit: maxListedTasks})
	cancel()
	if err != nil {
		b.log.Warn("brain: could not list the active tasks", "err", err)
	} else {
		tasks = list.Tasks
	}

	// The tasks the turn starts were caused by its sentences. A resumed
	// sentence may already have been acted on: the prompt tells what was
	// started for it, and the tools refuse to start it again.
	turn := mcp.Turn{ThreadID: job.thread, Channel: job.channel}
	for _, in := range job.inputs {
		if in.Said == "" {
			continue
		}
		turn.Said = append(turn.Said, in.EventID)
		if in.Resumed {
			turn.Resumed = append(turn.Resumed, in.EventID)
		}
	}
	acted := b.actedOn(ctx, turn.Resumed)

	inputs := make([]prompt.Input, len(job.inputs))
	for i, in := range job.inputs {
		inputs[i] = promptInput(in)
		if in.Resumed {
			inputs[i].Acted = acted[in.EventID]
		}
	}
	msg := prompt.Build(prompt.Context{
		Now: time.Now(), Location: b.opts.Location, ThreadID: job.thread,
		Tasks: tasks, History: job.history, MaxTurns: b.opts.HistoryTurns, Inputs: inputs,
	})
	block := 0
	out, err := b.provider.Generate(ctx, llm.Request{Turn: turn, Input: msg, Stream: true})
	if err != nil {
		b.failed(job, &rec, err, block, cause)
		rec.DurationMS = time.Since(job.started).Milliseconds()
		return rec, logged
	}
	var usage *llm.Usage
	// open is a block streamed in part but never finished: what DOGGIE says
	// itself goes in a block of its own.
	open := false
	for o := range out {
		switch {
		case o.Delta != "":
			open = true
			// Fire and forget: the out.speak that ends the block is the truth.
			bus.Emit(b.conn, events.OutSpeakDelta, events.SpeakDeltaData{
				ThreadID: job.thread, Channel: job.channel, Text: o.Delta, Block: block, CausedBy: cause,
			})
		case o.Text != "":
			rec.Replies = append(rec.Replies, o.Text)
			b.say(job, o.Text, block, cause)
			block++
			open = false
		case o.Tool != nil:
			rec.Actions = append(rec.Actions, events.TurnAction{Tool: o.Tool.Name, Input: o.Tool.Input})
		case o.Done != nil:
			d := o.Done
			rec.Model = d.Model
			rec.FirstTextMS = d.FirstText.Milliseconds()
			rec.RequestMS = d.Request.Milliseconds()
			if usage = d.Usage; usage != nil {
				rec.FiveHourUsage, rec.SevenDayUsage = usage.FiveHour, usage.SevenDay
			}
			if open {
				block, open = block+1, false
			}
			if d.Err != nil {
				block = b.failed(job, &rec, d.Err, block, cause)
			}
		}
	}
	// While DOGGIE stops, the warning waits for a turn someone sees.
	if rec.Failure != events.FailureUsageLimit && ctx.Err() == nil {
		b.warnLowUsage(job, &rec, usage, block, cause)
	}
	rec.DurationMS = time.Since(job.started).Milliseconds()
	return rec, logged
}

// say publishes a text block for the thread's channel. It is published even
// while DOGGIE stops: it was already written.
func (b *Brain) say(job turnJob, text string, block int, cause string) {
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	if _, err := bus.Publish(ctx, b.conn, events.OutSpeak, events.SpeakData{
		ThreadID: job.thread, Channel: job.channel, Text: text, Block: block,
	}, bus.CausedBy(cause)); err != nil {
		b.log.Error("brain: could not say a reply", "thread", job.thread, "err", err)
	}
}

// failed records why a turn failed. When the usage limit is spent, DOGGIE
// says so itself: the LLM can't. It returns the next block's number.
func (b *Brain) failed(job turnJob, rec *events.BrainTurnData, err error, block int, cause string) int {
	rec.Error = err.Error()
	b.log.Warn("brain: turn failed", "thread", job.thread, "err", err)
	var spent *llm.UsageLimitError
	switch {
	case errors.As(err, &spent):
		rec.Failure = events.FailureUsageLimit
		text := prompts.UsageSpent(b.opts.Language, spent.Renews, time.Now().In(b.opts.Location))
		rec.Replies = append(rec.Replies, text)
		b.say(job, text, block, cause)
		return block + 1
	case errors.Is(err, llm.ErrTurnTimeout):
		rec.Failure = events.FailureTimeout
	case errors.Is(err, llm.ErrToolsUnavailable):
		rec.Failure = events.FailureNoTools
	case errors.Is(err, llm.ErrNotLoggedIn):
		rec.Failure = events.FailureNotLoggedIn
	default:
		rec.Failure = events.FailureOther
	}
	return block
}

// warnLowUsage tells the owner, once per usage window, that little of it is
// left.
func (b *Brain) warnLowUsage(job turnJob, rec *events.BrainTurnData, usage *llm.Usage, block int, cause string) {
	now := time.Now()
	low := b.warnings.claim(usage, now)
	if len(low) == 0 {
		return
	}
	rec.LowUsage = low
	text := prompts.UsageLow(b.opts.Language, low[0].Renews, now.In(b.opts.Location))
	rec.Replies = append(rec.Replies, text)
	b.say(job, text, block, cause)
}

// loggable is what a turn's inputs may leave in the event log: the
// questions of sensitive tasks stay out of it, as their results do.
func loggable(inputs []events.TurnInput) []events.TurnInput {
	out := slices.Clone(inputs)
	for i, in := range out {
		if in.Task != nil && in.Task.Sensitive {
			task := *in.Task
			task.Question, task.Options = "", nil
			out[i].Task = &task
		}
	}
	return out
}

// actedOn returns the tasks each of the given sentences caused, oldest
// first. Without the task manager it returns none: the tools refuse to
// start anything they can't check.
func (b *Brain) actedOn(ctx context.Context, said []string) map[string][]events.TaskInfo {
	if len(said) == 0 {
		return nil
	}
	lctx, cancel := context.WithTimeout(ctx, lookupTimeout)
	defer cancel()
	list, err := bus.Request(lctx, b.conn, events.ListTasks, events.TaskListRequest{Limit: maxActedTasks})
	if err != nil {
		b.log.Warn("brain: could not list what resumed sentences caused", "err", err)
		return nil
	}
	out := make(map[string][]events.TaskInfo)
	for _, t := range list.Tasks {
		for _, id := range t.CausedBy {
			if slices.Contains(said, id) {
				out[id] = append(out[id], t)
			}
		}
	}
	return out
}
