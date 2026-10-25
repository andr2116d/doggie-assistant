package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`

// post sends a raw JSON-RPC request with an optional token and host.
func post(t *testing.T, url, token, host, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestListensOnLoopbackOnly(t *testing.T) {
	h := newHarness(t)
	host, _, err := net.SplitHostPort(strings.TrimSuffix(strings.TrimPrefix(h.mcp.URL(), "http://"), "/mcp"))
	if err != nil {
		t.Fatal(err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Errorf("listening on %s, want a loopback address", host)
	}
}

func TestRejectsRequestsWithoutAValidToken(t *testing.T) {
	h := newHarness(t)
	c, err := h.mcp.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	for name, token := range map[string]string{
		"no token":        "",
		"wrong token":     strings.Repeat("0", 64),
		"truncated token": c.Token()[:32],
		"token hash":      c.hash, // what identifies the client must not work as a token
	} {
		if resp := post(t, h.mcp.URL(), token, "", initBody); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
		}
	}
	// Not a bearer token, or a malformed header.
	for _, header := range []string{"Basic " + c.Token(), "Bearer", "Bearer " + c.Token() + " extra", c.Token()} {
		req, _ := http.NewRequest(http.MethodPost, h.mcp.URL(), strings.NewReader(initBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", header)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status %d, want 401", header, resp.StatusCode)
		}
	}
	if resp := post(t, h.mcp.URL(), c.Token(), "", initBody); resp.StatusCode != http.StatusOK {
		t.Errorf("valid token: status %d, want 200", resp.StatusCode)
	}
}

// DNS rebinding: a web page can make the browser reach 127.0.0.1 under its
// own host name. Even with a token, a foreign Host is refused.
func TestRejectsForeignHost(t *testing.T) {
	h := newHarness(t)
	c, _ := h.mcp.NewClient()
	defer c.Close()
	if resp := post(t, h.mcp.URL(), c.Token(), "evil.example.com", initBody); resp.StatusCode != http.StatusForbidden {
		t.Errorf("status %d, want 403", resp.StatusCode)
	}
}

func TestRejectsHugeBodies(t *testing.T) {
	h := newHarness(t)
	c, _ := h.mcp.NewClient()
	defer c.Close()
	huge := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"start_task","arguments":{"kind":"test.big","description":"x","input":{"pad":%q}}}}`,
		strings.Repeat("x", maxRequestBytes))
	resp := post(t, h.mcp.URL(), c.Token(), "", huge)
	if resp.StatusCode == http.StatusOK {
		t.Errorf("a %d-byte request was accepted", len(huge))
	}
	if n := h.taskCount(); n != 0 {
		t.Errorf("%d tasks started from a rejected request", n)
	}
}

func TestToolsNeedABoundTurn(t *testing.T) {
	h := newHarness(t)
	c, err := h.mcp.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cs, err := h.session(c.Token())
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	start := map[string]any{"kind": "demo.sleep", "description": "Set a timer"}

	// A pre-spawned process connects before its turn: tools refuse to act.
	mustContain(t, callErr(t, cs, "start_task", start), "no conversation turn")
	mustContain(t, callErr(t, cs, "list_tasks", map[string]any{}), "no conversation turn")

	if err := c.Bind(Turn{ThreadID: "th1", Channel: "terminal"}); err != nil {
		t.Fatal(err)
	}
	var out startTaskOut
	callOK(t, cs, "start_task", start, &out)

	// After the turn, the same token can't act anymore...
	c.Unbind()
	mustContain(t, callErr(t, cs, "start_task", start), "no conversation turn")
	// ...until the next turn binds it again.
	if err := c.Bind(Turn{ThreadID: "th2", Channel: "terminal"}); err != nil {
		t.Fatal(err)
	}
	callOK(t, cs, "start_task", start, &out)
	if got := h.get(out.TaskID).ThreadID; got != "th2" {
		t.Errorf("task of the second turn is in thread %q, want th2", got)
	}

	// Once closed, the token is revoked.
	c.Close()
	if _, err := call(t, cs, "list_tasks", map[string]any{}); err == nil {
		t.Error("a closed client could still call a tool")
	}
	if resp := post(t, h.mcp.URL(), c.Token(), "", initBody); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("closed client: status %d, want 401", resp.StatusCode)
	}
	if err := c.Bind(Turn{ThreadID: "th3"}); err == nil {
		t.Error("a closed client could be bound again")
	}
	c.Unbind() // harmless
	c.Close()  // idempotent
	if n := h.taskCount(); n != 2 {
		t.Errorf("%d tasks, want 2 (only from bound turns)", n)
	}
}

