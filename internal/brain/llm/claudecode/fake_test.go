//go:build unix

package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as a fake claude CLI: run with DOGGIE_FAKE_CLAUDE=1
// it speaks stream-json like the real one. What it does in a turn comes from
// a script sent as the turn's input; what it does at startup comes from the
// environment, which each spawn reads anew.
func TestMain(m *testing.M) {
	switch {
	case os.Getenv("DOGGIE_FAKE_CHILD") == "1":
		time.Sleep(time.Hour) // a process the fake starts, to test process groups
		os.Exit(0)
	case os.Getenv("DOGGIE_FAKE_CLAUDE") == "1":
		fakeMain()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// script is what the fake does in a turn.
type script struct {
	// MCP is the status reported for DOGGIE's server: "" (connected),
	// "failed", or "fail_once:<file>" (failed the first time only).
	MCP string `json:"mcp,omitempty"`
	// InitDelayMS delays the init event, as if the CLI were slow to start.
	InitDelayMS int    `json:"init_delay_ms,omitempty"`
	Steps       []step `json:"steps"`
}

type step struct {
	Text     string          `json:"text,omitempty"`     // a text block, streamed first
	Thinking string          `json:"thinking,omitempty"` // a thinking block
	Tool     string          `json:"tool,omitempty"`     // a tool_use block (not executed)
	Args     json.RawMessage `json:"args,omitempty"`
	Call     string          `json:"call,omitempty"` // call the tool over MCP and say the result
	Report   bool            `json:"report,omitempty"`
	SleepMS  int             `json:"sleep_ms,omitempty"`
	Exit     *int            `json:"exit,omitempty"` // exit without a result
	Stderr   string          `json:"stderr,omitempty"`
	Garbage  bool            `json:"garbage,omitempty"`
	Huge     int             `json:"huge,omitempty"` // a line of this many bytes
	Rate     []float64       `json:"rate,omitempty"` // five_hour, seven_day
	Error    string          `json:"error,omitempty"`
	Hang     bool            `json:"hang,omitempty"`
	Child    bool            `json:"child,omitempty"`   // start a child process and report its PID
	Raw      json.RawMessage `json:"raw,omitempty"`     // a line as is; a result ends the turn
	Account  bool            `json:"account,omitempty"` // keep an account profile in $CLAUDE_CONFIG_DIR
	// Spent ends the turn as the CLI does once the usage limit is spent, on
	// the "owner" login, the "backup" one or "both". Renews and
	// BackupRenews are when each renews (Unix seconds, 0 for unknown).
	Spent        string `json:"spent,omitempty"`
	Renews       int64  `json:"renews,omitempty"`
	BackupRenews int64  `json:"backup_renews,omitempty"`
}

// report is what the fake tells about itself.
type report struct {
	Args       []string    `json:"args"`
	Env        []string    `json:"env"`
	Cwd        string      `json:"cwd"`
	CwdEntries []string    `json:"cwd_entries"`
	System     string      `json:"system"`
	MCPConfig  string      `json:"mcp_config"`
	MCPMode    os.FileMode `json:"mcp_mode"`
	PID        int         `json:"pid"`
	Started    time.Time   `json:"started"`
	InputAt    time.Time   `json:"input_at"`
	InputLen   int         `json:"input_len"`
	ChildPID   int         `json:"child_pid,omitempty"`
}

func fakeMain() {
	started := time.Now()
	if f := os.Getenv("DOGGIE_FAKE_SPAWNLOG"); f != "" {
		appendLine(f, fmt.Sprint(os.Getpid()))
	}
	if code := os.Getenv("DOGGIE_FAKE_EXIT_AT_START"); code != "" {
		fmt.Fprintln(os.Stderr, "fake: dying at start")
		os.Exit(3)
	}
	out := bufio.NewWriter(os.Stdout)
	emit := func(v any) {
		b, _ := json.Marshal(v)
		out.Write(append(b, '\n'))
		out.Flush()
	}

	line, err := bufio.NewReaderSize(os.Stdin, 4<<20).ReadBytes('\n')
	if len(line) == 0 && err != nil {
		os.Exit(0) // stdin closed before any input
	}
	inputAt := time.Now()
	if f := os.Getenv("DOGGIE_FAKE_ENVLOG"); f != "" {
		appendLine(f, strings.Join(os.Environ(), "\n"))
	}
	if f := os.Getenv("DOGGIE_FAKE_INPUTLOG"); f != "" {
		appendLine(f, fmt.Sprint(os.Getpid()))
	}
	var msg struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	json.Unmarshal(line, &msg)
	var sc script
	if err := json.Unmarshal([]byte(msg.Message.Content), &sc); err != nil || sc.Steps == nil {
		sc = script{Steps: []step{{Text: "echo:" + msg.Message.Content}}}
	}

	status := "connected"
	switch {
	case sc.MCP == "failed":
		status = "failed"
	case strings.HasPrefix(sc.MCP, "fail_once:"):
		marker := strings.TrimPrefix(sc.MCP, "fail_once:")
		if _, err := os.Stat(marker); err != nil {
			os.WriteFile(marker, nil, 0o600)
			status = "failed"
		}
	}
	time.Sleep(time.Duration(sc.InitDelayMS) * time.Millisecond)
	emit(map[string]any{"type": "system", "subtype": "init", "model": "fake-model",
		"mcp_servers": []map[string]string{{"name": "doggie", "status": status}}})

	var childPID int
	mcpURL, mcpAuth := mcpConfig()
	for _, s := range sc.Steps {
		switch {
		case s.Text != "":
			emit(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta",
				"delta": map[string]string{"type": "text_delta", "text": s.Text}}})
			emit(assistant(map[string]any{"type": "text", "text": s.Text}))
		case s.Thinking != "":
			emit(assistant(map[string]any{"type": "thinking", "thinking": s.Thinking}))
		case s.Tool != "":
			emit(assistant(map[string]any{"type": "tool_use", "id": "toolu_1", "name": "mcp__doggie__" + s.Tool, "input": s.Args}))
		case s.Call != "":
			emit(assistant(map[string]any{"type": "text", "text": "tool:" + callTool(mcpURL, mcpAuth, s.Call, s.Args)}))
		case s.Report:
			r := selfReport(started, inputAt, len(msg.Message.Content))
			r.ChildPID = childPID
			b, _ := json.Marshal(r)
			emit(assistant(map[string]any{"type": "text", "text": "report:" + string(b)}))
		case s.Child:
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), "DOGGIE_FAKE_CHILD=1", "DOGGIE_FAKE_CLAUDE=")
			cmd.Start()
			childPID = cmd.Process.Pid
			if f := os.Getenv("DOGGIE_FAKE_CHILDLOG"); f != "" {
				appendLine(f, fmt.Sprint(childPID))
			}
		case s.SleepMS > 0:
			time.Sleep(time.Duration(s.SleepMS) * time.Millisecond)
		case s.Exit != nil:
			fmt.Fprint(os.Stderr, s.Stderr)
			out.Flush()
			os.Exit(*s.Exit)
		case s.Garbage:
			out.WriteString("{not json\n\n[1,2]\n\"str\"\n")
			out.Flush()
		case s.Huge > 0:
			out.WriteString(`{"type":"stream_event","pad":"` + strings.Repeat("x", s.Huge) + "\"}\n")
			out.Flush()
		case s.Rate != nil:
			emit(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"unifiedWindows": map[string]any{
				"five_hour": map[string]float64{"utilization": s.Rate[0]}, "seven_day": map[string]float64{"utilization": s.Rate[1]}}}})
		case s.Error != "":
			emit(map[string]any{"type": "result", "subtype": "error_during_execution", "is_error": true, "result": s.Error})
			return
		case s.Spent != "":
			backup := strings.HasSuffix(os.Getenv("CLAUDE_CONFIG_DIR"), filepath.Join("claudecode", "config"))
			renews := s.Renews
			if backup {
				renews = s.BackupRenews
			}
			if s.Spent == "both" || (s.Spent == "backup") == backup {
				emit(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{"status": "rejected", "resetsAt": renews}})
				emit(map[string]any{"type": "assistant", "message": map[string]any{"model": "<synthetic>", "content": []any{map[string]any{"type": "text", "text": "You've hit your session limit"}}}})
				emit(map[string]any{"type": "result", "subtype": "success", "is_error": true, "api_error_status": 429, "result": "You've hit your session limit"})
				return
			}
		case s.Account:
			os.WriteFile(filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), ".claude.json"),
				[]byte(`{"numStartups":3,"oauthAccount":{"emailAddress":"someone@example.com"}}`), 0o600)
		case s.Raw != nil:
			out.Write(append(append([]byte(nil), s.Raw...), '\n'))
			out.Flush()
			if bytes.Contains(s.Raw, []byte(`"type":"result"`)) {
				return
			}
		case s.Hang:
			select {}
		}
	}
	emit(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "ok"})
}

