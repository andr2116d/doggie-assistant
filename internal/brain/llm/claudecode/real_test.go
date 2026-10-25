//go:build unix

package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/brain/llm"
	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/demo"
	"github.com/andr2116d/doggie-assistant/internal/mcp"
	"github.com/andr2116d/doggie-assistant/internal/task/manager"
)

// TestRealClaudeCode runs turns on the real CLI, with the task manager and
// the demo worker, cold and pre-spawned. It uses the CLI's login, so it only
// runs with DOGGIE_REAL_CLAUDE=1.
func TestRealClaudeCode(t *testing.T) {
	if os.Getenv("DOGGIE_REAL_CLAUDE") != "1" {
		t.Skip("set DOGGIE_REAL_CLAUDE=1 to run against the real claude CLI")
	}
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
	client := conn("client")

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
