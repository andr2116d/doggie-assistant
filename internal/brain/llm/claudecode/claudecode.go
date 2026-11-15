// Package claudecode is the LLM provider that drives the official Claude Code
// CLI in headless mode (claude -p, stream-json), with the owner's
// subscription login.
//
// DOGGIE builds the whole context of a turn, and a process is never the
// record of a conversation: any turn can run on a new one. A turn that
// tells what is new since the last one leaves its process to the thread's
// next turn, which then sends only that; other processes serve one turn.
// To hide the CLI's startup, warm processes are started ahead: they
// connect to DOGGIE's tools and wait for their input, without calling the
// model. Their tools only act once a turn binds them.
package claudecode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/brain/llm"
	"github.com/andr2116d/doggie-assistant/internal/mcp"
)

// ErrClosed is returned by Generate once the provider is closed.
var ErrClosed = errors.New("claudecode: provider is closed")

// Tools is DOGGIE's MCP server, as the provider sees it.
type Tools interface {
	URL() string
	NewClient() (*mcp.Client, error)
}

// Options configures the provider.
type Options struct {
	// Command is the claude executable.
	Command string
	// Model is the model name or alias, e.g. "sonnet".
	Model string
	// System is the system prompt. It is fixed for the provider's life, so
	// warm processes can start before their turn.
	System string
	// TurnTimeout bounds one turn. Zero means 90s.
	TurnTimeout time.Duration
	// Dir is a private directory the provider owns: it keeps the system
	// prompt, the per-process MCP configs and the CLI's empty working
	// directory there.
	Dir   string
	Tools Tools
	Log   *slog.Logger
	// Warm is how many processes are kept started ahead of their turn.
	Warm int
	// MaxWarmAge is how long a warm process may wait before it is replaced
	// by a fresh one. Zero means 10 minutes.
	MaxWarmAge time.Duration
	// Token is a backup login (see SaveToken), maybe of another account.
	// Turns run on the owner's claude login, and on this one only while the
	// owner's usage limit is spent. Empty means there is none.
	Token string
}

// Provider runs turns on claude processes.
type Provider struct {
	opts       Options
	systemFile string
	cwd        string
	// configDir is the CLI's own config directory, with Token.
	configDir string
	// accountSeen is set once the CLI was found keeping an account
	// profile in configDir, so it is told only once.
	accountSeen atomic.Bool

	mu     sync.Mutex
	closed bool
	// ownerSpentUntil is until when the owner's login has its usage limit
	// spent, so turns run on the backup login.
	ownerSpentUntil time.Time
	done            chan struct{} // closed by Close
	warm            []*proc
	epochs          map[string]*proc // by thread, see epoch.go
	procs           map[*proc]bool   // every running process, to kill on Close
	spawning        int
	wg              sync.WaitGroup // turns, warm-up spawns and warm watchers
}

