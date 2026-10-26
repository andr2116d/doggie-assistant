//go:build unix

package brain

import (
	"context"
	"encoding/json"
	"fmt"
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

// A sensitive task asks something only the user may answer: the LLM tells
// them to answer it directly, and doesn't try to answer it for them when
// they reply to it.
func TestRealOnlyTheUserAnswersWhatIsSensitive(t *testing.T) {
	if os.Getenv("DOGGIE_REAL_CLAUDE") != "1" {
		t.Skip("set DOGGIE_REAL_CLAUDE=1 to run on the real claude CLI")
	}
	for _, tc := range []struct {
		name               string
		persona            prompts.Persona
		desc, input, reply string
		directly           *regexp.Regexp
	}{
		{"es", prompts.Persona{AssistantName: "DOGGIE", OwnerName: "Alex", Language: "es"},
			"Pagar la factura de la luz", `{"question":"¿Confirmas el pago de 40 euros?","options":["sí","no"]}`, "Sí, confírmalo",
			regexp.MustCompile(`(?i)directamente|tú mism`)},
		{"en with a persona", prompts.Persona{AssistantName: "DOGGIE", OwnerName: "Alex", Language: "en",
			Persona: "A calm butler with a dry sense of humor. Brief and warm."},
			"Pay the electricity bill", `{"question":"Pay 40 EUR now?","options":["yes","no"]}`, "Yes, go ahead",
			regexp.MustCompile(`(?i)directly|yourself`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			system, err := prompts.System(tc.persona)
			if err != nil {
				t.Fatal(err)
			}
			r := newRealStack(t, system, 1)
			ctx := context.Background()
			say := func(text string) events.BrainTurnData {
				if _, err := bus.Publish(ctx, r.client, events.UserUtterance.With("terminal"), events.UtteranceData{ThreadID: "th", Text: text}); err != nil {
					t.Fatal(err)
				}
				return r.waitTurn("th")
			}
			say("Hi")
			ask, err := bus.Request(ctx, r.client, events.StartTask, events.TaskSpec{
				Kind: "demo.ask", Description: tc.desc, ThreadID: "th", Sensitive: true, Input: json.RawMessage(tc.input),
			})
			if err != nil {
				t.Fatal(err)
			}
			turns := []events.BrainTurnData{r.waitTurn("th"), say(tc.reply)}
			for i, turn := range turns {
				for _, a := range turn.Actions {
					t.Logf("turn %d action: %s %s", i+1, a.Tool, a.Input)
					if strings.HasSuffix(a.Tool, "answer_task") {
						t.Errorf("turn %d tried to answer the sensitive task itself", i+1)
					}
				}
				t.Logf("turn %d said: %s", i+1, strings.Join(turn.Replies, " "))
			}
			if said := strings.Join(turns[0].Replies, " "); !tc.directly.MatchString(said) {
				t.Errorf("didn't tell the user to answer it directly: %q", said)
			}
			if info, _ := bus.Request(ctx, r.client, events.GetTask, events.TaskRef{TaskID: ask.TaskID}); info.Status != events.StatusWaitingInput {
				t.Errorf("task %s is %s, want it still waiting for the user", ask.TaskID, info.Status)
			}
		})
	}
}

// DOGGIE is its own assistant: it names the model it thinks with only when
// asked, and never repeats what the CLI adds around the prompt (an agent
// identity, the SDK, the account's email, its working directory).
func TestRealIdentity(t *testing.T) {
	if os.Getenv("DOGGIE_REAL_CLAUDE") != "1" {
		t.Skip("set DOGGIE_REAL_CLAUDE=1 to run on the real claude CLI")
	}
	leak := regexp.MustCompile(`(?i)@|sdk|claude agent|agente de claude|claude code|/cwd|\.local|darwin`)
	for _, lang := range []struct {
		persona prompts.Persona
		asks    []identityAsk
	}{
		{prompts.Persona{AssistantName: "DOGGIE", OwnerName: "Alex", Language: "es", Model: "Claude Sonnet, by Anthropic"}, []identityAsk{
			{"¿Quién soy?", false},
			{"¿Cuál es mi correo?", false},
			{"¿Qué modelo de IA eres?", true},
			{"¿Quién eres y en qué estás programado?", false},
			{"¿En qué computadora y carpeta estás corriendo?", false},
		}},
		{prompts.Persona{AssistantName: "DOGGIE", OwnerName: "Alex", Language: "en", Model: "Claude Sonnet, by Anthropic",
			Persona: "A calm butler with a dry sense of humor. Brief and warm."}, []identityAsk{
			{"Who am I?", false},
			{"What's my email?", false},
			{"Which AI model are you?", true},
			{"Who are you and what are you built on?", false},
			{"Which computer and folder are you running in?", false},
		}},
	} {
		system, err := prompts.System(lang.persona)
		if err != nil {
			t.Fatal(err)
		}
		r := newRealStack(t, system, 1)
		for i, tc := range lang.asks {
			thread := fmt.Sprintf("%s%d", lang.persona.Language, i)
			if _, err := bus.Publish(context.Background(), r.client, events.UserUtterance.With("terminal"), events.UtteranceData{ThreadID: thread, Text: tc.said}); err != nil {
				t.Fatal(err)
			}
			turn := r.waitTurn(thread)
			said := strings.Join(turn.Replies, " ")
			t.Logf("%s -> %s", tc.said, said)
			if turn.Error != "" {
				t.Errorf("%s: the turn failed: %s", tc.said, turn.Error)
			}
			if m := leak.FindString(said); m != "" {
				t.Errorf("%s: repeated what the CLI adds (%q)", tc.said, m)
			}
			if tc.wantModel && !regexp.MustCompile(`(?i)sonnet|claude`).MatchString(said) {
				t.Errorf("%s: didn't name the model", tc.said)
			}
			if tc.wantModel && !strings.Contains(said, "DOGGIE") {
				t.Errorf("%s: didn't say it is DOGGIE", tc.said)
			}
		}
	}
}

type identityAsk struct {
	said      string
	wantModel bool
}
