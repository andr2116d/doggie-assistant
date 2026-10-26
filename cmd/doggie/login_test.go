package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andr2116d/doggie-assistant/internal/brain/llm/claudecode"
)

// loginEnv points the config at a temporary data directory and returns
// where the provider's files go.
func loginEnv(t *testing.T) (configPath, dir string) {
	t.Helper()
	data := t.TempDir()
	configPath = filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("XDG_DATA_HOME", data)
	if err := os.WriteFile(configPath, []byte("language: es\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, filepath.Join(data, "doggie", "brain", "claudecode")
}

// pipeWith returns a stdin that reads text, as when the token is piped.
func pipeWith(t *testing.T, text string) *os.File {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString(text)
	w.Close()
	t.Cleanup(func() { r.Close() })
	return r
}

// fakeLogin records what login ran, and refuses the tokens in bad.
type fakeLogin struct {
	setups   []string
	verified []string
	bad      map[string]bool
}

func (f *fakeLogin) deps() loginDeps {
	return loginDeps{
		setupToken: func(command string) error { f.setups = append(f.setups, command); return nil },
		verify: func(command, model, dir, token string) error {
			f.verified = append(f.verified, fmt.Sprintf("%s %s %s", command, model, token))
			if f.bad[token] {
				return fmt.Errorf("%w (401)", claudecode.ErrBadToken)
			}
			return nil
		},
	}
}

func TestLoginRunsSetupTokenAndKeepsAWorkingToken(t *testing.T) {
	configPath, dir := loginEnv(t)
	f := &fakeLogin{}
	var out, errOut bytes.Buffer
	if err := loginCmd("login", []string{"-config", configPath}, pipeWith(t, "  sk-ant-oat01-abc  \n"), &out, &errOut, f.deps()); err != nil {
		t.Fatal(err)
	}
	if len(f.setups) != 1 || f.setups[0] != "claude" || len(f.verified) != 1 || f.verified[0] != "claude sonnet sk-ant-oat01-abc" {
		t.Errorf("setups %q, verified %q", f.setups, f.verified)
	}
	if tok, err := claudecode.LoadToken(dir); err != nil || tok != "sk-ant-oat01-abc" {
		t.Errorf("token = %q, %v", tok, err)
	}
	if !strings.Contains(out.String(), "works") || strings.Contains(out.String()+errOut.String(), "sk-ant-oat01-abc") {
		t.Errorf("printed %q / %q", out.String(), errOut.String())
	}
	if err := loginCmd("logout", []string{"-config", configPath}, pipeWith(t, ""), &out, &errOut, f.deps()); err != nil {
		t.Fatal(err)
	}
	if _, err := claudecode.LoadToken(dir); !errors.Is(err, claudecode.ErrNoToken) {
		t.Errorf("after logout: %v", err)
	}
}

// A long token copied from a narrow terminal comes in several lines, and a
// terminal may mark what was pasted.
func TestLoginJoinsAWrappedToken(t *testing.T) {
	for name, pasted := range map[string]string{
		"wrapped":       "sk-ant-oat01-abc\nDEF\n\nleft over\n",
		"marked":        "\x1b[200~sk-ant-oat01-abc\nDEF\x1b[201~\n",
		"spaces inside": "sk-ant-oat01-abc DEF\n",
		"blank first":   "\n\nsk-ant-oat01-abcDEF",
	} {
		t.Run(name, func(t *testing.T) {
			configPath, dir := loginEnv(t)
			f := &fakeLogin{}
			var out bytes.Buffer
			if err := loginCmd("login", []string{"-config", configPath, "-paste"}, pipeWith(t, pasted), &out, &out, f.deps()); err != nil {
				t.Fatal(err)
			}
			if tok, _ := claudecode.LoadToken(dir); tok != "sk-ant-oat01-abcDEF" || len(f.setups) != 0 {
				t.Errorf("token = %q, setups %q", tok, f.setups)
			}
		})
	}
}

func TestLoginKeepsNothingThatDoesntWork(t *testing.T) {
	configPath, dir := loginEnv(t)
	f := &fakeLogin{bad: map[string]bool{"sk-ant-oat01-cut": true}}
	var out bytes.Buffer
	err := loginCmd("login", []string{"-config", configPath, "-paste"}, pipeWith(t, "sk-ant-oat01-cut\n"), &out, &out, f.deps())
	if err == nil || !strings.Contains(err.Error(), "Nothing was kept") || !strings.Contains(err.Error(), "several lines") {
		t.Errorf("err = %v", err)
	}
	if _, err := claudecode.LoadToken(dir); !errors.Is(err, claudecode.ErrNoToken) {
		t.Errorf("kept something: %v", err)
	}
	// A failed setup-token keeps nothing either.
	failing := f.deps()
	failing.setupToken = func(string) error { return errors.New("cancelled") }
	if err := loginCmd("login", []string{"-config", configPath}, pipeWith(t, "sk-ant-oat01-abc\n"), &out, &out, failing); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Errorf("err = %v", err)
	}
	if _, err := claudecode.LoadToken(dir); !errors.Is(err, claudecode.ErrNoToken) {
		t.Errorf("kept something: %v", err)
	}
	if err := loginCmd("login", []string{"-config", configPath, "extra"}, pipeWith(t, ""), &out, &out, f.deps()); err == nil {
		t.Error("accepted an argument")
	}
	if err := loginCmd("logout", []string{"-config", configPath, "-paste"}, pipeWith(t, ""), &out, &out, f.deps()); err == nil {
		t.Error("logout accepted -paste")
	}
}
