package claudecode

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/mcp"
)

const (
	// maxLine bounds one line of the CLI's output. A longer one means
	// something is wrong; the process is killed.
	maxLine = 16 << 20
	// stderrTail is how much of the CLI's stderr is kept for error messages.
	stderrTail = 4 << 10
)

// proc is one claude process. It serves exactly one turn: DOGGIE writes one
// input, closes stdin and reads until the result.
type proc struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	lines   chan []byte   // stdout lines; closed at EOF
	readErr error         // why stdout ended; valid once lines is closed
	exited  chan struct{} // closed once the process is reaped
	waitErr error         // valid once exited is closed
	stderr  *tail
	client  *mcp.Client
	cfgFile string
	started time.Time
	// taken is closed when a warm process is taken for a turn (or
	// discarded), so its watcher stops. Nil for other processes.
	taken chan struct{}
	// backup is set when the process runs on the backup login.
	backup bool
	// said is set once the turn gave out something.
	said bool

	stopOnce sync.Once
}

// spawn starts a claude process with its own MCP client, on the owner's
// login or the backup one. It is ready for its turn as soon as it returns:
// the CLI connects to the tools while it waits for input.
func (p *Provider) spawn(backup bool) (*proc, error) {
	client, err := p.opts.Tools.NewClient()
	if err != nil {
		return nil, err
	}
	cfgFile, err := p.writeMCPConfig(client.Token())
	if err != nil {
		client.Close()
		return nil, err
	}
	cmd := exec.Command(p.opts.Command, p.args(cfgFile)...)
	cmd.Dir = p.cwd
	token := ""
	if backup {
		token = p.opts.Token
	}
	cmd.Env = childEnv(os.Environ(), token, p.configDir)
	setProcessGroup(cmd)
	pr := &proc{
		cmd: cmd, lines: make(chan []byte, 64), exited: make(chan struct{}),
		stderr: &tail{max: stderrTail}, client: client, cfgFile: cfgFile, backup: backup,
	}
	cmd.Stderr = pr.stderr
	if pr.stdin, err = cmd.StdinPipe(); err != nil {
		pr.cleanup()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		pr.cleanup()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		pr.cleanup()
		return nil, fmt.Errorf("claudecode: start %s: %w", p.opts.Command, err)
	}
	pr.started = time.Now()
	go pr.read(stdout)
	return pr, nil
}

// args lock the CLI down: no built-in tools, only DOGGIE's MCP server, no
// settings, sessions or slash commands, and DOGGIE's own system prompt.
// Files carry the system prompt and the MCP config, since one argument is
// limited to 128 KiB on Linux and any local user can read the command line.
func (p *Provider) args(cfgFile string) []string {
	return []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--tools", "",
		"--strict-mcp-config", "--mcp-config", cfgFile,
		"--allowedTools", toolPrefix + "*",
		"--permission-prompts", "none",
		"--setting-sources", "",
		"--system-prompt-file", p.systemFile,
		"--no-session-persistence",
		"--disable-slash-commands",
		"--model", p.opts.Model,
	}
}

// childEnv is the environment for claude. Without API keys the CLI uses a
// subscription login, which is what DOGGIE is meant to use: the owner's,
// or with a token, the backup login and its own config directory.
// Nothing else of the CLI's own reaches it: started from a Claude Code
// session, DOGGIE would pass on that session's variables, and the CLI
// would take itself for part of it.
func childEnv(env []string, token, configDir string) []string {
	out := make([]string, 0, len(env)+2)
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		ownersLogin := name == "CLAUDE_CODE_OAUTH_TOKEN" || name == "CLAUDE_CONFIG_DIR"
		switch {
		case name == "ANTHROPIC_API_KEY" || name == "ANTHROPIC_AUTH_TOKEN":
			continue
		case strings.HasPrefix(name, "CLAUDE") && (token != "" || !ownersLogin):
			continue
		}
		out = append(out, kv)
	}
	if token != "" {
		out = append(out, "CLAUDE_CODE_OAUTH_TOKEN="+token, "CLAUDE_CONFIG_DIR="+configDir)
	}
	return out
}

func (p *Provider) writeMCPConfig(token string) (string, error) {
	cfg, err := json.Marshal(map[string]any{"mcpServers": map[string]any{mcpServer: map[string]any{
		"type":    "http",
		"url":     p.opts.Tools.URL(),
		"headers": map[string]string{"Authorization": "Bearer " + token},
	}}})
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(p.opts.Dir, "mcp-*.json")
	if err != nil {
		return "", fmt.Errorf("claudecode: mcp config: %w", err)
	}
	// CreateTemp makes the file 0600: only DOGGIE's user can read the token.
	_, err = f.Write(cfg)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("claudecode: mcp config: %w", err)
	}
	return f.Name(), nil
}

// read delivers stdout lines until EOF, then reaps the process.
func (pr *proc) read(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		pr.lines <- line
	}
	pr.readErr = sc.Err()
	if pr.readErr != nil {
		// A line too long: stop the process, it would block on a full pipe.
		pr.kill()
		io.Copy(io.Discard, stdout)
	}
	close(pr.lines)
	pr.waitErr = pr.cmd.Wait()
	close(pr.exited)
}

// alive reports whether the process is still running.
func (pr *proc) alive() bool {
	select {
	case <-pr.exited:
		return false
	default:
		return true
	}
}

// send writes the turn's input and closes stdin, so the CLI exits after the
// turn. It runs apart from the reader: a large input must not block reading.
func (pr *proc) send(input string) error {
	msg, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": input},
	})
	if err != nil {
		return err
	}
	_, err = pr.stdin.Write(append(msg, '\n'))
	if cerr := pr.stdin.Close(); err == nil {
		err = cerr
	}
	return err
}

// stop ends the process: it lets it exit on its own for a moment, then kills
// it, and releases its MCP client and config file. Safe to call many times.
func (pr *proc) stop(grace time.Duration) {
	pr.stopOnce.Do(func() {
		// Nobody reads the output anymore: drain it, or the reader would
		// block on a full channel and never reap the process.
		go func() {
			for range pr.lines {
			}
		}()
		pr.stdin.Close()
		select {
		case <-pr.exited:
		case <-time.After(grace):
			pr.kill()
			<-pr.exited
		}
		pr.cleanup()
	})
}

func (pr *proc) cleanup() {
	pr.client.Close()
	os.Remove(pr.cfgFile)
}

// exitError explains why the process ended without a result.
func (pr *proc) exitError() error {
	<-pr.exited
	msg := strings.TrimSpace(pr.stderr.String())
	switch {
	case pr.readErr != nil:
		return fmt.Errorf("claudecode: read output: %w", pr.readErr)
	case pr.waitErr != nil && msg != "":
		return fmt.Errorf("claudecode: claude exited (%v): %s", pr.waitErr, msg)
	case pr.waitErr != nil:
		return fmt.Errorf("claudecode: claude exited: %w", pr.waitErr)
	case msg != "":
		return fmt.Errorf("claudecode: claude exited without a result: %s", msg)
	}
	return errors.New("claudecode: claude exited without a result")
}

// tail keeps the last max bytes written to it.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(b), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
