package bus

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// startTestServer runs a server with its event log in a temp dir. listen ""
// means in-process only; "127.0.0.1:-1" picks a random TCP port.
func startTestServer(t *testing.T, listen string) *Server {
	t.Helper()
	s, err := Start(context.Background(), Options{
		Listen:   listen,
		Token:    "test-token",
		StoreDir: t.TempDir(),
		MaxAge:   time.Hour,
		MaxBytes: 1 << 20,
		Log:      discardLog(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(s.Shutdown)
	return s
}

func connect(t *testing.T, s *Server, name string) *Conn {
	t.Helper()
	c, err := s.Connect(name)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestStartRequiresToken(t *testing.T) {
	_, err := Start(context.Background(), Options{StoreDir: t.TempDir(), Log: discardLog()})
	if err == nil {
		t.Fatal("expected an error without a token")
	}
}

func TestInProcessOnlyHasNoClientURL(t *testing.T) {
	s := startTestServer(t, "")
	if url := s.ClientURL(); url != "" {
		t.Errorf("ClientURL = %q, want empty", url)
	}
}

func TestTCPRequiresToken(t *testing.T) {
	s := startTestServer(t, "127.0.0.1:-1")
	url := s.ClientURL()

	if _, err := Dial(url, "wrong", "intruder", discardLog()); err == nil {
		t.Fatal("dial with a wrong token succeeded")
	}
	c, err := Dial(url, "test-token", "satellite", discardLog())
	if err != nil {
		t.Fatalf("dial with the right token: %v", err)
	}
	c.Close()
}

func TestEventLogStreamExists(t *testing.T) {
	s := startTestServer(t, "")
	c := connect(t, s, "test")

	js, err := jetstream.New(c.nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(context.Background(), EventLogStream)
	if err != nil {
		t.Fatalf("event log stream: %v", err)
	}
	cfg := stream.CachedInfo().Config
	if cfg.MaxAge != time.Hour || cfg.MaxBytes != 1<<20 || cfg.Discard != jetstream.DiscardOld {
		t.Errorf("unexpected stream config: %+v", cfg)
	}
}
