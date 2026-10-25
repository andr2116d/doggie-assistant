package prompt

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

// realistic is a busy thread: 20 turns and 20 active tasks.
func realistic() Context {
	c := Context{Now: at(60), Location: lima, ThreadID: "terminal", MaxTurns: 20, Inputs: []Input{said(60, "¿cómo va lo de Netflix?")}}
	for i := range 20 {
		c.History = append(c.History, Turn{
			At:      at(i),
			Inputs:  []Input{said(i, fmt.Sprintf("Pregunta número %d sobre la casa, el clima y los planes del día.", i))},
			Actions: []Action{{Tool: "start_task", Input: json.RawMessage(`{"kind":"demo.sleep","description":"Abrir Netflix"}`)}},
			Replies: []string{fmt.Sprintf("Respuesta breve número %d, confirmando la acción pedida.", i)},
		})
		c.Tasks = append(c.Tasks, events.TaskInfo{ID: fmt.Sprintf("t%d", i), TaskSpec: events.TaskSpec{Kind: "demo.sleep", Description: "Abrir Netflix", ThreadID: "terminal"},
			Status: events.StatusRunning, Percent: 40, Progress: "2/5 s", CreatedAt: at(i)})
	}
	return c
}

func BenchmarkBuild(b *testing.B) {
	c := realistic()
	b.ReportAllocs()
	var n int
	for b.Loop() {
		n = len(Build(c))
	}
	b.ReportMetric(float64(n), "bytes/msg")
}
