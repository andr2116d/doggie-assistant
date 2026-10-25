package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
)

func TestLocalBusURL(t *testing.T) {
	tests := []struct {
		listen, want string
		wantErr      bool
	}{
		{listen: "127.0.0.1:4222", want: "nats://127.0.0.1:4222"},
		{listen: "0.0.0.0:4222", want: "nats://127.0.0.1:4222"},
		{listen: ":5000", want: "nats://127.0.0.1:5000"},
		{listen: "192.168.1.20:4222", want: "nats://192.168.1.20:4222"},
		{listen: "", wantErr: true},
	}
	for _, tt := range tests {
		got, err := localBusURL(tt.listen)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("localBusURL(%q) = %q, %v; want %q (err %v)", tt.listen, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestPrintEnvelope(t *testing.T) {
	var buf bytes.Buffer
	printEnvelope(&buf, bus.Envelope{
		Meta: bus.Meta{Type: "task.done", Source: "task:7", CausedBy: "01ABC", Time: time.Now()},
		Data: json.RawMessage(`{"ok":true}`),
	})
	line := buf.String()
	for _, want := range []string{"task.done", "src=task:7", "cause=01ABC", `{"ok":true}`} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q missing %q", line, want)
		}
	}
}
