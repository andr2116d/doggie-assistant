package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/config"
	"github.com/andr2116d/doggie-assistant/internal/supervisor"
)

// core is DOGGIE running in this process: the bus and the components.
type core struct {
	srv    *bus.Server
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	// logFile, if set, is closed by whoever started the core.
	logFile *os.File
}

// startCore starts the bus and the selected components. They run until ctx
// is done or Stop is called.
func startCore(ctx context.Context, cfg config.Config, log *slog.Logger, selected []factory) (*core, error) {
	unlock, err := lockDataDir(cfg.DataDir)
	if err != nil {
		return nil, err
	}
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
	cctx, cancel := context.WithCancel(ctx)
	c := &core{srv: srv, cancel: cancel, done: make(chan struct{})}
	go watchSatellites(cctx, srv, satellitesFile(cfg), log, satelliteWatchInterval)
	log.Info("doggie starting", "version", version, "components", names(selected))
	go func() {
		defer close(c.done)
		c.err = supervisor.New(log).Run(cctx, comps...)
		// Connections close before the server shuts down.
		closeConns()
		srv.Shutdown()
		unlock()
		log.Info("doggie stopped")
	}()
	return c, nil
}

// Done is closed once DOGGIE has stopped.
func (c *core) Done() <-chan struct{} { return c.done }

// Stop stops DOGGIE and waits until it has.
func (c *core) Stop() error {
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
