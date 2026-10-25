package brain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/brain/llm"
	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/mcp"
)

func TestSaysTheReplyAndRecordsTheTurn(t *testing.T) {
	h := newHarness(t, Options{})
	h.fake.set(func(f *fake) {
		f.reply = func(context.Context, llm.Request) []llm.Output {
			return []llm.Output{
				{Text: "Ya estoy en eso."},
				{Tool: &llm.ToolCall{Name: "list_tasks", Input: json.RawMessage(`{}`)}},
				{Text: "¿Sabes por qué…?"},
				{Done: &llm.Done{Model: "m1", FirstText: 900 * time.Millisecond, Usage: &llm.Usage{FiveHour: 0.2, SevenDay: 0.4}}},
			}
		}
	})
	id := h.say("", "terminal", "  recuérdame comprar pan y cuéntame un chiste  ")

	for _, want := range []string{"Ya estoy en eso.", "¿Sabes por qué…?"} {
		s := h.nextSpeak()
		if s.Data.Text != want || s.Data.ThreadID != "terminal" || s.Data.Channel != "terminal" || s.CausedBy != id {
			t.Errorf("out.speak = %+v (caused by %s), want %q caused by %s", s.Data, s.CausedBy, want, id)
		}
	}
	turn := h.nextTurn()
	d := turn.Data
	if d.ThreadID != "terminal" || d.Channel != "terminal" || d.Provider != "fake" || d.Model != "m1" || d.FirstTextMS != 900 || d.Error != "" || turn.CausedBy != id {
		t.Errorf("turn = %+v", d)
	}
	if len(d.Inputs) != 1 || d.Inputs[0].Said != "recuérdame comprar pan y cuéntame un chiste" || d.Inputs[0].EventID != id {
		t.Errorf("inputs = %+v", d.Inputs)
	}
	if len(d.Replies) != 2 || len(d.Actions) != 1 || d.Actions[0].Tool != "list_tasks" || d.FiveHourUsage != 0.2 || d.SevenDayUsage != 0.4 {
		t.Errorf("turn = %+v", d)
	}
	// The provider got the thread to act for and the built context.
	reqs := h.fake.requests()
	if len(reqs) != 1 || reqs[0].Turn != (mcp.Turn{ThreadID: "terminal", Channel: "terminal"}) {
		t.Fatalf("requests = %+v", reqs)
	}
	mustContain(t, reqs[0].Input, "<world>", "Active tasks: none", `<said channel="terminal"`, "recuérdame comprar pan y cuéntame un chiste</said>")
}

func TestInputsArrivingMidTurnWaitForTheNextTurn(t *testing.T) {
	h := newHarness(t, Options{})
	gate := make(chan struct{})
	h.fake.set(func(f *fake) { f.gate = gate })
	h.say("th", "terminal", "uno")
	waitFor(t, "the first turn to start", func() bool { return len(h.fake.requests()) == 1 })
	h.say("th", "terminal", "dos")
	h.say("th", "terminal", "tres")
	time.Sleep(100 * time.Millisecond)
	if n := len(h.fake.requests()); n != 1 {
		t.Fatalf("%d turns ran at once in one thread", n)
	}
	close(gate)
	first, second := h.nextTurn(), h.nextTurn()
	if len(first.Data.Inputs) != 1 || len(second.Data.Inputs) != 2 {
		t.Errorf("inputs per turn: %d then %d, want 1 then 2", len(first.Data.Inputs), len(second.Data.Inputs))
	}
	in := h.fake.requests()[1].Input
	mustContain(t, in, ">dos</said>", ">tres</said>")
	if strings.Index(in, ">dos<") > strings.Index(in, ">tres<") {
		t.Error("queued inputs out of order")
	}
	// The second prompt has the first turn as history.
	mustContain(t, in, "<conversation>", ">uno</said>", "<reply>respuesta 1</reply>")
	h.noTurn(200 * time.Millisecond)
}

// An order and a joke, then the task's end is told in the same thread.
func TestTaskEndIsAnnouncedInItsThread(t *testing.T) {
	h := newHarness(t, Options{})
	var taskID string
	h.fake.set(func(f *fake) {
		f.reply = func(ctx context.Context, req llm.Request) []llm.Output {
			if strings.Contains(req.Input, "recuérdame comprar pan") && !strings.Contains(req.Input, "<event") {
				var tool llm.Output
				taskID, tool = h.startTask(ctx, req.Turn.ThreadID, "demo.sleep", "Recordar el pan")
				return []llm.Output{{Text: "Ya estoy en eso."}, tool, {Text: "Un chiste…"}}
			}
			return []llm.Output{{Text: "Listo, te lo recuerdo."}}
		}
	})
	h.say("terminal", "terminal", "recuérdame comprar pan y cuéntame un chiste")
	if turn := h.nextTurn(); len(turn.Data.Actions) != 1 || len(turn.Data.Replies) != 2 {
		t.Fatalf("first turn = %+v", turn.Data)
	}
	h.release <- struct{}{}
	turn := h.nextTurn()
	if len(turn.Data.Inputs) != 1 || turn.Data.Inputs[0].Task == nil {
		t.Fatalf("second turn inputs = %+v", turn.Data.Inputs)
	}
	task := turn.Data.Inputs[0].Task
	if task.Type != "task.done" || task.TaskID != taskID || task.Kind != "demo.sleep" || task.Description != "Recordar el pan" || string(task.Result) != `{"ok":true}` {
		t.Errorf("task input = %+v", task)
	}
	in := h.fake.requests()[1].Input
	mustContain(t, in, `<event type="task.done" task="`+taskID+`"`, `finished. Result: <data>{"ok":true}</data>`)
}

