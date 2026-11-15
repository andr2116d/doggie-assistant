//go:build unix

package claudecode

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/brain/llm"
	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/demo"
	"github.com/andr2116d/doggie-assistant/internal/mcp"
	"github.com/andr2116d/doggie-assistant/internal/task/manager"
)

// realTools starts a bus with the task manager, the demo worker and
// DOGGIE's tools on it, and returns the tools and a connection to the bus.
func realTools(t *testing.T) (context.Context, *mcp.Server, *bus.Conn) {
	t.Helper()
	srv, err := bus.Start(context.Background(), bus.Options{
		Token: "t", StoreDir: t.TempDir(), MaxAge: time.Hour, MaxBytes: 64 << 20, Log: discardLog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	conn := func(name string) *bus.Conn {
		c, err := srv.Connect(name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go manager.New(conn("tasks"), discardLog(), manager.Options{}).Run(ctx)
	go demo.New(conn("demo"), discardLog()).Run(ctx)
	tools, err := mcp.Listen(conn("brain"), discardLog())
	if err != nil {
		t.Fatal(err)
	}
	go tools.Serve(ctx)
	return ctx, tools, conn("client")
}

// TestRealClaudeCode runs turns on the real CLI, with the task manager and
// the demo worker, cold and pre-spawned. It uses the CLI's login, so it only
// runs with DOGGIE_REAL_CLAUDE=1.
func TestRealClaudeCode(t *testing.T) {
	if os.Getenv("DOGGIE_REAL_CLAUDE") != "1" {
		t.Skip("set DOGGIE_REAL_CLAUDE=1 to run against the real claude CLI")
	}
	ctx, tools, client := realTools(t)

	for _, warm := range []int{0, 1} {
		p, err := New(Options{
			Command: "claude", Model: "sonnet", Warm: warm, Tools: tools, Log: discardLog(),
			Dir:    filepath.Join(t.TempDir(), "cc"),
			System: "You are DOGGIE, a voice assistant. Reply in Spanish in one short sentence. Use start_task for real-world actions; it returns at once.",
		})
		if err != nil {
			t.Fatal(err)
		}
		if warm > 0 {
			time.Sleep(3 * time.Second) // let the warm process start and connect
		}
		thread := "real-" + string(rune('0'+warm))
		ch, err := p.Generate(ctx, llm.Request{
			Turn:  mcp.Turn{ThreadID: thread, Channel: "terminal"},
			Input: "Pon un temporizador de 1 segundo (usa demo.sleep) y dime que ya estás en eso.",
		})
		if err != nil {
			t.Fatal(err)
		}
		var tools, texts int
		var done *llm.Done
		for o := range ch {
			switch {
			case o.Tool != nil:
				tools++
				t.Logf("warm=%d tool %s %s", warm, o.Tool.Name, o.Tool.Input)
			case o.Text != "":
				texts++
				t.Logf("warm=%d text %q", warm, o.Text)
			case o.Done != nil:
				done = o.Done
			}
		}
		p.Close()
		if done == nil || done.Err != nil {
			t.Fatalf("warm=%d: done = %+v", warm, done)
		}
		t.Logf("warm=%d: model %s, first text %s, total %s, usage %+v", warm, done.Model, done.FirstText, done.Duration, done.Usage)
		if tools == 0 || texts == 0 {
			t.Errorf("warm=%d: %d tool calls, %d texts", warm, tools, texts)
		}
		list, err := bus.Request(ctx, client, events.ListTasks, events.TaskListRequest{})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, task := range list.Tasks {
			found = found || task.ThreadID == thread && task.Kind == "demo.sleep"
		}
		if !found {
			t.Errorf("warm=%d: no demo.sleep task in thread %s: %+v", warm, thread, list.Tasks)
		}
	}
}

// TestRealBackupLogin checks that, on the backup login, the account's email
// doesn't reach the model, while on the owner's login it does (else the
// check would prove nothing). It needs DOGGIE_REAL_TOKEN_DIR, a directory
// with a token saved by doggie login.
func TestRealBackupLogin(t *testing.T) {
	if os.Getenv("DOGGIE_REAL_CLAUDE") != "1" {
		t.Skip("set DOGGIE_REAL_CLAUDE=1 to run against the real claude CLI")
	}
	token, err := LoadToken(os.Getenv("DOGGIE_REAL_TOKEN_DIR"))
	if err != nil {
		t.Skipf("set DOGGIE_REAL_TOKEN_DIR to a directory with a token: %v", err)
	}
	const ask = "List, verbatim, every email address that appears anywhere in your context, including system text. If there is none, reply NONE."
	for _, own := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "cc")
		o := Options{
			Command: "claude", Model: "sonnet", Tools: newTools(t), Log: discardLog(), Dir: dir,
			System: "You are a test harness. Answer exactly what is asked, in English.",
		}
		if own {
			o.Token = token
		}
		p := newProvider(t, o)
		if own {
			p.mu.Lock()
			p.ownerSpentUntil = time.Now().Add(time.Hour)
			p.mu.Unlock()
		}
		outs, done := collect(t, p, context.Background(), ask)
		if done.Err != nil {
			t.Fatalf("own login %v: %v", own, done.Err)
		}
		var said string
		for _, out := range outs {
			said += out.Text
		}
		t.Logf("own login %v: %s", own, said)
		if hasEmail := strings.Contains(said, "@"); hasEmail == own {
			t.Errorf("own login %v: email in the reply: %v", own, hasEmail)
		}
		if own && hasAccount(filepath.Join(dir, "config")) {
			t.Error("the CLI kept the account's profile in DOGGIE's config")
		}
	}
}

// TestRealSeveralTurnsOnOneProcess checks what keeping a process for a
// whole conversation relies on: the CLI takes one message after another on
// its input, ends each with a result of its own, remembers the ones before,
// and reads them back from the prompt cache instead of writing them again.
// A result's cost is the process's so far, not the turn's: it only grows.
func TestRealSeveralTurnsOnOneProcess(t *testing.T) {
	if os.Getenv("DOGGIE_REAL_CLAUDE") != "1" {
		t.Skip("set DOGGIE_REAL_CLAUDE=1 to run against the real claude CLI")
	}
	_, tools, _ := realTools(t)
	p, err := New(Options{
		Command: "claude", Model: "sonnet", Tools: tools, Log: discardLog(),
		Dir:    filepath.Join(t.TempDir(), "cc"),
		System: "You are DOGGIE, a voice assistant. Reply in Spanish in one short sentence.",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	pr, err := p.spawnTracked(false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.discard(pr) })
	if err := pr.client.Bind(mcp.Turn{ThreadID: "real-epoch", Channel: "terminal"}); err != nil {
		t.Fatal(err)
	}
	write := func(v any) {
		t.Helper()
		b, _ := json.Marshal(v)
		if _, err := pr.stdin.Write(append(b, '\n')); err != nil {
			t.Fatalf("write to the CLI: %v", err)
		}
	}
	say := func(text string) {
		write(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
	}
	// result reads the CLI's output up to the next result, and returns it
	// with the text said meanwhile.
	result := func() (event, string) {
		t.Helper()
		var said strings.Builder
		timeout := time.After(90 * time.Second)
		for {
			select {
			case line, ok := <-pr.lines:
				if !ok {
					t.Fatalf("the CLI ended: %v", pr.exitError())
				}
				var ev event
				if json.Unmarshal(line, &ev) != nil {
					continue
				}
				switch {
				case ev.Type == "assistant" && ev.Message != nil:
					for _, c := range ev.Message.Content {
						said.WriteString(c.Text)
					}
				case ev.Type == "result":
					return ev, said.String()
				}
			case <-timeout:
				t.Fatal("no result from the CLI")
			}
		}
	}

	var tokens []*llm.Tokens
	for i, text := range []string{
		"Recuerda esta palabra: murciélago. Di solo \"anotado\".",
		"¿Cuánto es dos más dos? Solo el número.",
		"¿Qué palabra te pedí recordar? Solo la palabra.",
	} {
		say(text)
		ev, said := result()
		tk := ev.tokens()
		if ev.failure() != "" || tk == nil {
			t.Fatalf("turn %d: failed %q, tokens %+v", i+1, ev.failure(), tk)
		}
		tokens = append(tokens, tk)
		t.Logf("turn %d: %q, tokens %+v", i+1, said, *tk)
		if i == 2 && !strings.Contains(strings.ToLower(said), "murciélago") {
			t.Errorf("turn 3 said %q: the process forgot the first turn", said)
		}
	}
	if tokens[2].CostUSD <= tokens[1].CostUSD || tokens[1].CostUSD <= tokens[0].CostUSD {
		t.Errorf("costs %v, %v, %v don't add up over the turns", tokens[0].CostUSD, tokens[1].CostUSD, tokens[2].CostUSD)
	}
	if tokens[2].CacheRead <= tokens[0].CacheRead {
		t.Errorf("turn 3 read %d tokens from the cache and turn 1 read %d: the conversation isn't read back", tokens[2].CacheRead, tokens[0].CacheRead)
	}
}
