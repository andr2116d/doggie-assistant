package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/config"
	"github.com/andr2116d/doggie-assistant/internal/supervisor"
)

// core is DOGGIE running in this process: the bus and the components.
type core struct {
	dataDir  string
	srv      *bus.Server
	announce *bus.Conn
	cancel   context.CancelFunc
	done     chan struct{}
	err      error
	// logFile, if set, is closed by whoever started the core.
	logFile *os.File
}

// handoverTimeout bounds the wait for a chat to give its DOGGIE up.
const handoverTimeout = 20 * time.Second

// startCore starts the bus and the selected components, as role (roleRun or
// roleChat). They run until Stop is called. An explicit DOGGIE asks a chat
// running one to hand it over, and waits for it.
func startCore(ctx context.Context, cfg config.Config, log *slog.Logger, selected []factory, role string) (*core, error) {
	unlock, err := takeLock(ctx, cfg.DataDir, role, log)
	if err != nil {
		return nil, err
	}
	os.Remove(stopFile(cfg.DataDir)) // running again: whatever stopped it is past
	srv, err := startBus(ctx, cfg, log)
	if err != nil {
		unlock()
		return nil, err
	}
	comps, closeConns, err := buildComponents(selected, cfg, log, srv)
	if err != nil {
		srv.Shutdown()
		unlock()
		return nil, err
	}
	announce, err := srv.Connect("doggie")
	if err != nil {
		closeConns()
		srv.Shutdown()
		unlock()
		return nil, err
	}
	// Its life is Stop's to end, not ctx's: Stop first says why.
	cctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	c := &core{dataDir: cfg.DataDir, srv: srv, announce: announce, cancel: cancel, done: make(chan struct{})}
	go watchSatellites(cctx, srv, satellitesFile(cfg), log, satelliteWatchInterval)
	log.Info("doggie starting", "version", version, "components", names(selected), "as", role)
	go func() {
		defer close(c.done)
		c.err = supervisor.New(log).Run(cctx, comps...)
		// Connections close before the server shuts down.
		announce.Close()
		closeConns()
		srv.Shutdown()
		unlock()
		log.Info("doggie stopped")
	}()
	return c, nil
}

// takeLock locks the data dir for role. An explicit DOGGIE that finds it held
// by a chat asks the chat to hand it over and waits for the lock.
func takeLock(ctx context.Context, dir, role string, log *slog.Logger) (unlock func(), err error) {
	unlock, err = lockDataDir(dir, role)
	var running *runningError
	if role != roleRun || !errors.As(err, &running) || running.role != roleChat {
		return unlock, err
	}
	log.Info("a chat runs DOGGIE: asking it to hand it over", "pid", running.pid)
	if err := askToHandOver(running.pid); err != nil {
		return nil, fmt.Errorf("%w; asking it to hand DOGGIE over: %v", running, err)
	}
	deadline := time.Now().Add(handoverTimeout)
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		unlock, err = lockDataDir(dir, role)
		if !errors.Is(err, errRunning) || time.Now().After(deadline) {
			return unlock, err
		}
	}
}

// Done is closed once DOGGIE has stopped.
func (c *core) Done() <-chan struct{} { return c.done }

// Stop tells why DOGGIE stops (an events.Stop* reason), stops it and waits
// until it has. The reason goes to the bus's clients, and to a file in the
// data dir, which chats on this machine read to decide whether to take over:
// the event may not reach them before the server closes. A DOGGIE that
// crashes or is killed leaves no reason.
func (c *core) Stop(reason string) error {
	select {
	case <-c.done:
	default:
		os.WriteFile(stopFile(c.dataDir), []byte(reason+"\n"), 0o600)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		bus.Publish(ctx, c.announce, events.SystemStopping, events.StoppingData{Reason: reason})
		cancel()
	}
	c.cancel()
	<-c.done
	return c.err
}

// maxLogBytes is when the log file starts over, keeping the previous one.
const maxLogBytes = 10 << 20

// openLog opens DOGGIE's log file in the data dir, for when the terminal
// shows the chat. A file past maxLogBytes is kept as doggie.log.1.
func openLog(cfg config.Config) (*os.File, string, error) {
	path := filepath.Join(cfg.DataDir, "doggie.log")
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, "", err
	}
	if st, err := os.Stat(path); err == nil && st.Size() > maxLogBytes {
		if err := os.Rename(path, path+".1"); err != nil {
			return nil, "", fmt.Errorf("log: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, "", fmt.Errorf("log: %w", err)
	}
	return f, path, nil
}

func stopFile(dataDir string) string { return filepath.Join(dataDir, "doggie.stopped") }

// stopReason is why the DOGGIE of dataDir last stopped, if it said so.
func stopReason(dataDir string) string {
	data, _ := os.ReadFile(stopFile(dataDir))
	return strings.TrimSpace(string(data))
}