func TestTaskEndingDuringTheTurnIsAnnouncedAfterIt(t *testing.T) {
	h := newHarness(t, Options{})
	close(h.release) // tasks end at once
	finishedMidTurn := make(chan struct{})
	h.fake.set(func(f *fake) {
		f.reply = func(ctx context.Context, req llm.Request) []llm.Output {
			if strings.Contains(req.Input, "<event") {
				return []llm.Output{{Text: "Listo."}}
			}
			id, tool := h.startTask(ctx, req.Turn.ThreadID, "demo.sleep", "Rápida")
			h.waitStatus(id, events.StatusDone) // the task is over before the turn
			close(finishedMidTurn)
			return []llm.Output{tool, {Text: "Voy."}}
		}
	})
	h.say("th", "terminal", "haz algo rápido")
	<-finishedMidTurn
	first, second := h.nextTurn(), h.nextTurn()
	if first.Data.Inputs[0].Said == "" || second.Data.Inputs[0].Task == nil || second.Data.Inputs[0].Task.Type != "task.done" {
		t.Errorf("turns: %+v then %+v", first.Data.Inputs, second.Data.Inputs)
	}
}

func TestEventsOfThreadsNobodyTalksInAreIgnored(t *testing.T) {
	h := newHarness(t, Options{})
	close(h.release)
	for _, thread := range []string{"", "cli", "unknown-thread"} {
		ref, err := bus.Request(context.Background(), h.client, events.StartTask, events.TaskSpec{Kind: "demo.sleep", ThreadID: thread})
		if err != nil {
			t.Fatal(err)
		}
		h.waitStatus(ref.TaskID, events.StatusDone)
	}
	h.noTurn(300 * time.Millisecond)
}

func TestProgressAndRunningAreNotAnnounced(t *testing.T) {
	h := newHarness(t, Options{})
	h.say("th", "terminal", "hola")
	h.nextTurn()
	ref, err := bus.Request(context.Background(), h.client, events.StartTask, events.TaskSpec{Kind: "demo.sleep", ThreadID: "th"})
	if err != nil {
		t.Fatal(err)
	}
	h.waitStatus(ref.TaskID, events.StatusRunning)
	bus.Publish(context.Background(), h.client, events.TaskProgress, events.TaskProgressData{TaskID: ref.TaskID, Message: "1/5"})
	h.noTurn(300 * time.Millisecond)
	close(h.release)
	if turn := h.nextTurn(); turn.Data.Inputs[0].Task.Type != "task.done" {
		t.Errorf("turn = %+v", turn.Data.Inputs)
	}
}

// The LLM is told which questions only the user may answer.
func TestSensitiveQuestionsAreTheUsers(t *testing.T) {
	h := newHarness(t, Options{})
	h.say("th", "terminal", "hola")
	h.nextTurn()
	for i, spec := range []events.TaskSpec{{Kind: "demo.ask", ThreadID: "th", Sensitive: true}, {Kind: "demo.ask", ThreadID: "th"}} {
		if _, err := bus.Request(context.Background(), h.client, events.StartTask, spec); err != nil {
			t.Fatal(err)
		}
		h.nextTurn()
		in := h.fake.requests()[i+1].Input
		if got := strings.Contains(inputOf(in), "Only the user can answer it"); got != spec.Sensitive {
			t.Errorf("sensitive %v: told only the user may answer = %v:\n%s", spec.Sensitive, got, inputOf(in))
		}
	}
}

func TestQuestionsAndFailuresAreAnnounced(t *testing.T) {
	h := newHarness(t, Options{})
	h.say("th", "terminal", "hola")
	h.nextTurn()
	ask, _ := bus.Request(context.Background(), h.client, events.StartTask, events.TaskSpec{Kind: "demo.ask", ThreadID: "th"})
	turn := h.nextTurn()
	if task := turn.Data.Inputs[0].Task; task == nil || task.Type != "task.needs_input" || task.Question != "¿Qué perfil?" || len(task.Options) != 2 {
		t.Fatalf("question turn = %+v", turn.Data.Inputs)
	}
	mustContain(t, h.fake.requests()[1].Input, "needs an answer: <data>¿Qué perfil?</data> Options: <data>Ana | Luis</data>")

	// A failure (here the task kind has no worker) is told too.
	if _, err := bus.Request(context.Background(), h.client, events.StartTask, events.TaskSpec{Kind: "nobody.serves", ThreadID: "th"}); err == nil {
		t.Fatal("started a task nobody serves")
	}
	turn = h.nextTurn()
	if task := turn.Data.Inputs[0].Task; task == nil || task.Type != "task.failed" || !strings.Contains(task.Reason, "no worker") {
		t.Fatalf("failure turn = %+v", turn.Data.Inputs)
	}
	bus.Request(context.Background(), h.client, events.CancelTask, events.TaskCancelRequest{TaskID: ask.TaskID, Reason: "ya no"})
	turn = h.nextTurn()
	if task := turn.Data.Inputs[0].Task; task == nil || task.Type != "task.cancelled" || task.Reason != "ya no" {
		t.Errorf("cancel turn = %+v", turn.Data.Inputs)
	}
}

