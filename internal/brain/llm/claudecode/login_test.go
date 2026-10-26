//go:build unix

package claudecode

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/brain/llm"
)

const goodToken = "sk-ant-oat01-abcDEF_123-xyz"

func TestTokenIsKeptPrivately(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "claudecode")
	if _, err := LoadToken(dir); !errors.Is(err, ErrNoToken) {
		t.Fatalf("LoadToken without one = %v, want ErrNoToken", err)
	}
	if err := SaveToken(dir, "  "+goodToken+"\n"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, tokenFile))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file: %v, mode %v", err, fi.Mode().Perm())
	}
	if di, _ := os.Stat(dir); di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", di.Mode().Perm())
	}
	if got, err := LoadToken(dir); err != nil || got != goodToken {
		t.Errorf("LoadToken = %q, %v", got, err)
	}
	// Saving again replaces it, and leaves no temporary files.
	if err := SaveToken(dir, goodToken+"2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadToken(dir); got != goodToken+"2" {
		t.Errorf("LoadToken = %q", got)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".token-*")); len(left) != 0 {
		t.Errorf("left %v", left)
	}
	if err := RemoveToken(dir); err != nil {
		t.Fatal(err)
	}
	if err := RemoveToken(dir); err != nil {
		t.Errorf("removing twice: %v", err)
	}
	if _, err := LoadToken(dir); !errors.Is(err, ErrNoToken) {
		t.Errorf("after RemoveToken: %v", err)
	}
}

func TestBadTokensAreRefused(t *testing.T) {
	dir := t.TempDir()
	for _, tok := range []string{"", "   ", "abc", "sk-ant- oat", "sk-ant-oat\x00", "sk-ant-ñ", "sk-ant-" + strings.Repeat("x", maxToken)} {
		if err := SaveToken(dir, tok); err == nil {
			t.Errorf("SaveToken(%q) accepted", tok)
		}
	}
	// A file broken by hand is refused too, not passed to the CLI.
	os.WriteFile(filepath.Join(dir, tokenFile), []byte("sk-ant-a b\n"), 0o600)
	if _, err := LoadToken(dir); err == nil || errors.Is(err, ErrNoToken) {
		t.Errorf("LoadToken of a broken file = %v", err)
	}
}

func TestScrubAccount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if err := scrubAccount(dir); err != nil {
		t.Errorf("no file: %v", err)
	}
	os.WriteFile(path, []byte(`{"numStartups":3,"oauthAccount":{"emailAddress":"someone@example.com"},"theme":"dark"}`), 0o600)
	if !hasAccount(dir) {
		t.Fatal("hasAccount = false")
	}
	if err := scrubAccount(dir); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if bytes.Contains(b, []byte("someone@")) || !bytes.Contains(b, []byte(`"theme":"dark"`)) || !bytes.Contains(b, []byte(`"numStartups":3`)) {
		t.Errorf("after scrubbing: %s", b)
	}
	if hasAccount(dir) {
		t.Error("hasAccount after scrubbing")
	}
	for _, s := range []string{`{"oauthAccount":null}`, `{}`} {
		os.WriteFile(path, []byte(s), 0o600)
		if hasAccount(dir) {
			t.Errorf("hasAccount(%s)", s)
		}
	}
	os.WriteFile(path, []byte(`{not json`), 0o600)
	if err := scrubAccount(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an unreadable config was kept: %v", err)
	}
}

// loginOf tells, from a fake's report, which login a turn ran on.
func loginOf(t *testing.T, outs []llm.Output) string {
	t.Helper()
	for _, kv := range reportOf(t, outs).Env {
		if kv == "CLAUDE_CODE_OAUTH_TOKEN="+goodToken {
			return "backup"
		}
	}
	return "owner"
}

// Turns run on the owner's login, even with a backup one, and leave the
// owner's variables alone; the backup's turns get its token and config
// directory.
func TestTurnsRunOnTheOwnersLoginFirst(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/owner/.claude")
	o := testOptions(t)
	o.Token = goodToken
	p := newProvider(t, o)
	outs, done := collect(t, p, context.Background(), scriptOf(step{Report: true}))
	if done.Err != nil {
		t.Fatal(done.Err)
	}
	env := reportOf(t, outs).Env
	if !slices.Contains(env, "CLAUDE_CONFIG_DIR=/home/owner/.claude") || loginOf(t, outs) != "owner" {
		t.Errorf("owner's turn env %q", env)
	}

	p.mu.Lock()
	p.ownerSpentUntil = time.Now().Add(time.Hour)
	p.mu.Unlock()
	outs, done = collect(t, p, context.Background(), scriptOf(step{Report: true}))
	if done.Err != nil {
		t.Fatal(done.Err)
	}
	env = reportOf(t, outs).Env
	if !slices.Contains(env, "CLAUDE_CONFIG_DIR="+filepath.Join(o.Dir, "config")) || slices.Contains(env, "CLAUDE_CONFIG_DIR=/home/owner/.claude") || loginOf(t, outs) != "backup" {
		t.Errorf("backup's turn env %q", env)
	}
}

