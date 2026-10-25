package manager

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/task/worker"
)

func TestSensitiveTaskStaysOutOfTheLog(t *testing.T) {
	h := newHarness(t, testOptions(), map[string]worker.Func{"test.echo": echo})
	id := h.mustStart(events.TaskSpec{
		Kind: "test.echo", Sensitive: true, Description: "send a message",
		Input: json.RawMessage(`{"secret":"código-4821"}`),
	})
	info := h.waitStatus(id, events.StatusDone)
	if string(info.Result) != `{"secret":"código-4821"}` {
		t.Errorf("requester did not get the result: %s", info.Result)
	}

	logged, err := bus.History(context.Background(), h.client, time.Time{}, "task.>")
	if err != nil {
		t.Fatal(err)
	}
	for _, env := range logged {
		if strings.Contains(string(env.Data), "4821") {
			t.Errorf("sensitive data in the event log: %s %s", env.Type, env.Data)
		}
	}
	if !strings.Contains(string(logged[len(logged)-1].Data), `"redacted":true`) {
		t.Errorf("last logged event %s is not a redacted task.done", logged[len(logged)-1].Data)
	}

	// After a restart nothing sensitive survives: it only lived in memory.
	h.stopManager()
	h.startManager()
	if info := h.get(id); info.Status != events.StatusDone || len(info.Input) != 0 || len(info.Result) != 0 {
		t.Errorf("after restart: %+v, want done with no input or result", info)
	}
}

func TestReportResultIsOnlyForSensitiveTasks(t *testing.T) {
	b := newBlocker()
	h := newHarness(t, testOptions(), map[string]worker.Func{"test.block": b.run})
	id := h.mustStart(events.TaskSpec{Kind: "test.block"})
	recv(t, b.started, "task start")
	_, err := bus.Request(context.Background(), h.client, events.ReportResult, events.TaskDoneData{TaskID: id, Result: json.RawMessage(`1`)})
	if err == nil || !strings.Contains(err.Error(), "not sensitive") {
		t.Errorf("err = %v, want not sensitive", err)
	}
}