func TestBindNeedsAThread(t *testing.T) {
	h := newHarness(t)
	c, _ := h.mcp.NewClient()
	defer c.Close()
	if err := c.Bind(Turn{Channel: "terminal"}); err == nil {
		t.Error("bound a turn without a thread")
	}
}

func TestTokensAreUniqueAndUnguessable(t *testing.T) {
	h := newHarness(t)
	seen := make(map[string]bool)
	for range 100 {
		c, err := h.mcp.NewClient()
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Token()) != 64 || seen[c.Token()] {
			t.Fatalf("bad or repeated token %q", c.Token())
		}
		seen[c.Token()] = true
		c.Close()
	}
	h.mcp.mu.Lock()
	n := len(h.mcp.clients)
	h.mcp.mu.Unlock()
	if n != 0 {
		t.Errorf("%d clients left after closing them all", n)
	}
}

// Two LLM processes serving different threads at once: each task lands in the
// thread of the client that started it, whatever the timing.
func TestConcurrentClientsKeepTheirThreads(t *testing.T) {
	h := newHarness(t)
	const perClient = 10
	threads := []string{"alpha", "beta", "gamma"}
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids = make(map[string]string) // task -> thread
	)
	for _, th := range threads {
		_, cs := h.bound(th)
		for range perClient {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := call(t, cs, "start_task", map[string]any{"kind": "demo.sleep", "description": "Wait"})
				if err != nil || res.IsError {
					t.Errorf("start_task: %v %v", err, res)
					return
				}
				b, _ := json.Marshal(res.StructuredContent)
				var out startTaskOut
				json.Unmarshal(b, &out)
				mu.Lock()
				ids[out.TaskID] = th
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	if len(ids) != perClient*len(threads) {
		t.Fatalf("%d distinct task IDs, want %d", len(ids), perClient*len(threads))
	}
	for id, th := range ids {
		if got := h.get(id).ThreadID; got != th {
			t.Errorf("task %s is in thread %q, want %q", id, got, th)
		}
	}
}

func TestListsTheToolsWithTheirRisk(t *testing.T) {
	h := newHarness(t)
	_, cs := h.bound("th")
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]events.Risk{
		"start_task": events.RiskAct, "list_tasks": events.RiskRead, "get_task": events.RiskRead,
		"cancel_task": events.RiskAct, "answer_task": events.RiskAct,
	}
	if got := h.mcp.Tools(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Tools() = %v, want %v", got, want)
	}
	if len(res.Tools) != len(want) {
		t.Errorf("listed %d tools, want %d", len(res.Tools), len(want))
	}
	for _, tool := range res.Tools {
		risk, ok := want[tool.Name]
		if !ok {
			t.Errorf("unexpected tool %q", tool.Name)
			continue
		}
		readOnly := tool.Annotations != nil && tool.Annotations.ReadOnlyHint
		if readOnly != (risk == events.RiskRead) {
			t.Errorf("%s: read-only hint %v with risk %s", tool.Name, readOnly, risk)
		}
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
	}
	// The start_task description lists every kind with its risk and schema.
	for _, tool := range res.Tools {
		if tool.Name != "start_task" {
			continue
		}
		for _, k := range testKinds() {
			mustContain(t, tool.Description, fmt.Sprintf("- %s (risk: %s)", k.Name, k.Risk))
		}
		mustContain(t, tool.Description, `"maximum":3600`)
	}
	// Mutating the returned map doesn't change the server's.
	h.mcp.Tools()["start_task"] = events.RiskRead
	if h.mcp.Tools()["start_task"] != events.RiskAct {
		t.Error("Tools() exposes the server's own map")
	}
}