// When the LLM cancels a task itself, the cancellation is not told again.
func TestCancellationByTheLLMIsNotAnnounced(t *testing.T) {
	h := newHarness(t, Options{})
	var id string
	h.fake.set(func(f *fake) {
		f.reply = func(ctx context.Context, req llm.Request) []llm.Output {
			switch in := inputOf(req.Input); {
			case strings.Contains(in, "empieza"):
				var tool llm.Output
				id, tool = h.startTask(ctx, req.Turn.ThreadID, "demo.sleep", "Larga")
				return []llm.Output{tool, {Text: "Empezada."}}
			case strings.Contains(in, "cancela"):
				bus.Request(ctx, h.client, events.CancelTask, events.TaskCancelRequest{TaskID: id})
				in, _ := json.Marshal(map[string]string{"task_id": id})
				return []llm.Output{{Tool: &llm.ToolCall{Name: "cancel_task", Input: in}}, {Text: "Cancelada."}}
			}
			return []llm.Output{{Text: "otra cosa"}}
		}
	})
	h.say("th", "terminal", "empieza")
	h.nextTurn()
	h.say("th", "terminal", "cancela")
	h.nextTurn()
	h.waitStatus(id, events.StatusCancelled)
	h.noTurn(300 * time.Millisecond)

	// A cancellation by someone else is told.
	other, _ := bus.Request(context.Background(), h.client, events.StartTask, events.TaskSpec{Kind: "demo.sleep", ThreadID: "th"})
	bus.Request(context.Background(), h.client, events.CancelTask, events.TaskCancelRequest{TaskID: other.TaskID})
	if turn := h.nextTurn(); turn.Data.Inputs[0].Task == nil || turn.Data.Inputs[0].Task.TaskID != other.TaskID {
		t.Errorf("turn = %+v", turn.Data.Inputs)
	}
}

// A task the user cancelled directly on the conversation's channel, e.g.
// from its chat, is not told. One cancelled from another channel is, with
// the channel.
func TestCancellationByTheUserIsNotAnnounced(t *testing.T) {
	h := newHarness(t, Options{})
	h.say("th", "terminal", "hola")
	h.nextTurn()
	ctx := context.Background()
	start := func() string {
		ref, _ := bus.Request(ctx, h.client, events.StartTask, events.TaskSpec{Kind: "demo.sleep", ThreadID: "th"})
		h.waitStatus(ref.TaskID, events.StatusRunning)
		return ref.TaskID
	}
	fromHere, _ := events.CancelTaskOn("terminal")
	if _, err := bus.Request(ctx, h.client, fromHere, events.TaskCancelRequest{TaskID: start()}); err != nil {
		t.Fatal(err)
	}
	h.noTurn(300 * time.Millisecond)

	fromKitchen, _ := events.CancelTaskOn("voice:kitchen")
	kitchen := start()
	bus.Request(ctx, h.client, fromKitchen, events.TaskCancelRequest{TaskID: kitchen})
	turn := h.nextTurn()
	if len(turn.Data.Inputs) != 1 || turn.Data.Inputs[0].Task == nil || turn.Data.Inputs[0].Task.Channel != "voice:kitchen" {
		t.Errorf("turn = %+v", turn.Data.Inputs)
	}
	mustContain(t, inputOf(h.fake.requests()[1].Input), "was cancelled by someone on voice:kitchen")

	// DOGGIE's own cancellations are told too.
	other := start()
	bus.Request(ctx, h.client, events.CancelTask, events.TaskCancelRequest{TaskID: other})
	if turn := h.nextTurn(); len(turn.Data.Inputs) != 1 || turn.Data.Inputs[0].Task == nil || turn.Data.Inputs[0].Task.TaskID != other {
		t.Errorf("turn = %+v", turn.Data.Inputs)
	}
}

// A task ends once: a second end in the log (e.g. done racing a timeout) and
// a question after the end are not told.
func TestSecondEndsAndStaleQuestionsAreIgnored(t *testing.T) {
	h := newHarness(t, Options{})
	h.say("th", "terminal", "hola")
	h.nextTurn()
	ctx := context.Background()
	info := events.TaskInfo{ID: "t900", TaskSpec: events.TaskSpec{Kind: "demo.sleep", ThreadID: "th"}, Status: events.StatusPending}
	bus.Publish(ctx, h.client, events.TaskCreated, info)
	bus.Publish(ctx, h.client, events.TaskDone, events.TaskDoneData{TaskID: "t900"})
	bus.Publish(ctx, h.client, events.TaskFailed, events.TaskFailedData{TaskID: "t900", Error: "timed out"})
	bus.Publish(ctx, h.client, events.TaskNeedsInput, events.TaskNeedsInputData{TaskID: "t900", Question: "¿tarde?"})
	turn := h.nextTurn()
	if len(turn.Data.Inputs) != 1 || turn.Data.Inputs[0].Task.Type != "task.done" {
		t.Errorf("inputs = %+v", turn.Data.Inputs)
	}
	h.noTurn(300 * time.Millisecond)
}

