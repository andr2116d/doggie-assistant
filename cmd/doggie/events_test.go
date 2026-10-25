package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/config"
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

func TestCAFile(t *testing.T) {
	cfg := config.Default()
	cfg.DataDir = "/data/doggie"

	t.Setenv(bus.CAEnv, "")
	if got, want := caFile("", cfg), "/data/doggie/tls/ca.pem"; got != want {
		t.Errorf("local default = %q, want %q", got, want)
	}
	t.Setenv(bus.CAEnv, "/env/ca.pem")
	if got := caFile("", cfg); got != "/env/ca.pem" {
		t.Errorf("env = %q", got)
	}
	if got := caFile("/flag/ca.pem", cfg); got != "/flag/ca.pem" {
		t.Errorf("flag = %q", got)
	}
	t.Setenv(bus.CAEnv, "")
	cfg.Bus.TLS.Enabled = false
	if got := caFile("", cfg); got != "" {
		t.Errorf("tls disabled = %q, want empty", got)
	}
}

func TestIsLoopback(t *testing.T) {
	for listen, want := range map[string]bool{
		"127.0.0.1:4222": true, "localhost:4222": true, "[::1]:4222": true,
		"0.0.0.0:4222": false, "192.168.1.20:4222": false, ":4222": false,
	} {
		if got := isLoopback(listen); got != want {
			t.Errorf("isLoopback(%q) = %v, want %v", listen, got, want)
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