// New prepares the provider's directory and starts the warm processes.
func New(opts Options) (*Provider, error) {
	switch {
	case opts.Command == "":
		return nil, errors.New("claudecode: no command")
	case opts.Model == "" || strings.HasPrefix(opts.Model, "-"):
		return nil, fmt.Errorf("claudecode: invalid model %q", opts.Model)
	case opts.Tools == nil:
		return nil, errors.New("claudecode: no tools")
	case opts.Dir == "":
		return nil, errors.New("claudecode: no directory")
	case opts.Warm < 0 || opts.TurnTimeout < 0 || opts.MaxWarmAge < 0:
		return nil, errors.New("claudecode: negative option")
	}
	if opts.TurnTimeout == 0 {
		opts.TurnTimeout = 90 * time.Second
	}
	if opts.MaxWarmAge == 0 {
		opts.MaxWarmAge = 10 * time.Minute
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	p := &Provider{
		opts:       opts,
		systemFile: filepath.Join(opts.Dir, "system.md"),
		cwd:        filepath.Join(opts.Dir, "cwd"),
		configDir:  filepath.Join(opts.Dir, "config"),
		done:       make(chan struct{}),
		procs:      make(map[*proc]bool),
		epochs:     make(map[string]*proc),
	}
	if err := p.prepareDir(); err != nil {
		return nil, err
	}
	p.fill()
	return p, nil
}

// prepareDir makes the private directory, removes what a previous run left
// (its MCP configs hold revoked tokens) and writes the system prompt. The
// CLI's working directory must be empty: no project files may reach the
// model.
func (p *Provider) prepareDir() error {
	if err := os.MkdirAll(p.opts.Dir, 0o700); err != nil {
		return fmt.Errorf("claudecode: %w", err)
	}
	if err := os.Chmod(p.opts.Dir, 0o700); err != nil {
		return fmt.Errorf("claudecode: %w", err)
	}
	stale, _ := filepath.Glob(filepath.Join(p.opts.Dir, "mcp-*.json"))
	for _, f := range stale {
		os.Remove(f)
	}
	if err := os.RemoveAll(p.cwd); err != nil {
		return fmt.Errorf("claudecode: %w", err)
	}
	if err := os.Mkdir(p.cwd, 0o700); err != nil {
		return fmt.Errorf("claudecode: %w", err)
	}
	os.Remove(p.systemFile) // WriteFile keeps the mode of an existing file
	if err := os.WriteFile(p.systemFile, []byte(p.opts.System), 0o600); err != nil {
		return fmt.Errorf("claudecode: system prompt: %w", err)
	}
	if p.opts.Token == "" {
		return nil
	}
	if err := os.MkdirAll(p.configDir, 0o700); err != nil {
		return fmt.Errorf("claudecode: %w", err)
	}
	// No process runs yet, so nothing writes the file meanwhile.
	if err := scrubAccount(p.configDir); err != nil {
		return fmt.Errorf("claudecode: clear the CLI's account profile: %w", err)
	}
	return nil
}

// Name implements llm.Brain.
func (p *Provider) Name() string { return "claudecode" }

// Generate implements llm.Brain.
func (p *Provider) Generate(ctx context.Context, req llm.Request) (<-chan llm.Output, error) {
	input := req.Input
	pr := (*proc)(nil)
	if req.Continue != "" {
		pr = p.takeEpoch(req.Turn.ThreadID)
	}
	if pr != nil {
		input = req.Continue
	} else {
		var err error
		if pr, err = p.take(); err != nil {
			return nil, err
		}
		p.fill()
	}
	out := make(chan llm.Output, 16)
	go func() {
		defer p.wg.Done()
		defer close(out)
		done := llm.Output{Done: p.runTurn(ctx, pr, req, input, out)}
		select {
		case out <- done:
			return
		case <-ctx.Done():
		case <-p.done:
		}
		// Cancelled or closing: the caller may have stopped reading, so
		// only wait a moment.
		select {
		case out <- done:
		case <-time.After(time.Second):
		}
	}()
	return out, nil
}

// take returns a process for a turn: a warm one if there is a usable one,
// else a new one. The caller owns a wg slot until the turn ends.
func (p *Provider) take() (*proc, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	p.wg.Add(1)
	backup := p.backupLocked()
	for len(p.warm) > 0 {
		pr := p.warm[0]
		p.warm = p.warm[1:]
		close(pr.taken)
		if pr.alive() && pr.backup == backup && time.Since(pr.started) < p.opts.MaxWarmAge {
			p.mu.Unlock()
			return pr, nil
		}
		p.discardLater(pr)
	}
	p.mu.Unlock()

	pr, err := p.spawnTracked(backup)
	if err != nil {
		p.wg.Done()
		return nil, err
	}
	return pr, nil
}

// spawnTracked starts a process and records it, unless the provider closed
// meanwhile.
func (p *Provider) spawnTracked(backup bool) (*proc, error) {
	pr, err := p.spawn(backup)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	closed := p.closed
	if !closed {
		p.procs[pr] = true
	}
	p.mu.Unlock()
	if closed {
		pr.stop(0)
		return nil, ErrClosed
	}
	return pr, nil
}

// fill starts warm processes until there are opts.Warm of them.
func (p *Provider) fill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	need := p.opts.Warm - len(p.warm) - p.spawning
	if p.closed || need <= 0 {
		return
	}
	p.spawning += need
	p.wg.Add(need)
	for range need {
		go p.warmUp()
	}
}

func (p *Provider) warmUp() {
	defer p.wg.Done()
	p.mu.Lock()
	backup := p.backupLocked()
	p.mu.Unlock()
	pr, err := p.spawn(backup)
	p.mu.Lock()
	p.spawning--
	if err != nil {
		p.mu.Unlock()
		// Not retried here: a broken CLI would make it loop. The next turn
		// starts its own process and tries again.
		p.opts.Log.Warn("claudecode: could not start a warm process", "err", err)
		return
	}
	if p.closed {
		p.mu.Unlock()
		pr.stop(0)
		return
	}
	pr.taken = make(chan struct{})
	p.procs[pr] = true
	p.warm = append(p.warm, pr)
	p.wg.Add(1)
	p.mu.Unlock()
	go p.watch(pr)
}

// watch retires a warm process that dies or waits too long.
func (p *Provider) watch(pr *proc) {
	defer p.wg.Done()
	timer := time.NewTimer(p.opts.MaxWarmAge)
	defer timer.Stop()
	select {
	case <-pr.taken:
	case <-p.done:
	case <-pr.exited:
		if p.dropWarm(pr) {
			p.opts.Log.Warn("claudecode: warm process exited", "err", pr.exitError())
		}
	case <-timer.C:
		if p.dropWarm(pr) {
			p.fill()
		}
	}
}

