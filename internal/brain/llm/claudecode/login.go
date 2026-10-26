package claudecode

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The backup login is a long-lived token made by "claude setup-token",
// maybe of another account. It runs with a config directory of its own, so
// the CLI never reads that account's profile and its email can't reach the
// model.

// tokenFile is where the token is kept, in the provider's directory.
const tokenFile = "token"

// maxToken bounds a token's size; real ones are about a hundred bytes.
const maxToken = 4 << 10

// ErrNoToken means DOGGIE has no login of its own.
var ErrNoToken = errors.New("claudecode: no token of DOGGIE's own")

// SaveToken keeps token in dir, readable by DOGGIE's user only.
func SaveToken(dir, token string) error {
	token = strings.TrimSpace(token)
	if err := checkToken(token); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("claudecode: %w", err)
	}
	f, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return fmt.Errorf("claudecode: %w", err)
	}
	_, err = f.WriteString(token + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	// A rename, so a DOGGIE starting meanwhile reads the old token or the
	// new one, never half of one.
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, tokenFile))
	}
	if err != nil {
		os.Remove(f.Name())
		return fmt.Errorf("claudecode: save token: %w", err)
	}
	return nil
}

// LoadToken returns the token kept in dir, or ErrNoToken.
func LoadToken(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, tokenFile))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", ErrNoToken
	case err != nil:
		return "", fmt.Errorf("claudecode: read token: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if err := checkToken(token); err != nil {
		return "", err
	}
	return token, nil
}

// RemoveToken forgets DOGGIE's own login. It is not an error if there is
// none.
func RemoveToken(dir string) error {
	err := os.Remove(filepath.Join(dir, tokenFile))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("claudecode: %w", err)
	}
	return nil
}

func checkToken(token string) error {
	switch {
	case token == "":
		return errors.New("claudecode: empty token")
	case len(token) > maxToken:
		return errors.New("claudecode: token too long")
	case !strings.HasPrefix(token, "sk-ant-"):
		return errors.New(`claudecode: not a Claude token (they start with "sk-ant-")`)
	case strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r >= 0x7f }):
		return errors.New("claudecode: the token has spaces or odd characters")
	}
	return nil
}

// scrubAccount removes the account profile the CLI may have kept in its
// config file, which is where it takes the account's email from.
func scrubAccount(configDir string) error {
	path := filepath.Join(configDir, ".claude.json")
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var cfg map[string]json.RawMessage
	if err := json.Unmarshal(b, &cfg); err != nil {
		// Unreadable: start over, the CLI makes a new one.
		return os.Remove(path)
	}
	if _, ok := cfg["oauthAccount"]; !ok {
		return nil
	}
	delete(cfg, "oauthAccount")
	out, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}

// hasAccount reports whether the CLI kept an account profile in configDir.
func hasAccount(configDir string) bool {
	b, err := os.ReadFile(filepath.Join(configDir, ".claude.json"))
	if err != nil {
		return false
	}
	var cfg struct {
		Account json.RawMessage `json:"oauthAccount"`
	}
	return json.Unmarshal(b, &cfg) == nil && len(cfg.Account) > 0 && string(cfg.Account) != "null"
}

// needsLogin reports whether the CLI said, in msg, that it has no valid
// login: "Invalid API key · Please run /login", or an expired token.
func needsLogin(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "/login") || strings.Contains(m, "invalid api key") ||
		strings.Contains(m, "oauth token has expired") || strings.Contains(m, "token has been revoked")
}
