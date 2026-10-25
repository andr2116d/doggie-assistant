// Package supervisor runs DOGGIE's components and restarts them when they
// crash, so one failing component never takes the others down.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"time"
)

// Component is a long-running part of DOGGIE.
//
// Run must block until ctx is cancelled. Returning earlier, with or without an
// error, or panicking counts as a crash, and the component is restarted.
// Each run gets its own ctx, cancelled when that run ends, so subscriptions
// and goroutines tied to it are released before the restart. Dependencies
// (bus connection, config, ...) are passed to the component's constructor,
// not to Run, so a restarted component must reset any state it keeps.
type Component interface {
	Name() string
	Run(ctx context.Context) error
}

// Supervisor runs components and restarts the ones that crash.
type Supervisor struct {
	Log *slog.Logger
	// MinBackoff and MaxBackoff bound the delay before a restart. The delay
	// doubles on every consecutive crash.
	MinBackoff, MaxBackoff time.Duration
	// StableAfter is how long a component must run before its backoff resets.
	StableAfter time.Duration
	// ShutdownTimeout is how long Run waits for components to return once ctx
	// is cancelled.
	ShutdownTimeout time.Duration
}

// New returns a Supervisor with production defaults.
func New(log *slog.Logger) *Supervisor {
	return &Supervisor{
		Log:             log,
		MinBackoff:      time.Second,
		MaxBackoff:      30 * time.Second,
		StableAfter:     time.Minute,
		ShutdownTimeout: 10 * time.Second,
	}
}

// Run starts every component and blocks until ctx is cancelled and all of
// them have stopped. It returns an error naming the components that did not
// stop within ShutdownTimeout.
func (s *Supervisor) Run(ctx context.Context, components ...Component) error {
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		running = make(map[string]bool)
	)
	for _, c := range components {
		running[c.Name()] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.supervise(ctx, c)
			mu.Lock()
			delete(running, c.Name())
			mu.Unlock()
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
	}

	select {
	case <-done:
		return nil
	case <-time.After(s.ShutdownTimeout):
		mu.Lock()
		defer mu.Unlock()
		var stuck []string
		for name := range running {
			stuck = append(stuck, name)
		}
		slices.Sort(stuck)
		return fmt.Errorf("components did not stop within %s: %v", s.ShutdownTimeout, stuck)
	}
}

// supervise runs c until ctx is cancelled, restarting it after each crash.
func (s *Supervisor) supervise(ctx context.Context, c Component) {
	log := s.Log.With("component", c.Name())
	backoff := s.MinBackoff
	for {
		log.Debug("starting")
		started := time.Now()
		runCtx, cancelRun := context.WithCancel(ctx)
		err, stack := runSafely(runCtx, c)
		cancelRun()

		if ctx.Err() != nil {
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Warn("stopped with error", "err", err)
			} else {
				log.Debug("stopped")
			}
			return
		}

		if err == nil {
			err = errors.New("returned before shutdown")
		}
		if time.Since(started) >= s.StableAfter {
			backoff = s.MinBackoff
		}
		attrs := []any{"err", err, "restart_in", backoff}
		if stack != nil {
			attrs = append(attrs, "stack", string(stack))
		}
		log.Error("crashed", attrs...)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, s.MaxBackoff)
	}
}

// runSafely runs c and turns a panic into an error plus its stack trace.
func runSafely(ctx context.Context, c Component) (err error, stack []byte) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
			stack = debug.Stack()
		}
	}()
	return c.Run(ctx), nil
}
