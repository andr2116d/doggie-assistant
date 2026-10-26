package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrBadToken means the CLI refused a token: it may have been cut or
// mistyped when copied.
var ErrBadToken = errors.New("claudecode: the token doesn't work; it may have been cut when copied")

// verifyTimeout bounds the turn that tries a token. A variable so tests
// can shrink it.
var verifyTimeout = 90 * time.Second

// VerifyToken runs one short turn of model with token, to tell a token that
// works from one that doesn't before DOGGIE keeps it. It spends a little of
// the usage limit. dir is the provider's directory.
func VerifyToken(ctx context.Context, command, model, dir, token string) error {
	return verifyToken(ctx, command, model, dir, token, "Reply with the word OK.")
}

func verifyToken(ctx context.Context, command, model, dir, token, input string) error {
	if err := checkToken(token); err != nil {
		return err
	}
	configDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return fmt.Errorf("claudecode: %w", err)
	}
	// Its own working directory: a running DOGGIE clears the provider's.
	cwd, err := os.MkdirTemp(dir, "verify-")
	if err != nil {
		return fmt.Errorf("claudecode: %w", err)
	}
	defer os.RemoveAll(cwd)
	mcpConfig := filepath.Join(cwd, "mcp.json")
	if err := os.WriteFile(mcpConfig, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		return fmt.Errorf("claudecode: %w", err)
	}
	msg, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": input}})

	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command, "-p",
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--tools", "", "--strict-mcp-config", "--mcp-config", mcpConfig,
		"--setting-sources", "", "--no-session-persistence", "--disable-slash-commands",
		"--model", model)
	cmd.Dir = cwd
	// The whole group, as for turns: the CLI may have started processes of
	// its own, which would keep its output open.
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = time.Second
	cmd.Env = childEnv(os.Environ(), token, configDir)
	cmd.Stdin = strings.NewReader(string(msg) + "\n")
	stderr := &tail{max: stderrTail}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("claudecode: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("claudecode: start %s: %w", command, err)
	}
	failure, done := "", false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		var ev event
		if json.Unmarshal(sc.Bytes(), &ev) != nil || ev.Type != "result" {
			continue
		}
		done = true
		failure = ev.failure()
		if failure != "" && (ev.apiStatus() == 401 || needsLogin(failure)) {
			failure = "login: " + failure
		}
	}
	waitErr := cmd.Wait()
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("claudecode: trying the token: %w", ctx.Err())
	case strings.HasPrefix(failure, "login: "):
		return fmt.Errorf("%w (%s)", ErrBadToken, strings.TrimPrefix(failure, "login: "))
	case failure != "":
		return fmt.Errorf("claudecode: trying the token: %s", failure)
	case done:
		return nil
	case needsLogin(stderr.String()):
		return fmt.Errorf("%w (%s)", ErrBadToken, strings.TrimSpace(stderr.String()))
	}
	return fmt.Errorf("claudecode: trying the token: claude exited without a result (%v): %s", waitErr, strings.TrimSpace(stderr.String()))
}
