//go:build unix

package brain

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

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
		{"explicit", "Pon un temporizador, usa demo.sleep de 60 segundos."},
		{"vague", "Avísame en un minuto (demo.sleep, 60 segundos)."},
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
				Kind: "demo.sleep", Description: "Poner un temporizador", ThreadID: "th", Input: json.RawMessage(`{"seconds":60}`),
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
	if _, err := bus.Publish(ctx, r.client, events.UserUtterance.With("terminal"), events.UtteranceData{ThreadID: "th", Text: "Pon un temporizador, usa demo.sleep de 1 segundo."}); err != nil {
		t.Fatal(err)
	}
	first, err := bus.Request(ctx, r.client, events.StartTask, events.TaskSpec{
		Kind: "demo.sleep", Description: "Poner un temporizador", ThreadID: "th", Input: json.RawMessage(`{"seconds":1}`),
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

// A similar task running for another conversation doesn't do what this
// one asks: the LLM starts its own, and doesn't talk about the other's.
func TestRealOtherConversationsTasksAreNotOurs(t *testing.T) {
	if os.Getenv("DOGGIE_REAL_CLAUDE") != "1" {
		t.Skip("set DOGGIE_REAL_CLAUDE=1 to run on the real claude CLI")
	}
	system, err := prompts.System(prompts.Persona{AssistantName: "DOGGIE", OwnerName: "Alex", Language: "es"})
	if err != nil {
		t.Fatal(err)
	}
	r := newRealStack(t, system, 1)
	say := func(thread, text string) events.BrainTurnData {
		if _, err := bus.Publish(context.Background(), r.client, events.UserUtterance.With("terminal"), events.UtteranceData{ThreadID: thread, Text: text}); err != nil {
			t.Fatal(err)
		}
		return r.waitTurn(thread)
	}
	// The same request in both: the trap is taking the other's for ours.
	const ask = "Espera 30 segundos y después dime qué hora es"
	if turn := say("other", ask); len(turn.Actions) == 0 {
		t.Fatalf("the other conversation started nothing: %q", turn.Replies)
	}
	time.Sleep(8 * time.Second)
	if turn := say("mine", ask); len(turn.Actions) == 0 {
		t.Errorf("took the other conversation's wait as its own: %q", turn.Replies)
	}
	reply := strings.Join(say("mine2", "¿Cómo has estado?").Replies, " ")
	if regexp.MustCompile(`(?i)espera|\d+\s*(%|seg)|va por`).MatchString(reply) {
		t.Errorf("brought up another conversation's task: %q", reply)
	}
}
