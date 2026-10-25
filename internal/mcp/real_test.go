package mcp

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

// TestRealClaudeCode checks the server against the real Claude Code CLI: it
// must connect with the bearer token, see the tools and start a task in the
// bound thread. It uses the CLI's login, so it only runs with
// DOGGIE_REAL_CLAUDE=1.
func TestRealClaudeCode(t *testing.T) {
	if os.Getenv("DOGGIE_REAL_CLAUDE") != "1" {
		t.Skip("set DOGGIE_REAL_CLAUDE=1 to run against the real claude CLI")
	}
	h := newHarness(t)
	c, err := h.mcp.NewClient()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Bind(Turn{ThreadID: "real", Channel: "terminal"}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"doggie": map[string]any{
		"type": "http", "url": h.mcp.URL(), "headers": map[string]string{"Authorization": "Bearer " + c.Token()},
	}}})
	cfgFile := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(cfgFile, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(dir, "cwd")
	os.Mkdir(cwd, 0o700)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "-p",
		"--output-format", "json",
		"--tools", "", "--strict-mcp-config", "--mcp-config", cfgFile,
		"--allowedTools", "mcp__doggie__*", "--permission-prompts", "none",
		"--setting-sources", "", "--no-session-persistence", "--disable-slash-commands",
		"--model", "sonnet",
		"--system-prompt", "You are a test harness. Do exactly what you are asked, then reply DONE.",
		"Call list_tasks. Then start a demo.sleep task with seconds 1 and the description 'Real test'. Then reply DONE.")
	cmd.Dir = cwd
	cmd.Env = withoutAPIKeys(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("claude: %v\n%s", err, out)
	}
	t.Logf("claude: %s", out)

	close(h.gate)
	list := h.waitAny()
	if len(list) != 1 || list[0].Kind != "demo.sleep" || list[0].ThreadID != "real" || list[0].Description != "Real test" {
		t.Fatalf("tasks = %+v, want one demo.sleep in thread real", list)
	}
}

func (h *harness) waitAny() []events.TaskInfo {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		list, err := busList(h)
		if err == nil && len(list) > 0 || time.Now().After(deadline) {
			return list
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func busList(h *harness) ([]events.TaskInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	list, err := bus.Request(ctx, h.direct, events.ListTasks, events.TaskListRequest{})
	return list.Tasks, err
}

func withoutAPIKeys(env []string) []string {
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") || strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// With a revoked or wrong token, the CLI must still finish its turn (without
// tools) instead of hanging in an OAuth flow.
func TestRealClaudeCodeWithABadToken(t *testing.T) {
	if os.Getenv("DOGGIE_REAL_CLAUDE") != "1" {
		t.Skip("set DOGGIE_REAL_CLAUDE=1 to run against the real claude CLI")
	}
	h := newHarness(t)
	dir := t.TempDir()
	cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"doggie": map[string]any{
		"type": "http", "url": h.mcp.URL(), "headers": map[string]string{"Authorization": "Bearer " + strings.Repeat("0", 64)},
	}}})
	cfgFile := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(cfgFile, cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	start := time.Now()
	cmd := exec.CommandContext(ctx, "claude", "-p",
		"--output-format", "json",
		"--tools", "", "--strict-mcp-config", "--mcp-config", cfgFile,
		"--allowedTools", "mcp__doggie__*", "--permission-prompts", "none",
		"--setting-sources", "", "--no-session-persistence", "--disable-slash-commands",
		"--model", "sonnet",
		"--system-prompt", "You are a test harness. Reply with the names of the tools you have, or NONE.",
		"Which tools do you have?")
	cmd.Dir = dir
	cmd.Env = withoutAPIKeys(os.Environ())
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("claude hung with a bad MCP token (%s)", time.Since(start))
	}
	if err != nil {
		t.Fatalf("claude: %v\n%s", err, out)
	}
	t.Logf("claude (%s): %s", time.Since(start).Round(time.Millisecond), out)
	if n := h.taskCount(); n != 0 {
		t.Errorf("%d tasks started with a bad token", n)
	}
}