// dropWarm removes pr from the warm processes and stops it. It reports
// false if pr was taken for a turn first.
func (p *Provider) dropWarm(pr *proc) bool {
	p.mu.Lock()
	i := -1
	for j, w := range p.warm {
		if w == pr {
			i = j
		}
	}
	if i < 0 {
		p.mu.Unlock()
		return false
	}
	p.warm = append(p.warm[:i], p.warm[i+1:]...)
	p.mu.Unlock()
	p.discard(pr)
	return true
}

func (p *Provider) discard(pr *proc) {
	pr.stop(0)
	p.mu.Lock()
	delete(p.procs, pr)
	p.mu.Unlock()
	// The profile holds the account's email; it is cleared on the next start.
	if p.opts.Token != "" && !p.accountSeen.Load() && hasAccount(p.configDir) && p.accountSeen.CompareAndSwap(false, true) {
		p.opts.Log.Warn("claudecode: the CLI kept the account's profile in its config; it is cleared when DOGGIE starts again")
	}
}

// runTurn serves req on pr, sending it input: req.Input, or req.Continue
// on a process that holds the thread's conversation. If the process failed
// before the turn really started (it died first, e.g. a process that
// exited just as it was taken, or it could not reach DOGGIE's tools), it
// retries once on a fresh process. If the owner's usage limit is spent and
// there is a backup login, it runs the turn again there: the limit stops a
// turn before the model acts, so nothing can happen twice. A fresh process
// always gets req.Input.
func (p *Provider) runTurn(ctx context.Context, pr *proc, req llm.Request, input string, out chan<- llm.Output) *llm.Done {
	// One deadline for the whole turn, retries included.
	tctx, cancel := context.WithTimeout(ctx, p.opts.TurnTimeout)
	defer cancel()
	d, retry := p.attempt(ctx, tctx, pr, req, input, out)
	if retry && tctx.Err() == nil {
		p.opts.Log.Warn("claudecode: claude failed before the turn started, retrying on a new process", "err", d.Err)
		next, err := p.spawnTracked(pr.backup)
		if err != nil {
			return &llm.Done{Err: err}
		}
		pr = next
		d, _ = p.attempt(ctx, tctx, pr, req, req.Input, out)
	}
	var spent *llm.UsageLimitError
	if !errors.As(d.Err, &spent) {
		return d
	}
	if !pr.backup && p.ownerSpent(spent.Renews) && !pr.said && tctx.Err() == nil {
		p.opts.Log.Info("claudecode: the usage limit of the owner's login is spent; turns run on the backup login meanwhile", "renews", spent.Renews)
		next, err := p.spawnTracked(true)
		if err != nil {
			return &llm.Done{Err: err}
		}
		pr = next
		if d, _ = p.attempt(ctx, tctx, pr, req, req.Input, out); !errors.As(d.Err, &spent) {
			return d
		}
	}
	if pr.backup {
		// Spent on both: DOGGIE can think again when the first renews.
		spent.Renews = p.firstRenewal(spent.Renews)
	}
	return d
}

// backupLocked reports whether turns run on the backup login now. The
// caller holds p.mu.
func (p *Provider) backupLocked() bool {
	return p.opts.Token != "" && time.Now().Before(p.ownerSpentUntil)
}

// unknownRenewal is how long the owner's login counts as spent when the
// CLI doesn't say when it renews.
const unknownRenewal = 30 * time.Minute

// ownerSpent records that the owner's usage limit is spent until renews,
// and reports whether there is a backup login to run on meanwhile.
func (p *Provider) ownerSpent(renews time.Time) bool {
	if p.opts.Token == "" {
		return false
	}
	now := time.Now()
	if !renews.After(now) {
		renews = now.Add(unknownRenewal)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if renews.After(p.ownerSpentUntil) {
		p.ownerSpentUntil = renews
	}
	return true
}

// firstRenewal is the earlier of backup, when the backup login's limit
// renews (zero if unknown), and when the owner's does.
func (p *Provider) firstRenewal(backup time.Time) time.Time {
	p.mu.Lock()
	owner := p.ownerSpentUntil
	p.mu.Unlock()
	switch {
	case !owner.After(time.Now()):
		return backup
	case backup.IsZero() || owner.Before(backup):
		return owner
	}
	return backup
}

func (p *Provider) isClosed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Close kills every process, waits for running turns to end and returns.
// Turns in progress end with ErrClosed.
func (p *Provider) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	close(p.done)
	procs := make([]*proc, 0, len(p.procs))
	for pr := range p.procs {
		procs = append(procs, pr)
	}
	p.warm = nil
	clear(p.epochs)
	p.mu.Unlock()

	for _, pr := range procs {
		pr.kill()
	}
	p.wg.Wait()
	for _, pr := range procs {
		pr.stop(0)
	}
	p.mu.Lock()
	clear(p.procs)
	p.mu.Unlock()
}