// Events of a task the brain didn't see being created (e.g. it restarted)
// are looked up in the task manager, in order.
func TestUnknownTasksAreLookedUpInOrder(t *testing.T) {
	h := newHarness(t, Options{})
	h.say("th", "terminal", "hola")
	h.nextTurn()
	h.stop()
	// Created while the brain is down.
	ask, _ := bus.Request(context.Background(), h.client, events.StartTask, events.TaskSpec{Kind: "demo.ask", ThreadID: "th"})
	h.waitStatus(ask.TaskID, events.StatusWaitingInput)
	h.startBrain(Options{})
	// The question asked while the brain was down is caught up from the log.
	if turn := h.nextTurn(); turn.Data.Inputs[0].Task == nil || turn.Data.Inputs[0].Task.Type != "task.needs_input" {
		t.Fatalf("caught-up turn = %+v", turn.Data.Inputs)
	}
	bus.Request(context.Background(), h.client, events.AnswerTask, events.TaskAnswer{TaskID: ask.TaskID, Answer: "Luis"})
	turn := h.nextTurn()
	if task := turn.Data.Inputs[0].Task; task == nil || task.Type != "task.done" || task.Kind != "demo.ask" || string(task.Result) != `{"answer":"Luis"}` {
		t.Errorf("inputs = %+v", turn.Data.Inputs)
	}

	// Two events of the same unknown task keep their order through the lookup.
	ctx := context.Background()
	bus.Publish(ctx, h.client, events.TaskNeedsInput, events.TaskNeedsInputData{TaskID: ask.TaskID + "x", Question: "q"})
	h.noTurn(200 * time.Millisecond) // unknown to the manager: dropped
}

func TestTurnsRunInParallelUpToTheLimit(t *testing.T) {
	h := newHarness(t, Options{MaxTurns: 2})
	gate := make(chan struct{})
	h.fake.set(func(f *fake) { f.gate = gate })
	for i := range 5 {
		h.say(fmt.Sprintf("th%d", i), "terminal", "hola")
	}
	waitFor(t, "two turns", func() bool { return len(h.fake.requests()) == 2 })
	time.Sleep(100 * time.Millisecond)
	if n := len(h.fake.requests()); n != 2 {
		t.Errorf("%d turns started with a limit of 2", n)
	}
	close(gate)
	threads := map[string]bool{}
	for range 5 {
		threads[h.nextTurn().Data.ThreadID] = true
	}
	if len(threads) != 5 {
		t.Errorf("answered threads: %v", threads)
	}
	h.fake.mu.Lock()
	defer h.fake.mu.Unlock()
	if h.fake.maxRunning > 2 {
		t.Errorf("%d turns ran at once", h.fake.maxRunning)
	}
}

func TestProviderFailures(t *testing.T) {
	h := newHarness(t, Options{})
	h.fake.set(func(f *fake) { f.fail = errBoom })
	h.say("th", "terminal", "uno")
	turn := h.nextTurn()
	if turn.Data.Error != "boom" || len(turn.Data.Replies) != 0 {
		t.Errorf("turn = %+v", turn.Data)
	}
	// A turn that fails after saying something keeps what it said.
	h.fake.set(func(f *fake) {
		f.fail = nil
		f.reply = func(context.Context, llm.Request) []llm.Output {
			return []llm.Output{{Text: "Voy"}, {Done: &llm.Done{Err: llm.ErrTurnTimeout}}}
		}
	})
	h.say("th", "terminal", "dos")
	if s := h.nextSpeak(); s.Data.Text != "Voy" {
		t.Errorf("said %q", s.Data.Text)
	}
	turn = h.nextTurn()
	if turn.Data.Error != llm.ErrTurnTimeout.Error() || len(turn.Data.Replies) != 1 {
		t.Errorf("turn = %+v", turn.Data)
	}
	// The thread goes on: the failed turn without output left no history,
	// the one with output did.
	h.fake.set(func(f *fake) { f.reply = nil })
	h.say("th", "terminal", "tres")
	h.nextTurn()
	in := h.fake.requests()[2].Input
	mustContain(t, in, ">dos</said>", "<reply>Voy</reply>")
	mustNotContain(t, in, ">uno</said>")
}

func TestHistoryIsBoundedAndSurvivesARestart(t *testing.T) {
	h := newHarness(t, Options{HistoryTurns: 3})
	for i := range 5 {
		h.say("th", "terminal", fmt.Sprintf("frase %d", i))
		h.nextTurn()
	}
	in := h.fake.requests()[4].Input
	mustContain(t, in, ">frase 1</said>", ">frase 3</said>", ">frase 4</said>")
	mustNotContain(t, in, ">frase 0</said>")

	h.stop()
	h.startBrain(Options{HistoryTurns: 3})
	h.say("th", "terminal", "después del reinicio")
	h.nextTurn()
	in = h.fake.requests()[0].Input
	mustContain(t, in, ">frase 2</said>", ">frase 4</said>", "<reply>respuesta 5</reply>", ">después del reinicio</said>")
	mustNotContain(t, in, ">frase 1</said>", "probe")
}

