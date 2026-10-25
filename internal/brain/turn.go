package brain

import (
	"context"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/brain/llm"
	"github.com/andr2116d/doggie-assistant/internal/brain/prompt"
	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/mcp"
)

// maxListedTasks is how many active tasks a prompt may list.
const maxListedTasks = 50

// turnJob is what a turn works on: a snapshot taken by the Run loop.
type turnJob struct {
	thread  string
	channel string
	inputs  []events.TurnInput
	history []prompt.Turn
	started time.Time
}

// runTurn runs one turn of the LLM: it says each text block as it comes and
// records the turn. It runs outside the Run loop and touches no brain state.
func (b *Brain) runTurn(ctx context.Context, job turnJob) events.BrainTurnData {
	rec := events.BrainTurnData{ThreadID: job.thread, Channel: job.channel, Inputs: job.inputs, Provider: b.provider.Name()}
	cause := job.inputs[len(job.inputs)-1].EventID
	defer func() {
		pctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
		defer cancel()
		if _, err := bus.Publish(pctx, b.conn, events.BrainTurn, rec, bus.CausedBy(cause)); err != nil {
			b.log.Error("brain: could not record a turn", "thread", job.thread, "err", err)
		}
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

	inputs := make([]prompt.Input, len(job.inputs))
	for i, in := range job.inputs {
		inputs[i] = promptInput(in)
	}
	msg := prompt.Build(prompt.Context{
		Now: time.Now(), Location: b.opts.Location, ThreadID: job.thread,
		Tasks: tasks, History: job.history, MaxTurns: b.opts.HistoryTurns, Inputs: inputs,
	})

	out, err := b.provider.Generate(ctx, llm.Request{Turn: mcp.Turn{ThreadID: job.thread, Channel: job.channel}, Input: msg})
	if err != nil {
		rec.Error = err.Error()
		rec.DurationMS = time.Since(job.started).Milliseconds()
		b.log.Warn("brain: turn failed", "thread", job.thread, "err", err)
		return rec
	}
	for o := range out {
		switch {
		case o.Text != "":
			rec.Replies = append(rec.Replies, o.Text)
			b.say(job, o.Text, cause)
		case o.Tool != nil:
			rec.Actions = append(rec.Actions, events.TurnAction{Tool: o.Tool.Name, Input: o.Tool.Input})
		case o.Done != nil:
			d := o.Done
			rec.Model = d.Model
			rec.FirstTextMS = d.FirstText.Milliseconds()
			if d.Usage != nil {
				rec.FiveHourUsage, rec.SevenDayUsage = d.Usage.FiveHour, d.Usage.SevenDay
			}
			if d.Err != nil {
				rec.Error = d.Err.Error()
				b.log.Warn("brain: turn failed", "thread", job.thread, "err", d.Err)
			}
		}
	}
	rec.DurationMS = time.Since(job.started).Milliseconds()
	return rec
}

// say publishes a text block for the thread's channel. It is published even
// while DOGGIE stops: it was already written.
func (b *Brain) say(job turnJob, text, cause string) {
	ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
	defer cancel()
	if _, err := bus.Publish(ctx, b.conn, events.OutSpeak, events.SpeakData{
		ThreadID: job.thread, Channel: job.channel, Text: text,
	}, bus.CausedBy(cause)); err != nil {
		b.log.Error("brain: could not say a reply", "thread", job.thread, "err", err)
	}
}
