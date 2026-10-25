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
// to path, readable only by the owner. Processes creating it at once all get
// the same token.
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
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("bus: create token dir: %w", err)
	}
	// Write it whole elsewhere, then link it in place: the link fails if
	// another process got there first, and nobody reads half a token.
	tmp, err := os.CreateTemp(dir, ".bus.token-*")
	if err != nil {
		return "", fmt.Errorf("bus: write token: %w", err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.WriteString(tok + "\n")
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("bus: write token: %w", err)
	}
	err = os.Link(tmp.Name(), path)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		// No hard links on this file system: create it exclusively instead.
		err = writeNew(path, tok)
	}
	if errors.Is(err, fs.ErrExist) {
		return LoadToken(path)
	}
	if err != nil {
		return "", fmt.Errorf("bus: write token: %w", err)
	}
	return tok, nil
}

// writeNew writes tok to path, failing with fs.ErrExist if path exists.
func writeNew(path, tok string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(tok + "\n")
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
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