func assistant(block map[string]any) map[string]any {
	return map[string]any{"type": "assistant", "message": map[string]any{"model": "fake-model", "content": []any{block}}}
}

// mcpConfig reads the --mcp-config file the fake was started with.
func mcpConfig() (url, auth string) {
	for i, a := range os.Args {
		if a == "--mcp-config" && i+1 < len(os.Args) {
			b, _ := os.ReadFile(os.Args[i+1])
			var cfg struct {
				MCPServers map[string]struct {
					URL     string            `json:"url"`
					Headers map[string]string `json:"headers"`
				} `json:"mcpServers"`
			}
			json.Unmarshal(b, &cfg)
			s := cfg.MCPServers["doggie"]
			return s.URL, s.Headers["Authorization"]
		}
	}
	return "", ""
}

// callTool calls a tool on DOGGIE's MCP server, like the real CLI would, and
// returns its result text (or the HTTP failure).
func callTool(url, auth, name string, args json.RawMessage) string {
	if args == nil {
		args = json.RawMessage(`{}`)
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", auth)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "http error: " + err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Sprintf("http %d", resp.StatusCode)
	}
	return string(b)
}

func selfReport(started, inputAt time.Time, inputLen int) report {
	cwd, _ := os.Getwd()
	r := report{Args: os.Args[1:], Cwd: cwd, PID: os.Getpid(), Started: started, InputAt: inputAt, InputLen: inputLen}
	entries, _ := os.ReadDir(cwd)
	for _, e := range entries {
		r.CwdEntries = append(r.CwdEntries, e.Name())
	}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ANTHROPIC_") || strings.HasPrefix(kv, "CLAUDE_") || strings.HasPrefix(kv, "DOGGIE_TEST_") {
			r.Env = append(r.Env, kv)
		}
	}
	for i, a := range os.Args {
		if i+1 >= len(os.Args) {
			break
		}
		switch a {
		case "--system-prompt-file":
			b, _ := os.ReadFile(os.Args[i+1])
			r.System = string(b)
		case "--mcp-config":
			b, _ := os.ReadFile(os.Args[i+1])
			r.MCPConfig = string(b)
			if fi, err := os.Stat(os.Args[i+1]); err == nil {
				r.MCPMode = fi.Mode().Perm()
			}
		}
	}
	return r
}

func appendLine(file, s string) {
	f, err := os.OpenFile(file, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, s)
}

// processAlive reports whether pid still runs (and is not a zombie we own).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
