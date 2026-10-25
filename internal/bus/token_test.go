package bus

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestLoadOrCreateToken(t *testing.T) {
	t.Setenv(TokenEnv, "")
	path := filepath.Join(t.TempDir(), "sub", "bus.token")

	first, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 64 {
		t.Errorf("token length = %d, want 64 hex chars", len(first))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %o, want 600", perm)
	}

	second, err := LoadOrCreateToken(path)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Error("token changed between calls")
	}
}

func TestTokenEnvOverridesFile(t *testing.T) {
	t.Setenv(TokenEnv, "from-env")
	tok, err := LoadOrCreateToken(filepath.Join(t.TempDir(), "bus.token"))
	if err != nil || tok != "from-env" {
		t.Fatalf("got %q, %v; want from-env", tok, err)
	}
}

func TestLoadTokenMissing(t *testing.T) {
	t.Setenv(TokenEnv, "")
	_, err := LoadToken(filepath.Join(t.TempDir(), "nope"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

// Processes starting at once on a fresh data dir all end up with the one
// token in the file, or their clients can't connect.
func TestLoadOrCreateTokenConcurrently(t *testing.T) {
	t.Setenv(TokenEnv, "")
	for round := range 20 {
		path := filepath.Join(t.TempDir(), "doggie", "bus.token")
		tokens := make(chan string, 20)
		var wg sync.WaitGroup
		for range 20 {
			wg.Go(func() {
				tok, err := LoadOrCreateToken(path)
				if err != nil {
					t.Error(err)
				}
				tokens <- tok
			})
		}
		wg.Wait()
		close(tokens)
		file, err := LoadToken(path)
		if err != nil {
			t.Fatal(err)
		}
		for tok := range tokens {
			if tok != file {
				t.Fatalf("round %d: a process got %.8s…, the file holds %.8s…", round, tok, file)
			}
		}
		entries, _ := os.ReadDir(filepath.Dir(path))
		if len(entries) != 1 {
			t.Errorf("round %d: %d files left in the dir", round, len(entries))
		}
	}
}
