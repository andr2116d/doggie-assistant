//go:build unix

package brain

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/prompts"
)

// TestRealResumeDoesNotRepeatAnAction: the brain died after a turn started
// a task but before it was recorded. When the brain comes back and resumes
// the sentence, the real LLM must not start the task a second time, whether
// it notices by itself or the tools refuse. Runs with DOGGIE_REAL_CLAUDE=1.
func TestRealResumeDoesNotRepeatAnAction(t *testing.T) {
	if os.Getenv("DOGGIE_REAL_CLAUDE") != "1" {
		t.Skip("set DOGGIE_REAL_CLAUDE=1 to run on the real claude CLI")
	}
	system, _ := prompts.System(prompts.Persona{AssistantName: "DOGGIE", OwnerName: "Alex", Language: "es"})
	for _, tc := range []struct{ name, said string }{
		{"explicit", "Abre Netflix en la tele, usa demo.sleep de 60 segundos."},
		{"vague", "Ponme Netflix en la tele (demo.sleep, 60 segundos)."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRealStackDown(t)
			ctx := context.Background()
			// The sentence, while the brain is down...
			if _, err := bus.Publish(ctx, r.client, events.UserUtterance.With("terminal"), events.UtteranceData{ThreadID: "th", Text: tc.said}); err != nil {
				t.Fatal(err)
			}
			// ...and the task the dead turn started for it.
			first, err := bus.Request(ctx, r.client, events.StartTask, events.TaskSpec{
				Kind: "demo.sleep", Description: "Abrir Netflix en la TV", ThreadID: "th", Input: json.RawMessage(`{"seconds":60}`),
			})
			if err != nil {
				t.Fatal(err)
			}
			r.startBrain(system, 1)

			turn := r.waitTurn("th")
			for _, a := range turn.Actions {
				t.Logf("action: %s %s", a.Tool, a.Input)
			}
			for _, s := range turn.Replies {
				t.Logf("said: %s", s)
			}
			list, _ := bus.Request(ctx, r.client, events.ListTasks, events.TaskListRequest{})
			for _, task := range list.Tasks {
				if task.ID != first.TaskID && task.Kind == "demo.sleep" && task.Status != events.StatusFailed && task.Status != events.StatusCancelled {
					t.Errorf("the action ran twice: %s %s %s", task.ID, task.Input, task.Status)
				}
			}
		})
	}
}

// TestRealGuardStopsARepeatTheLLMCantSee: the task the dead turn started has
// already finished, and nothing in the prompt says so. The LLM tries to
// start it again; the tools refuse, and it tells the user.
func TestRealGuardStopsARepeatTheLLMCantSee(t *testing.T) {
	if os.Getenv("DOGGIE_REAL_CLAUDE") != "1" {
		t.Skip("set DOGGIE_REAL_CLAUDE=1 to run on the real claude CLI")
	}
	system, _ := prompts.System(prompts.Persona{AssistantName: "DOGGIE", OwnerName: "Alex", Language: "es"})
	r := newRealStackDown(t)
	ctx := context.Background()
	if _, err := bus.Publish(ctx, r.client, events.UserUtterance.With("terminal"), events.UtteranceData{ThreadID: "th", Text: "Abre Netflix en la tele, usa demo.sleep de 1 segundo."}); err != nil {
		t.Fatal(err)
	}
	first, err := bus.Request(ctx, r.client, events.StartTask, events.TaskSpec{
		Kind: "demo.sleep", Description: "Abrir Netflix en la TV", ThreadID: "th", Input: json.RawMessage(`{"seconds":1}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := <-r.dones
	// Its notice was given elsewhere: the thread doesn't see it.
	bus.Publish(ctx, r.client, events.BrainTurn, events.BrainTurnData{ThreadID: "elsewhere", Channel: "terminal",
		Inputs: []events.TurnInput{{EventID: done.ID, Task: &events.TurnTask{Type: "task.done", TaskID: first.TaskID}}}, Replies: []string{"ok"}})
	r.startBrain(system, 1)

	turn := r.waitTurn("th")
	for _, a := range turn.Actions {
		t.Logf("action: %s %s", a.Tool, a.Input)
	}
	for _, s := range turn.Replies {
		t.Logf("said: %s", s)
	}
	list, _ := bus.Request(ctx, r.client, events.ListTasks, events.TaskListRequest{})
	for _, task := range list.Tasks {
		if task.ID != first.TaskID {
			t.Errorf("the action ran twice: %s %s %s", task.ID, task.Input, task.Status)
		}
	}
}
