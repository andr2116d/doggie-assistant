package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/config"
)

// hosting is a DOGGIE a chat runs in its own process: when none runs on this
// machine, or when the one it used went away without being stopped on
// purpose, until an explicit DOGGIE (doggie run) asks for it. Only the chat
// loop uses it.
type hosting struct {
	configPath string
	dataDir    string // known once the config is read
	selected   []factory
	own        *core // nil when DOGGIE doesn't run here
	logPath    string
	// handover delivers an explicit DOGGIE's requests for the one run here,
	// while one runs here; stopHandover stops watching for them.
	handover     <-chan struct{}
	stopHandover func()
}

// takeoverDelay is how long a chat waits before taking over a DOGGIE that
// went away, so a quick restart wins, plus up to takeoverJitter that
// differs between chats, so they don't all try at once. Variables so tests
// can shrink them.
var (
	takeoverDelay  = 2 * time.Second
	takeoverJitter = time.Second
)

func newHosting(configPath string, selected []factory) *hosting {
	return &hosting{configPath: configPath, selected: selected, stopHandover: func() {}}
}

// start runs DOGGIE in this process, logging to a file. An error wrapping
// errRunning means another process runs it.
func (h *hosting) start(ctx context.Context) error {
	cfg, err := config.Load(h.configPath)
	if err != nil {
		return err
	}
	h.dataDir = cfg.DataDir
	logFile, logPath, err := openLog(cfg)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(logFile, &slog.HandlerOptions{Level: cfg.LogLevel()}))
	own, err := startCore(ctx, cfg, log, h.selected, roleChat)
	if err != nil {
		logFile.Close()
		return err
	}
	own.logFile = logFile
	h.own, h.logPath = own, logPath
	wctx, stop := context.WithCancel(context.Background())
	h.handover, h.stopHandover = watchHandover(wctx, h.dataDir), stop
	return nil
}

// lastStop is why the DOGGIE of this machine last stopped, if it said so.
func (h *hosting) lastStop() string {
	if h.dataDir == "" {
		cfg, err := config.Load(h.configPath)
		if err != nil {
			return ""
		}
		h.dataDir = cfg.DataDir
	}
	return stopReason(h.dataDir)
}

// done is closed if the DOGGIE run here stops; nil when none runs here.
func (h *hosting) done() <-chan struct{} {
	if h == nil || h.own == nil {
		return nil
	}
	return h.own.Done()
}

// release forgets the DOGGIE run here and returns it, for the caller to
// stop.
func (h *hosting) release() *core {
	own := h.own
	h.own = nil
	h.stopHandover()
	h.handover, h.stopHandover = nil, func() {}
	return own
}

// close stops the DOGGIE run here, if any, telling the other chats they
// may take over.
func (h *hosting) close() {
	if own := h.release(); own != nil {
		stopCore(own, events.StopHostClosed)
	}
}

// stopCore stops a DOGGIE run by a chat and closes its log.
func stopCore(own *core, reason string) {
	own.Stop(reason)
	if own.logFile != nil {
		own.logFile.Close()
	}
}

// jitter spreads the chats' attempts to take over.
func jitter() time.Duration {
	if takeoverJitter <= 0 {
		return 0
	}
	return time.Duration(os.Getpid()) * time.Millisecond % takeoverJitter
}
