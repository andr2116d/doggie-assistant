package bus

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// captureHandler records log lines so tests can assert on them.
type captureHandler struct {
	mu    sync.Mutex
	lines []string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	h.mu.Lock()
	h.lines = append(h.lines, b.String())
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) contains(s string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.lines {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

func opts(storeDir, listen string, log *slog.Logger) Options {
	return Options{
		Listen: listen, Token: "test-token", StoreDir: storeDir,
		MaxAge: time.Hour, MaxBytes: 64 << 20, Log: log,
	}
}

func mustStart(t *testing.T, o Options) *Server {
	t.Helper()
	s, err := Start(context.Background(), o)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return s
}

func eventLogInfo(t *testing.T, c *Conn) *jetstream.StreamInfo {
	t.Helper()
	js, err := jetstream.New(c.nc)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := js.Stream(context.Background(), EventLogStream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := stream.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func waitForLogged(t *testing.T, c *Conn, n uint64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for eventLogInfo(t, c).State.Msgs < n {
		if time.Now().After(deadline) {
			t.Fatalf("event log has fewer than %d events", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// delayProxy forwards TCP connections to target, delaying every chunk the
// client sends by delay. It makes subscription races deterministic.
func delayProxy(t *testing.T, target string, delay time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			server, err := net.Dial("tcp", target)
			if err != nil {
				client.Close()
				return
			}
			go func() { // server -> client, as is
				io.Copy(client, server)
				client.Close()
			}()
			go func() { // client -> server, delayed
				buf := make([]byte, 32<<10)
				for {
					n, err := client.Read(buf)
					if err != nil {
						server.Close()
						return
					}
					time.Sleep(delay)
					if _, err := server.Write(buf[:n]); err != nil {
						return
					}
				}
			}()
		}
	}()
	return "nats://" + ln.Addr().String()
}
