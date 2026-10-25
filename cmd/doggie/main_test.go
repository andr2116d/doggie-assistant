package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/config"
)

func testConfig(t *testing.T, listen string) config.Config {
	t.Helper()
	t.Setenv(bus.TokenEnv, "")
	t.Setenv(bus.CAEnv, "")
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Bus.Listen = listen
	return cfg
}

func TestStartBusWithTLS(t *testing.T) {
	cfg := testConfig(t, "127.0.0.1:-1")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := startBus(context.Background(), cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	token, err := bus.LoadToken(tokenPath(cfg))
	if err != nil {
		t.Fatal(err)
	}
	url := srv.ClientURL()
	if _, err := bus.Dial(url, bus.DialOptions{Name: "plain", Token: token, Log: log}); err == nil {
		t.Error("dial without the CA succeeded although TLS is enabled")
	}
	c, err := bus.Dial(url, bus.DialOptions{Name: "viewer", Token: token, CAFile: caFile("", cfg), Log: log})
	if err != nil {
		t.Fatalf("dial with the local CA: %v", err)
	}
	c.Close()
}

func TestStartBusInProcessOnly(t *testing.T) {
	cfg := testConfig(t, "")
	srv, err := startBus(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	if url := srv.ClientURL(); url != "" {
		t.Errorf("ClientURL = %q, want empty", url)
	}
	if _, err := os.Stat(tlsDir(cfg)); !os.IsNotExist(err) {
		t.Error("TLS files were created although the bus does not listen on TCP")
	}
}
