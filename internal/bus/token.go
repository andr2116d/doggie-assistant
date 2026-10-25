package bus

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// TokenEnv overrides the token file, e.g. for satellites on other machines.
const TokenEnv = "DOGGIE_BUS_TOKEN"

// LoadOrCreateToken returns the bus token from $DOGGIE_BUS_TOKEN or from the
// file at path. If neither exists, it generates a random token and writes it
// to path, readable only by the owner.
func LoadOrCreateToken(path string) (string, error) {
	tok, err := LoadToken(path)
	if !errors.Is(err, fs.ErrNotExist) {
		return tok, err
	}

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("bus: generate token: %w", err)
	}
	tok = hex.EncodeToString(b)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("bus: create token dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("bus: write token: %w", err)
	}
	return tok, nil
}

// LoadToken returns the bus token from $DOGGIE_BUS_TOKEN or from the file at
// path, without creating it. A missing file yields an fs.ErrNotExist error.
func LoadToken(path string) (string, error) {
	if tok := os.Getenv(TokenEnv); tok != "" {
		return tok, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("bus: read token: %w", err)
	}
	tok := strings.TrimSpace(string(data))
	if tok == "" {
		return "", fmt.Errorf("bus: token file %s is empty", path)
	}
	return tok, nil
}