func TestServeStopsAndRevokesClients(t *testing.T) {
	srv, err := bus.Start(context.Background(), bus.Options{
		Token: "t", StoreDir: t.TempDir(), MaxAge: time.Hour, MaxBytes: 64 << 20, Log: discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	s, err := Listen(connect(t, srv, "brain"), discardLog())
	if err != nil {
		t.Fatal(err)
	}
	c, _ := s.NewClient()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	if resp := post(t, s.URL(), c.Token(), "", initBody); resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d before stopping", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Serve returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
	if _, err := http.Post(s.URL(), "application/json", bytes.NewReader([]byte(initBody))); err == nil {
		t.Error("server still accepts connections after stopping")
	}
	if err := c.Bind(Turn{ThreadID: "th"}); err == nil {
		t.Error("a client survived the server")
	}
}

// Serve with an already cancelled context must not hang or leak.
func TestServeWithCancelledContext(t *testing.T) {
	srv, err := bus.Start(context.Background(), bus.Options{
		Token: "t", StoreDir: t.TempDir(), MaxAge: time.Hour, MaxBytes: 64 << 20, Log: discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	s, err := Listen(connect(t, srv, "brain"), discardLog())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Serve hangs with a cancelled context")
	}
}

func TestCompileKindsRejectsBadCatalogs(t *testing.T) {
	ok := json.RawMessage(`{"type":"object"}`)
	tests := map[string][]events.TaskKind{
		"invalid name":   {{Name: "bad kind", Risk: events.RiskAct, InputSchema: ok}},
		"wildcard name":  {{Name: "notes.*", Risk: events.RiskAct, InputSchema: ok}},
		"empty name":     {{Name: "", Risk: events.RiskAct, InputSchema: ok}},
		"duplicate":      {{Name: "a.b", Risk: events.RiskAct, InputSchema: ok}, {Name: "a.b", Risk: events.RiskRead, InputSchema: ok}},
		"no risk":        {{Name: "a.b", InputSchema: ok}},
		"unknown risk":   {{Name: "a.b", Risk: "harmless", InputSchema: ok}},
		"no schema":      {{Name: "a.b", Risk: events.RiskAct}},
		"invalid json":   {{Name: "a.b", Risk: events.RiskAct, InputSchema: json.RawMessage(`{"type":`)}},
		"invalid schema": {{Name: "a.b", Risk: events.RiskAct, InputSchema: json.RawMessage(`{"type": 5}`)}},
		"bad ref":        {{Name: "a.b", Risk: events.RiskAct, InputSchema: json.RawMessage(`{"$ref": "#/nowhere"}`)}},
	}
	for name, kinds := range tests {
		if _, err := compileKinds(kinds); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}
}

func TestTheRealCatalogCompiles(t *testing.T) {
	kinds, err := compileKinds(events.TaskKinds())
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range kinds {
		if strings.TrimSpace(k.Description) == "" {
			t.Errorf("%s has no description for the LLM", k.Name)
		}
	}
}

// The SDK client used in these tests must see tool errors as results, not
// protocol errors, so the LLM can read and react to them.
func TestToolErrorsReachTheModel(t *testing.T) {
	h := newHarness(t)
	_, cs := h.bound("th")
	res, err := call(t, cs, "get_task", map[string]any{"task_id": "t999"})
	if err != nil {
		t.Fatalf("protocol error: %v", err)
	}
	if !res.IsError {
		t.Fatal("no error for an unknown task")
	}
	mustContain(t, text(res), "t999")
}

// Starting, using and stopping servers many times must not leak goroutines
// (HTTP connections, shutdown watchers).
func TestServeDoesNotLeak(t *testing.T) {
	srv, err := bus.Start(context.Background(), bus.Options{
		Token: "t", StoreDir: t.TempDir(), MaxAge: time.Hour, MaxBytes: 64 << 20, Log: discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	conn := connect(t, srv, "brain")
	cycle := func() {
		s, err := Listen(conn, discardLog())
		if err != nil {
			t.Fatal(err)
		}
		c, _ := s.NewClient()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Serve(ctx) }()
		resp := post(t, s.URL(), c.Token(), "", initBody)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		cancel()
		<-done
	}
	cycle() // warm up pools and lazily started goroutines
	http.DefaultClient.CloseIdleConnections()
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()
	for range 30 {
		cycle()
	}
	http.DefaultClient.CloseIdleConnections()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before+2 {
		t.Errorf("%d goroutines after 30 cycles, %d before", n, before)
	}
}

// When Serve fails on its own (the listener breaks) while ctx is still
// alive, it returns the error and leaves nothing behind.
func TestServeFailureDoesNotLeak(t *testing.T) {
	srv, err := bus.Start(context.Background(), bus.Options{
		Token: "t", StoreDir: t.TempDir(), MaxAge: time.Hour, MaxBytes: 64 << 20, Log: discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Shutdown()
	conn := connect(t, srv, "brain")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	before := runtime.NumGoroutine()
	for range 30 {
		s, err := Listen(conn, discardLog())
		if err != nil {
			t.Fatal(err)
		}
		s.ln.Close()
		if err := s.Serve(ctx); err == nil || err == context.Canceled {
			t.Fatalf("Serve on a broken listener returned %v", err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before+2 {
		t.Errorf("%d goroutines after 30 failed Serves, %d before", n, before)
	}
}
