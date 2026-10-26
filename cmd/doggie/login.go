package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/term"

	"github.com/andr2116d/doggie-assistant/internal/brain/llm/claudecode"
	"github.com/andr2116d/doggie-assistant/internal/config"
)

const loginUsage = `Usage:
  doggie login [flags]    give DOGGIE a backup Claude login
  doggie logout [flags]   forget it

DOGGIE thinks with your claude login. With a backup login, maybe of
another account, it goes on when your usage limit is spent, and comes
back to yours once it renews. doggie login runs "claude setup-token",
which opens the browser (log in there with the account to use), and
asks for the token it printed. DOGGIE uses it from its next start.

Flags:
`

// claudeDir is where the Claude Code provider keeps its files.
func claudeDir(cfg config.Config) string {
	return filepath.Join(cfg.DataDir, "brain", "claudecode")
}

// loginDeps are what doggie login runs outside DOGGIE, apart for tests.
type loginDeps struct {
	// setupToken runs "claude setup-token" in the terminal.
	setupToken func(command string) error
	// verify tries a token with one short turn.
	verify func(command, model, dir, token string) error
}

var realLogin = loginDeps{
	setupToken: runSetupToken,
	verify: func(command, model, dir, token string) error {
		// The CLI runs in a process group of its own, out of reach of the
		// terminal's Ctrl+C: cancelling stops it.
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return claudecode.VerifyToken(ctx, command, model, dir, token)
	},
}

// loginCmd runs doggie login or logout.
func loginCmd(name string, args []string, stdin *os.File, stdout, stderr io.Writer, deps loginDeps) error {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprint(stderr, loginUsage)
		flags.PrintDefaults()
	}
	configPath := flags.String("config", "", "config file (default $XDG_CONFIG_HOME/doggie/config.yaml)")
	paste := false
	if name == "login" {
		flags.BoolVar(&paste, "paste", false, `don't run "claude setup-token": just ask for a token made before`)
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 0 {
		return fmt.Errorf("%s takes no arguments", name)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	dir := claudeDir(cfg)

	if name == "logout" {
		if err := claudecode.RemoveToken(dir); err != nil {
			return err
		}
		fmt.Fprintln(stdout, "DOGGIE forgot its backup login, from its next start.")
		return nil
	}
	command := cfg.Brain.ClaudeCode.Command
	if !paste {
		if err := deps.setupToken(command); err != nil {
			return fmt.Errorf("claude setup-token: %w", err)
		}
	}
	token, err := readToken(stdin, stderr)
	if err != nil {
		return err
	}
	fmt.Fprintln(stderr, "Trying the token...")
	if err := deps.verify(command, cfg.Brain.Model, dir, token); err != nil {
		if errors.Is(err, claudecode.ErrBadToken) {
			return fmt.Errorf(`%w. Nothing was kept; copy it again, all of it (a long token may wrap onto several lines), and run "doggie login -paste"`, err)
		}
		return fmt.Errorf("%w. Nothing was kept", err)
	}
	if err := claudecode.SaveToken(dir, token); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "The token works. From its next start, DOGGIE uses it when your claude login's usage limit is spent.")
	return nil
}

// maxTokenLines bounds how many lines a pasted token may take.
const maxTokenLines = 8

// readToken asks for the token, unseen in a terminal. A long token may come
// in several lines, as the terminal wrapped it when it was copied: they are
// joined until an empty line, or the end of the input.
func readToken(stdin *os.File, stderr io.Writer) (string, error) {
	var lines []string
	if fd := int(stdin.Fd()); term.IsTerminal(fd) {
		// Ctrl+C would leave the terminal without echo: give it back first.
		if state, err := term.GetState(fd); err == nil {
			interrupt := make(chan os.Signal, 1)
			signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)
			done := make(chan struct{})
			defer func() { signal.Stop(interrupt); close(done) }()
			go func() {
				select {
				case <-interrupt:
					term.Restore(fd, state)
					fmt.Fprintln(stderr)
					os.Exit(130)
				case <-done:
				}
			}()
		}
		fmt.Fprint(stderr, "Paste the token, then press Enter on an empty line (it won't show): ")
		for len(lines) < maxTokenLines {
			b, err := term.ReadPassword(fd)
			if err != nil {
				fmt.Fprintln(stderr)
				return "", fmt.Errorf("read the token: %w", err)
			}
			line := cleanTokenLine(string(b))
			if line == "" && len(lines) > 0 {
				break
			}
			if line != "" {
				lines = append(lines, line)
			}
		}
		fmt.Fprintln(stderr)
	} else {
		sc := bufio.NewScanner(io.LimitReader(stdin, 64<<10))
		for sc.Scan() && len(lines) < maxTokenLines {
			line := cleanTokenLine(sc.Text())
			if line == "" && len(lines) > 0 {
				break
			}
			if line != "" {
				lines = append(lines, line)
			}
		}
		if err := sc.Err(); err != nil {
			return "", fmt.Errorf("read the token: %w", err)
		}
	}
	return strings.Join(lines, ""), nil
}

// cleanTokenLine drops what a terminal adds to pasted text: the bracketed
// paste marks and spaces.
func cleanTokenLine(s string) string {
	s = strings.NewReplacer("\x1b[200~", "", "\x1b[201~", "").Replace(s)
	return strings.Join(strings.Fields(s), "")
}

// runSetupToken runs "claude setup-token" in this terminal.
func runSetupToken(command string) error {
	cmd := exec.Command(command, "setup-token")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