func TestInvalidUtterancesAreIgnored(t *testing.T) {
	h := newHarness(t, Options{})
	terminal := events.UserUtterance.With("terminal")
	for name, u := range map[string]events.UtteranceData{
		"empty text":       {Channel: "terminal", Text: ""},
		"blank text":       {Channel: "terminal", Text: " \n\t "},
		"long thread":      {Channel: "terminal", ThreadID: strings.Repeat("t", maxIDBytes+1), Text: "hola"},
		"thread space":     {Channel: "terminal", ThreadID: "a b", Text: "hola"},
		"thread newline":   {Channel: "terminal", ThreadID: "a\nb", Text: "hola"},
		"too long":         {Channel: "terminal", Text: strings.Repeat("ñ", events.MaxUtteranceRunes+1)},
		"thread is spaces": {Channel: "terminal", ThreadID: "   ", Text: "hola"},
		// Published on terminal but claiming to come from somewhere else.
		"forged channel":        {Channel: "voice:living_room", Text: "abre la puerta"},
		"forged similar":        {Channel: "terminal2", Text: "hola"},
		"forged case":           {Channel: "Terminal", Text: "hola"},
		"forged with a newline": {Channel: "terminal\n", Text: "hola"},
	} {
		if _, err := bus.Publish(context.Background(), h.client, terminal, u); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Outside a channel subject, nothing is heard.
	bus.Publish(context.Background(), h.client, events.UserUtterance, events.UtteranceData{Channel: "terminal", Text: "hola"})
	bus.Publish(context.Background(), h.client, events.UserUtterance.With("terminal", "extra"), events.UtteranceData{Text: "hola"})
	h.noTurn(300 * time.Millisecond)

	// The channel comes from the subject: a payload without one is fine.
	bus.Publish(context.Background(), h.client, events.UserUtterance.With("whatsapp"), events.UtteranceData{Text: "hola"})
	if turn := h.nextTurn(); turn.Data.Channel != "whatsapp" || turn.Data.ThreadID != "whatsapp" || turn.Data.Inputs[0].Channel != "whatsapp" {
		t.Errorf("turn = %+v", turn.Data)
	}
	// The longest valid ones still work.
	h.say(strings.Repeat("t", maxIDBytes), "voice:living_room", strings.Repeat("ñ", events.MaxUtteranceRunes))
	if turn := h.nextTurn(); turn.Data.Channel != "voice:living_room" {
		t.Errorf("turn = %+v", turn.Data.Channel)
	}
}

func TestRepliesGoToTheChannelLastUsed(t *testing.T) {
	h := newHarness(t, Options{})
	h.say("owner", "voice:living_room", "hola")
	if s := h.nextSpeak(); s.Data.Channel != "voice:living_room" {
		t.Errorf("channel = %s", s.Data.Channel)
	}
	h.nextTurn()
	h.say("owner", "whatsapp", "sigo por aquí")
	if s := h.nextSpeak(); s.Data.Channel != "whatsapp" || s.Data.ThreadID != "owner" {
		t.Errorf("speak = %+v", s.Data)
	}
	h.nextTurn()
	// A task event goes to where the user is now.
	ref, _ := bus.Request(context.Background(), h.client, events.StartTask, events.TaskSpec{Kind: "demo.sleep", ThreadID: "owner"})
	h.waitStatus(ref.TaskID, events.StatusRunning)
	close(h.release)
	if s := h.nextSpeak(); s.Data.Channel != "whatsapp" {
		t.Errorf("event reply on %s", s.Data.Channel)
	}
}

func TestPromptListsTheActiveTasks(t *testing.T) {
	h := newHarness(t, Options{})
	mine, _ := bus.Request(context.Background(), h.client, events.StartTask, events.TaskSpec{Kind: "demo.sleep", Description: "Mía", ThreadID: "th"})
	theirs, _ := bus.Request(context.Background(), h.client, events.StartTask, events.TaskSpec{Kind: "demo.sleep", Description: "Ajena", ThreadID: "otro"})
	h.waitStatus(theirs.TaskID, events.StatusRunning)
	h.say("th", "terminal", "¿cómo va?")
	h.nextTurn()
	in := h.fake.requests()[0].Input
	mustContain(t, in, "- "+mine.TaskID+" demo.sleep <data>Mía</data>: running, asked in this conversation", "- "+theirs.TaskID+" demo.sleep <data>Ajena</data>: running, asked in another conversation\n")
	close(h.release)
}

func TestQueueAndThreadsAreBounded(t *testing.T) {
	h := newHarness(t, Options{})
	gate := make(chan struct{})
	h.fake.set(func(f *fake) { f.gate = gate })
	h.say("th", "terminal", "primera")
	waitFor(t, "the first turn", func() bool { return len(h.fake.requests()) == 1 })
	for i := range maxPending + 10 {
		h.say("th", "terminal", fmt.Sprintf("cola %d", i))
	}
	for i := range maxThreads + 20 {
		h.say(fmt.Sprintf("hilo%d", i), "terminal", "hola")
	}
	time.Sleep(300 * time.Millisecond) // let the brain take them all in
	close(gate)
	// Find th's second turn: the one with its queue.
	var queued events.BrainTurnData
	for len(queued.Inputs) < 2 {
		if ev := h.nextTurn(); ev.Data.ThreadID == "th" {
			queued = ev.Data
		}
	}
	if n := len(queued.Inputs); n != maxPending || queued.Inputs[n-1].Said != fmt.Sprintf("cola %d", maxPending+9) {
		t.Errorf("queued turn has %d inputs ending with %q, want the newest %d", n, queued.Inputs[n-1].Said, maxPending)
	}
	h.stop()
	if n := len(h.brain.threads); n > maxThreads {
		t.Errorf("%d threads kept, max %d", n, maxThreads)
	}
}

func TestStopsPromptlyMidTurn(t *testing.T) {
	h := newHarness(t, Options{})
	h.fake.set(func(f *fake) { f.gate = make(chan struct{}) })
	h.say("th", "terminal", "hola")
	waitFor(t, "the turn", func() bool { return len(h.fake.requests()) == 1 })
	start := time.Now()
	if err := h.stop(); err != context.Canceled {
		t.Errorf("Run returned %v", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("stopping took %s", took)
	}
	// The interrupted turn is still recorded.
	if turn := h.nextTurn(); turn.Data.Error != context.Canceled.Error() {
		t.Errorf("turn = %+v", turn.Data)
	}
}

func TestRunFailsWithoutAProvider(t *testing.T) {
	h := newHarness(t, Options{})
	h.stop()
	b := New(connect(t, h.srv, "brain-x"), discardLog(), Options{NewProvider: func(*mcp.Server) (llm.Brain, func(), error) { return nil, nil, errBoom }})
	if err := b.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Run = %v", err)
	}
	if err := New(connect(t, h.srv, "brain-y"), discardLog(), Options{}).Run(context.Background()); err == nil {
		t.Error("Run without a provider succeeded")
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// After a restart the brain doesn't remember which tasks ended: a late
// question for a task the manager says is over is still not asked.
func TestLateQuestionAfterARestartIsIgnored(t *testing.T) {
	h := newHarness(t, Options{})
	h.say("th", "terminal", "hola")
	h.nextTurn()
	h.stop()
	ref, _ := bus.Request(context.Background(), h.client, events.StartTask, events.TaskSpec{Kind: "demo.sleep", ThreadID: "th"})
	h.waitStatus(ref.TaskID, events.StatusRunning)
	h.release <- struct{}{}
	h.waitStatus(ref.TaskID, events.StatusDone)
	noCatchUp(t) // the task ended long ago: the brain must look it up
	h.startBrain(Options{})
	bus.Publish(context.Background(), h.client, events.TaskNeedsInput, events.TaskNeedsInputData{TaskID: ref.TaskID, Question: "¿tarde?"})
	h.noTurn(400 * time.Millisecond)
}

// noCatchUp makes the brain ignore past task events on start, as if they
// were older than the catch-up window.
func noCatchUp(t *testing.T) {
	old := catchUpWindow
	catchUpWindow = 0
	t.Cleanup(func() { catchUpWindow = old })
}

// Events of a task the brain must look up all arrive, in order, even when
// they come back to back.
func TestLookedUpEventsKeepTheirOrder(t *testing.T) {
	h := newHarness(t, Options{})
	h.say("th", "terminal", "hola")
	h.nextTurn()
	gate := make(chan struct{})
	h.fake.set(func(f *fake) { f.gate = gate })
	h.say("th", "terminal", "ocupado") // hold the thread so the events queue up
	h.waitRequest(">ocupado</said>")
	h.stop()
	ask, _ := bus.Request(context.Background(), h.client, events.StartTask, events.TaskSpec{Kind: "demo.ask", ThreadID: "th"})
	h.waitStatus(ask.TaskID, events.StatusWaitingInput)
	noCatchUp(t) // so the task is unknown and must be looked up
	h.fake.set(func(f *fake) { f.gate = nil })
	h.startBrain(Options{})
	h.fake.set(func(f *fake) { f.gate = gate })
	h.say("th", "terminal", "ocupado otra vez")
	waitFor(t, "the busy turn", func() bool { return len(h.fake.requests()) == 1 })
	ctx := context.Background()
	bus.Publish(ctx, h.client, events.TaskNeedsInput, events.TaskNeedsInputData{TaskID: ask.TaskID, Question: "primera"})
	bus.Publish(ctx, h.client, events.TaskNeedsInput, events.TaskNeedsInputData{TaskID: ask.TaskID, Question: "segunda"})
	time.Sleep(300 * time.Millisecond)
	close(gate)
	var questions []string
	for len(questions) < 2 {
		for _, in := range h.nextTurn().Data.Inputs {
			if in.Task != nil {
				questions = append(questions, in.Task.Question)
			}
		}
	}
	if questions[0] != "primera" || questions[1] != "segunda" {
		t.Errorf("questions = %q", questions)
	}
}

// The result of a sensitive task never reaches the LLM or the turn record,
// even if an event carried it.
func TestSensitiveResultsStayHidden(t *testing.T) {
	h := newHarness(t, Options{})
	h.say("th", "terminal", "hola")
	h.nextTurn()
	ctx := context.Background()
	bus.Publish(ctx, h.client, events.TaskCreated, events.TaskInfo{ID: "t777", TaskSpec: events.TaskSpec{Kind: "demo.sleep", ThreadID: "th", Sensitive: true}})
	bus.Publish(ctx, h.client, events.TaskDone, events.TaskDoneData{TaskID: "t777", Result: json.RawMessage(`{"pin":"4821"}`)})
	turn := h.nextTurn()
	b, _ := json.Marshal(turn.Data)
	mustNotContain(t, string(b), "4821")
	mustNotContain(t, h.fake.requests()[1].Input, "4821")
	mustContain(t, h.fake.requests()[1].Input, "hidden: it is a sensitive task")
}

// What happened to a thread's tasks while the brain was down is told when it
// comes back, and what was already told is not told again.
func TestCatchesUpOnTaskEventsAfterARestart(t *testing.T) {
	h := newHarness(t, Options{})
	h.say("th", "terminal", "hola")
	h.nextTurn()
	told, _ := bus.Request(context.Background(), h.client, events.StartTask, events.TaskSpec{Kind: "demo.sleep", ThreadID: "th"})
	h.waitStatus(told.TaskID, events.StatusRunning)
	h.release <- struct{}{}
	if turn := h.nextTurn(); turn.Data.Inputs[0].Task.TaskID != told.TaskID {
		t.Fatalf("turn = %+v", turn.Data.Inputs)
	}

	h.stop()
	missed, _ := bus.Request(context.Background(), h.client, events.StartTask, events.TaskSpec{Kind: "demo.sleep", ThreadID: "th"})
	bus.Request(context.Background(), h.client, events.CancelTask, events.TaskCancelRequest{TaskID: missed.TaskID, Reason: "interrupted: DOGGIE restarted"})
	h.waitStatus(missed.TaskID, events.StatusCancelled)
	h.startBrain(Options{})

	turn := h.nextTurn()
	if len(turn.Data.Inputs) != 1 || turn.Data.Inputs[0].Task == nil || turn.Data.Inputs[0].Task.TaskID != missed.TaskID {
		t.Fatalf("caught-up turn = %+v, want only the missed task", turn.Data.Inputs)
	}
	h.noTurn(300 * time.Millisecond)

	// A second restart has nothing new to tell.
	h.stop()
	h.startBrain(Options{})
	h.noTurn(300 * time.Millisecond)
}

// A question already asked is not asked again after a restart, and a second
// end that comes after the restart is not told either.
func TestRestartDoesNotRepeatQuestionsOrEnds(t *testing.T) {
	h := newHarness(t, Options{})
	h.say("th", "terminal", "hola")
	h.nextTurn()
	ask, _ := bus.Request(context.Background(), h.client, events.StartTask, events.TaskSpec{Kind: "demo.ask", ThreadID: "th"})
	if turn := h.nextTurn(); turn.Data.Inputs[0].Task == nil || turn.Data.Inputs[0].Task.Type != "task.needs_input" {
		t.Fatalf("question turn = %+v", turn.Data.Inputs)
	}
	done, _ := bus.Request(context.Background(), h.client, events.StartTask, events.TaskSpec{Kind: "demo.sleep", ThreadID: "th"})
	h.waitStatus(done.TaskID, events.StatusRunning)
	h.release <- struct{}{}
	h.nextTurn()

	h.stop()
	h.startBrain(Options{})
	h.noTurn(300 * time.Millisecond) // the question was asked already
	bus.Publish(context.Background(), h.client, events.TaskFailed, events.TaskFailedData{TaskID: done.TaskID, Error: "timed out"})
	h.noTurn(300 * time.Millisecond) // the task already ended
	_ = ask
}

// The catch-up runs on its own: the notice doesn't wait for the next event.
func TestCaughtUpEventsAreToldWithoutWaitingForInput(t *testing.T) {
	h := newHarness(t, Options{})
	h.say("th", "terminal", "hola")
	h.nextTurn()
	h.stop()
	ref, _ := bus.Request(context.Background(), h.client, events.StartTask, events.TaskSpec{Kind: "demo.sleep", ThreadID: "th"})
	h.waitStatus(ref.TaskID, events.StatusRunning)
	bus.Request(context.Background(), h.client, events.CancelTask, events.TaskCancelRequest{TaskID: ref.TaskID, Reason: "interrupted: DOGGIE restarted"})
	h.waitStatus(ref.TaskID, events.StatusCancelled)

	// Start the brain without probing it: nothing else will happen.
	b := New(connect(t, h.srv, "brain-quiet"), discardLog(), Options{HistoryTurns: 20,
		NewProvider: func(*mcp.Server) (llm.Brain, func(), error) { return h.fake, func() {}, nil }})
	goRun(t, b.Run)
	turn := h.nextTurn()
	if task := turn.Data.Inputs[0].Task; task == nil || task.TaskID != ref.TaskID {
		t.Errorf("turn = %+v", turn.Data.Inputs)
	}
}

// DOGGIE never goes silent on someone because it restarted: what was said
// while the brain was down, or during a turn that died without a word, is
// answered when it comes back, marked as resumed.
func TestUnansweredSentencesAreResumedAfterARestart(t *testing.T) {
	h := newHarness(t, Options{})
	h.say("th", "terminal", "hola")
	h.nextTurn()

	// A turn stopped mid-way, before it said or did anything.
	h.fake.set(func(f *fake) { f.gate = make(chan struct{}) })
	h.say("th", "terminal", "¿qué hora es?")
	h.waitRequest(">¿qué hora es?</said>")
	h.stop()
	if turn := h.nextTurn(); turn.Data.Error == "" {
		t.Fatalf("interrupted turn = %+v", turn.Data)
	}
	// Something said while the brain is down.
	h.say("th", "terminal", "¿sigues ahí?")

	h.fake.set(func(f *fake) { f.gate = nil })
	h.startBrain(Options{})
	turn := h.nextTurn()
	var said []string
	for _, in := range turn.Data.Inputs {
		if !in.Resumed {
			t.Errorf("input %q not marked resumed", in.Said)
		}
		said = append(said, in.Said)
	}
	if strings.Join(said, "|") != "¿qué hora es?|¿sigues ahí?" {
		t.Errorf("resumed = %q", said)
	}
	mustContain(t, h.fake.requests()[0].Input, `resumed="yes">¿qué hora es?</said>`, `resumed="yes">¿sigues ahí?</said>`)
	// The tools guard the resumed turn from the oldest resumed sentence on.
	if got := h.fake.requests()[0].Turn.Resumed; !got.Equal(turn.Data.Inputs[0].At) {
		t.Errorf("Turn.Resumed = %s, want %s", got, turn.Data.Inputs[0].At)
	}

	// Once answered, a further restart resumes nothing.
	h.stop()
	h.startBrain(Options{})
	h.noTurn(300 * time.Millisecond)
}

// A sentence a turn acted on is not resumed, even if the turn then failed:
// that could repeat the action.
func TestSentencesATurnActedOnAreNotResumed(t *testing.T) {
	h := newHarness(t, Options{})
	h.fake.set(func(f *fake) {
		f.reply = func(ctx context.Context, req llm.Request) []llm.Output {
			_, tool := h.startTask(ctx, req.Turn.ThreadID, "demo.sleep", "Algo")
			return []llm.Output{tool, {Done: &llm.Done{Err: llm.ErrTurnTimeout}}}
		}
	})
	h.say("th", "terminal", "haz algo")
	if turn := h.nextTurn(); turn.Data.Error == "" || len(turn.Data.Actions) != 1 {
		t.Fatalf("turn = %+v", turn.Data)
	}
	h.fake.set(func(f *fake) { f.reply = nil })
	h.stop()
	h.startBrain(Options{})
	h.noTurn(300 * time.Millisecond)
}

// A sentence said while the brain gets ready, e.g. by a chat that has just
// started DOGGIE, is new: no earlier brain could have acted on it.
func TestSentencesSaidWhileStartingAreNotResumed(t *testing.T) {
	h := newHarness(t, Options{})
	h.stop()
	h.say("th", "terminal", "de antes")
	var said string
	h.startBrain(Options{NewProvider: func(*mcp.Server) (llm.Brain, func(), error) {
		said = h.say("th", "terminal", "recién dicho") // before the brain subscribes
		return h.fake, func() {}, nil
	}})
	resumed := map[string]bool{}
	for len(resumed) < 2 {
		for _, in := range h.nextTurn().Data.Inputs {
			resumed[in.Said] = in.Resumed
			if in.Said == "recién dicho" && in.EventID != said {
				t.Errorf("answered %s, want %s", in.EventID, said)
			}
		}
	}
	if !resumed["de antes"] || resumed["recién dicho"] {
		t.Errorf("resumed = %v, want only the sentence said while the brain was down", resumed)
	}
}

// Sentences older than the catch-up window are not resumed.
func TestOldSentencesAreNotResumed(t *testing.T) {
	h := newHarness(t, Options{})
	h.stop()
	h.say("th", "terminal", "hace mucho")
	noCatchUp(t)
	h.startBrain(Options{})
	h.noTurn(300 * time.Millisecond)
}

// A sentence that keeps failing is resumed once, not on every restart.
func TestFailingSentenceIsResumedOnlyOnce(t *testing.T) {
	h := newHarness(t, Options{})
	h.say("th", "terminal", "hola")
	h.nextTurn()
	h.fake.set(func(f *fake) { f.fail = errBoom })
	h.say("th", "terminal", "frase venenosa")
	if turn := h.nextTurn(); turn.Data.Error != "boom" {
		t.Fatalf("turn = %+v", turn.Data)
	}
	h.stop()
	h.startBrain(Options{}) // the probe works: fail only hits Generate
	if turn := h.nextTurn(); !turn.Data.Inputs[0].Resumed || turn.Data.Error != "boom" {
		t.Fatalf("resumed turn = %+v", turn.Data)
	}
	h.stop()
	h.startBrain(Options{})
	h.noTurn(300 * time.Millisecond)
}

// Text is streamed as signals while it is written; each out.speak then ends
// its block, numbered within the turn.
func TestRepliesAreStreamed(t *testing.T) {
	h := newHarness(t, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deltas, err := bus.SubscribeSignal(ctx, h.client, events.OutSpeakDelta)
	if err != nil {
		t.Fatal(err)
	}
	var streamed bool
	h.fake.set(func(f *fake) {
		f.reply = func(_ context.Context, req llm.Request) []llm.Output {
			streamed = req.Stream
			return []llm.Output{{Delta: "Ya "}, {Delta: "voy."}, {Text: "Ya voy."}, {Tool: &llm.ToolCall{Name: "list_tasks"}}, {Delta: "Listo"}, {Text: "Listo"}}
		}
	})
	id := h.say("th", "terminal", "hola")
	var got []string
	for range 3 {
		d := receiveSignal(t, deltas)
		if d.ThreadID != "th" || d.Channel != "terminal" || d.CausedBy != id {
			t.Errorf("delta = %+v", d)
		}
		got = append(got, fmt.Sprintf("%d:%s", d.Block, d.Text))
	}
	if strings.Join(got, "|") != "0:Ya |0:voy.|1:Listo" {
		t.Errorf("deltas = %q", got)
	}
	for i, want := range []string{"Ya voy.", "Listo"} {
		if s := h.nextSpeak(); s.Data.Text != want || s.Data.Block != i {
			t.Errorf("out.speak = %+v, want %q block %d", s.Data, want, i)
		}
	}
	h.nextTurn()
	if !streamed {
		t.Error("the brain did not ask for streaming")
	}
}

func receiveSignal[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("no signal")
	}
	var zero T
	return zero
}