// Once the owner's limit is spent, the same turn runs again on the backup
// login, and so do the next ones until the owner's renews.
func TestBackupLoginTakesOverWhileTheOwnersIsSpent(t *testing.T) {
	o := testOptions(t)
	o.Token = goodToken
	o.Warm = 1
	p := newProvider(t, o)
	renews := time.Now().Add(time.Hour).Truncate(time.Second)
	outs, done := collect(t, p, context.Background(), scriptOf(step{Spent: "owner", Renews: renews.Unix()}, step{Report: true}, step{Text: "Hola"}))
	if done.Err != nil || len(outs) != 2 || outs[1].Text != "Hola" || loginOf(t, outs) != "backup" {
		t.Fatalf("outs = %+v, done = %+v", outs, done)
	}
	p.mu.Lock()
	until := p.ownerSpentUntil
	p.mu.Unlock()
	if !until.Equal(renews) {
		t.Errorf("owner spent until %v, want %v", until, renews)
	}
	// The next turn goes straight to the backup, warm process or not.
	outs, done = collect(t, p, context.Background(), scriptOf(step{Spent: "owner", Renews: renews.Unix()}, step{Report: true}))
	if done.Err != nil || loginOf(t, outs) != "backup" {
		t.Errorf("second turn: %v on %s", done.Err, loginOf(t, outs))
	}
	// Once it renews, turns are the owner's again.
	p.mu.Lock()
	p.ownerSpentUntil = time.Now().Add(-time.Second)
	p.mu.Unlock()
	outs, done = collect(t, p, context.Background(), scriptOf(step{Report: true}))
	if done.Err != nil || loginOf(t, outs) != "owner" {
		t.Errorf("after the renewal: %v on %s", done.Err, loginOf(t, outs))
	}
}

func TestSpentWithoutABackupOrOnBoth(t *testing.T) {
	soon, later := time.Now().Add(time.Hour).Truncate(time.Second), time.Now().Add(3*time.Hour).Truncate(time.Second)
	for _, tc := range []struct {
		name   string
		token  string
		script step
		want   time.Time
	}{
		{"no backup", "", step{Spent: "owner", Renews: later.Unix()}, later},
		{"both, owner first", goodToken, step{Spent: "both", Renews: soon.Unix(), BackupRenews: later.Unix()}, soon},
		{"both, backup first", goodToken, step{Spent: "both", Renews: later.Unix(), BackupRenews: soon.Unix()}, soon},
		{"both, backup unknown", goodToken, step{Spent: "both", Renews: later.Unix()}, later},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spawns := filepath.Join(t.TempDir(), "spawns")
			t.Setenv("DOGGIE_FAKE_INPUTLOG", spawns)
			o := testOptions(t)
			o.Token = tc.token
			p := newProvider(t, o)
			_, done := collect(t, p, context.Background(), scriptOf(tc.script))
			var spent *llm.UsageLimitError
			if !errors.As(done.Err, &spent) || !spent.Renews.Equal(tc.want) {
				t.Fatalf("Done.Err = %v, want a usage limit renewing at %v", done.Err, tc.want)
			}
			b, _ := os.ReadFile(spawns)
			if n, want := strings.Count(string(b), "\n"), map[bool]int{true: 1, false: 2}[tc.token == ""]; n != want {
				t.Errorf("ran %d turns, want %d", n, want)
			}
		})
	}
}

// A turn that already said something isn't run again elsewhere, and an
// unknown renewal counts as a while.
func TestNoBackupRerunAfterSayingSomething(t *testing.T) {
	spawns := filepath.Join(t.TempDir(), "spawns")
	t.Setenv("DOGGIE_FAKE_INPUTLOG", spawns)
	o := testOptions(t)
	o.Token = goodToken
	p := newProvider(t, o)
	outs, done := collect(t, p, context.Background(), scriptOf(step{Text: "Voy"}, step{Spent: "owner"}))
	var spent *llm.UsageLimitError
	if !errors.As(done.Err, &spent) || len(outs) != 1 || outs[0].Text != "Voy" {
		t.Fatalf("outs = %+v, done = %+v", outs, done)
	}
	if b, _ := os.ReadFile(spawns); strings.Count(string(b), "\n") != 1 {
		t.Errorf("ran the turn again: %q", b)
	}
	p.mu.Lock()
	left := time.Until(p.ownerSpentUntil)
	p.mu.Unlock()
	if left < unknownRenewal-time.Minute || left > unknownRenewal {
		t.Errorf("owner spent for %v more, want about %v", left, unknownRenewal)
	}
	// The next turn is the backup's.
	if outs, _ := collect(t, p, context.Background(), scriptOf(step{Report: true})); loginOf(t, outs) != "backup" {
		t.Error("the next turn wasn't the backup's")
	}
}

