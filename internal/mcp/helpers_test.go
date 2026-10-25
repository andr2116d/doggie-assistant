package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/task/manager"
	"github.com/andr2116d/doggie-assistant/internal/task/worker"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// Test-only kinds next to the real demo ones: one with a permissive input,
// one sensitive.
var (
	kindBig = events.TaskKind{
		Name: "test.big", Description: "Anything goes.", Risk: events.RiskAct,
		InputSchema: json.RawMessage(`{"type": "object"}`),
	}
	kindLock = events.TaskKind{
		Name: "test.lock", Description: "Open the front door.", Risk: events.RiskSensitive,
		InputSchema: json.RawMessage(`{"type": "object"}`),
	}
)

func testKinds() []events.TaskKind {
	return append(events.TaskKinds(), kindBig, kindLock)
}

// harness runs a bus, the task manager, a worker and the MCP server. direct
// is a bus connection that plays a person using the task commands directly.
type harness struct {
	t      *testing.T
	srv    *bus.Server
	direct *bus.Conn
	mcp    *Server
	// gate releases demo.sleep tasks; closing it releases them all.
	gate chan struct{}
}

func connect(t *testing.T, srv *bus.Server, name string) *bus.Conn {
	t.Helper()
	c, err := srv.Connect(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// run starts f in a goroutine and stops it at cleanup.
func run(t *testing.T, f func(ctx context.Context)) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		f(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	srv, err := bus.Start(context.Background(), bus.Options{
		Token: "t", StoreDir: t.TempDir(), MaxAge: time.Hour, MaxBytes: 64 << 20, Log: discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	h := &harness{t: t, srv: srv, direct: connect(t, srv, "direct"), gate: make(chan struct{})}

	m := manager.New(connect(t, srv, "tasks"), discardLog(), manager.Options{})
	run(t, func(ctx context.Context) { m.Run(ctx) })
	wconn := connect(t, srv, "worker")
	run(t, func(ctx context.Context) { worker.Serve(ctx, wconn, discardLog(), h.funcs()) })
	h.waitManager()

	h.mcp, err = listen(connect(t, srv, "brain"), discardLog(), testKinds())
	if err != nil {
		t.Fatal(err)
	}
	run(t, func(ctx context.Context) { h.mcp.Serve(ctx) })
	return h
}

func (h *harness) funcs() map[string]worker.Func {
	ask := func(ctx context.Context, t *worker.Task) (any, error) {
		in := struct {
			Question string   `json:"question"`
			Options  []string `json:"options"`
		}{Question: "Which one?"}
		if err := t.Input(&in); err != nil {
			return nil, err
		}
		a, err := t.Ask(ctx, in.Question, in.Options...)
		return map[string]string{"answer": a}, err
	}
	return map[string]worker.Func{
		events.KindDemoSleep.Name: func(ctx context.Context, t *worker.Task) (any, error) {
			select {
			case <-h.gate:
				return map[string]bool{"ok": true}, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
		events.KindDemoAsk.Name: ask,
		kindLock.Name:           ask,
		"other.ask":             ask, // a kind the brain doesn't know
		kindBig.Name: func(ctx context.Context, t *worker.Task) (any, error) {
			in := struct {
				Result   int    `json:"result_bytes"`
				Progress string `json:"progress"`
			}{}
			if err := t.Input(&in); err != nil {
				return nil, err
			}
			if in.Progress != "" {
				if err := t.Progress(ctx, in.Progress, 50); err != nil {
					return nil, err
				}
				<-h.gate
			}
			return strings.Repeat("x", in.Result), nil
		},
	}
}

func (h *harness) waitManager() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		_, err := bus.Request(ctx, h.direct, events.ListTasks, events.TaskListRequest{})
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("task manager not ready: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// bearer adds a token to every request.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return b.next.RoundTrip(r)
}

// session connects an MCP client sending token.
func (h *harness) session(token string) (*sdk.ClientSession, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	return c.Connect(ctx, &sdk.StreamableClientTransport{
		Endpoint:   h.mcp.URL(),
		HTTPClient: &http.Client{Transport: bearer{token: token, next: http.DefaultTransport}},
		MaxRetries: -1,
	}, nil)
}

// bound returns a client bound to thread, and a session using it.
func (h *harness) bound(thread string) (*Client, *sdk.ClientSession) {
	h.t.Helper()
	c, err := h.mcp.NewClient()
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(c.Close)
	if err := c.Bind(Turn{ThreadID: thread, Channel: "terminal"}); err != nil {
		h.t.Fatal(err)
	}
	cs, err := h.session(c.Token())
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { cs.Close() })
	return c, cs
}

func call(t *testing.T, cs *sdk.ClientSession, name string, args any) (*sdk.CallToolResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
}

// callOK calls a tool that must succeed and decodes its output into out.
func callOK(t *testing.T, cs *sdk.ClientSession, name string, args, out any) {
	t.Helper()
	res, err := call(t, cs, name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s(%v) failed: %s", name, args, text(res))
	}
	if out != nil {
		b, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: decode %s: %v", name, b, err)
		}
	}
}

// callErr calls a tool that must fail and returns its error text.
func callErr(t *testing.T, cs *sdk.ClientSession, name string, args any) string {
	t.Helper()
	res, err := call(t, cs, name, args)
	if err != nil {
		return err.Error() // a protocol-level failure, e.g. invalid arguments
	}
	if !res.IsError {
		t.Fatalf("%s(%v) succeeded, want an error", name, args)
	}
	return text(res)
}

func text(res *sdk.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, " ")
}

func (h *harness) get(id string) events.TaskInfo {
	h.t.Helper()
	info, err := bus.Request(context.Background(), h.direct, events.GetTask, events.TaskRef{TaskID: id})
	if err != nil {
		h.t.Fatalf("get %s: %v", id, err)
	}
	return info
}

func (h *harness) waitStatus(id string, want events.TaskStatus) events.TaskInfo {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		info := h.get(id)
		if info.Status == want {
			return info
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("task %s is %s, want %s", id, info.Status, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startDirect starts a task as a person would, outside the brain.
func (h *harness) startDirect(spec events.TaskSpec) string {
	h.t.Helper()
	ref, err := bus.Request(context.Background(), h.direct, events.StartTask, spec)
	if err != nil {
		h.t.Fatalf("start %+v: %v", spec, err)
	}
	return ref.TaskID
}

func (h *harness) taskCount() int {
	h.t.Helper()
	list, err := bus.Request(context.Background(), h.direct, events.ListTasks, events.TaskListRequest{})
	if err != nil {
		h.t.Fatal(err)
	}
	return list.Total
}

func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("%q does not mention %q", got, want)
	}
}
