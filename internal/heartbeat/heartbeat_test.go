package heartbeat

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

func TestPublishesHeartbeats(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := bus.Start(context.Background(), bus.Options{
		Token: "t", StoreDir: t.TempDir(), MaxAge: time.Hour, MaxBytes: 1 << 20, Log: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()

	pub, _ := srv.Connect("heartbeat")
	defer pub.Close()
	sub, _ := srv.Connect("test")
	defer sub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	beats, err := bus.Subscribe(ctx, sub, events.Heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	go New(pub, log, 10*time.Millisecond).Run(ctx)

	for range 2 {
		select {
		case ev := <-beats:
			if ev.Source != "heartbeat" {
				t.Errorf("Source = %q, want heartbeat", ev.Source)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no heartbeat received")
		}
	}
}