// An account profile the backup's CLI keeps is told about once, and cleared
// on the next start.
func TestAccountProfileIsClearedOnStart(t *testing.T) {
	var logs bytes.Buffer
	o := testOptions(t)
	o.Token = goodToken
	o.Log = slog.New(slog.NewTextHandler(&logs, nil))
	p := newProvider(t, o)
	p.mu.Lock()
	p.ownerSpentUntil = time.Now().Add(time.Hour)
	p.mu.Unlock()
	for range 2 {
		if _, done := collect(t, p, context.Background(), scriptOf(step{Account: true})); done.Err != nil {
			t.Fatal(done.Err)
		}
	}
	configDir := filepath.Join(o.Dir, "config")
	waitFor(t, "the warning", func() bool { return strings.Contains(logs.String(), "account's profile") })
	if n := strings.Count(logs.String(), "account's profile"); n != 1 {
		t.Errorf("warned %d times", n)
	}
	if !hasAccount(configDir) {
		t.Fatal("the fake kept no profile")
	}
	p.Close()
	newProvider(t, o)
	if hasAccount(configDir) {
		t.Error("the profile survived a new start")
	}
}

func TestLoginFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		login bool
	}{
		{"exit asking for /login", scriptOf(step{Stderr: "Invalid API key · Please run /login", Exit: intp(1)}), true},
		{"401", scriptOf(step{Raw: []byte(`{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"result":"Failed to authenticate"}`)}), true},
		{"expired", scriptOf(step{Error: "OAuth token has expired. Please obtain a new token"}), true},
		{"something else", scriptOf(step{Error: "overloaded"}), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newProvider(t, testOptions(t))
			_, done := collect(t, p, context.Background(), tc.input)
			if got := errors.Is(done.Err, llm.ErrNotLoggedIn); got != tc.login {
				t.Errorf("Done.Err = %v; not logged in: %v, want %v", done.Err, got, tc.login)
			}
		})
	}
}

func TestVerifyToken(t *testing.T) {
	t.Setenv("DOGGIE_FAKE_CLAUDE", "1")
	for _, tc := range []struct {
		name    string
		token   string
		script  string
		bad, ok bool
	}{
		{"works", goodToken, scriptOf(step{Text: "OK"}), false, true},
		{"refused", goodToken, scriptOf(step{Raw: []byte(`{"type":"result","subtype":"success","is_error":true,"api_error_status":401,"result":"Failed to authenticate. API Error: 401 OAuth access token is invalid."}`)}), true, false},
		{"exit asking for /login", goodToken, scriptOf(step{Stderr: "Invalid API key · Please run /login", Exit: intp(1)}), true, false},
		{"another failure", goodToken, scriptOf(step{Error: "overloaded"}), false, false},
		{"no result", goodToken, scriptOf(step{Exit: intp(2)}), false, false},
		{"not a token", "abc", scriptOf(step{Text: "OK"}), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			err := verifyToken(context.Background(), os.Args[0], "sonnet", dir, tc.token, tc.script)
			if (err == nil) != tc.ok || errors.Is(err, ErrBadToken) != tc.bad {
				t.Errorf("err = %v; want ok %v, bad token %v", err, tc.ok, tc.bad)
			}
			if left, _ := filepath.Glob(filepath.Join(dir, "verify-*")); len(left) != 0 {
				t.Errorf("left %v", left)
			}
		})
	}
}

// The token is tried as DOGGIE uses it: its own login and config directory.
func TestVerifyTokenUsesDOGGIEsLogin(t *testing.T) {
	t.Setenv("DOGGIE_FAKE_CLAUDE", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", "/home/owner/.claude")
	report := filepath.Join(t.TempDir(), "env")
	t.Setenv("DOGGIE_FAKE_ENVLOG", report)
	dir := t.TempDir()
	if err := verifyToken(context.Background(), os.Args[0], "sonnet", dir, goodToken, scriptOf(step{Text: "OK"})); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(report)
	for _, want := range []string{"CLAUDE_CODE_OAUTH_TOKEN=" + goodToken, "CLAUDE_CONFIG_DIR=" + filepath.Join(dir, "config")} {
		if !strings.Contains(string(b), want+"\n") {
			t.Errorf("env lacks %q:\n%s", want, b)
		}
	}
	if strings.Contains(string(b), "/home/owner") {
		t.Errorf("the owner's config reached it:\n%s", b)
	}
}

// A CLI that hangs is stopped with all it started.
func TestVerifyTokenTimesOut(t *testing.T) {
	t.Setenv("DOGGIE_FAKE_CLAUDE", "1")
	old := verifyTimeout
	verifyTimeout = 500 * time.Millisecond
	t.Cleanup(func() { verifyTimeout = old })
	report := filepath.Join(t.TempDir(), "child")
	t.Setenv("DOGGIE_FAKE_CHILDLOG", report)
	err := verifyToken(context.Background(), os.Args[0], "sonnet", t.TempDir(), goodToken, scriptOf(step{Child: true}, step{Hang: true}))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a timeout", err)
	}
	b, _ := os.ReadFile(report)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid == 0 {
		t.Fatal("the fake started no child")
	}
	waitFor(t, "the child to die", func() bool { return !processAlive(pid) })
}
