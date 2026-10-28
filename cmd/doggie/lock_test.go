//go:build unix

package main

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/andr2116d/doggie-assistant/internal/config"
)

func TestStartCoreRefusesASecondDoggie(t *testing.T) {
	client := localDoggie(t)
	ui := newFakeUI(false)
	host := newHosting(*client.config, noBrain(t))
	conn, _, err := startHere(t.Context(), ui, client, make(chan connState, 1), host, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer host.close()
	defer conn.Close()
	cfg, err := config.Load(*client.config)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Bus.Listen = "127.0.0.1:-1" // not even the port is shared
	if _, err := startCore(t.Context(), cfg, slog.New(slog.DiscardHandler), noBrain(t), roleChat); !errors.Is(err, errRunning) {
		t.Errorf("second core on the same data dir: %v", err)
	}
}
