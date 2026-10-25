package main

import (
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/brain"
	"github.com/andr2116d/doggie-assistant/internal/brain/llm"
	"github.com/andr2116d/doggie-assistant/internal/brain/llm/claudecode"
	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/config"
	"github.com/andr2116d/doggie-assistant/internal/demo"
	"github.com/andr2116d/doggie-assistant/internal/heartbeat"
	"github.com/andr2116d/doggie-assistant/internal/mcp"
	"github.com/andr2116d/doggie-assistant/internal/supervisor"
	"github.com/andr2116d/doggie-assistant/internal/task/manager"
	"github.com/andr2116d/doggie-assistant/prompts"
)

// deps is what a component receives when it is built.
type deps struct {
	cfg  config.Config
	log  *slog.Logger
	conn *bus.Conn // the component's own bus connection
}

// factory builds one component. Its name must match the component's Name.
type factory struct {
	name  string
	build func(d deps) (supervisor.Component, error)
}

// factories lists every component DOGGIE knows how to run.
func factories() []factory {
	return []factory{
		{name: "heartbeat", build: func(d deps) (supervisor.Component, error) {
			return heartbeat.New(d.conn, d.log, time.Minute), nil
		}},
		{name: "tasks", build: func(d deps) (supervisor.Component, error) {
			return manager.New(d.conn, d.log, manager.DefaultOptions()), nil
		}},
		{name: "demo", build: func(d deps) (supervisor.Component, error) {
			return demo.New(d.conn, d.log), nil
		}},
		{name: "brain", build: newBrain},
	}
}

// buildComponents builds the selected components, each with its own bus
// connection. The returned func closes those connections.
func buildComponents(fs []factory, cfg config.Config, log *slog.Logger, srv *bus.Server) ([]supervisor.Component, func(), error) {
	var conns []*bus.Conn
	closeAll := func() {
		for _, c := range conns {
			c.Close()
		}
	}
	comps := make([]supervisor.Component, 0, len(fs))
	for _, f := range fs {
		conn, err := srv.Connect(f.name)
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		conns = append(conns, conn)
		c, err := f.build(deps{cfg: cfg, log: log.With("component", f.name), conn: conn})
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("%s: %w", f.name, err)
		}
		comps = append(comps, c)
	}
	return comps, closeAll, nil
}

// selectFactories applies the --only and --except filters.
func selectFactories(all []factory, only, except string) ([]factory, error) {
	switch {
	case only != "" && except != "":
		return nil, errors.New("--only and --except are mutually exclusive")
	case only == "" && except == "":
		return all, nil
	}

	known := make(map[string]bool, len(all))
	for _, f := range all {
		known[f.name] = true
	}
	keep := only != "" // --only keeps listed components; --except drops them
	list := except
	if keep {
		list = only
	}
	filter, err := parseList(list, known)
	if err != nil {
		return nil, err
	}

	var selected []factory
	for _, f := range all {
		if filter[f.name] == keep {
			selected = append(selected, f)
		}
	}
	if len(selected) == 0 {
		return nil, errors.New("no components selected")
	}
	return selected, nil
}

func parseList(list string, known map[string]bool) (map[string]bool, error) {
	set := make(map[string]bool)
	for _, name := range strings.Split(list, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !known[name] {
			return nil, fmt.Errorf("unknown component %q", name)
		}
		set[name] = true
	}
	return set, nil
}

func names(fs []factory) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.name
	}
	return out
}

// newBrain builds the brain with the Claude Code provider. The system prompt
// is fixed for the brain's life, so warm processes can start ahead.
func newBrain(d deps) (supervisor.Component, error) {
	system, err := prompts.System(prompts.Persona{
		AssistantName: d.cfg.Assistant.Name,
		OwnerName:     d.cfg.Owner.Name,
		OwnerAbout:    d.cfg.Owner.About,
		Persona:       d.cfg.Assistant.Persona,
		Language:      d.cfg.Language,
	})
	if err != nil {
		return nil, err
	}
	bc := d.cfg.Brain
	return brain.New(d.conn, d.log, brain.Options{
		HistoryTurns: bc.HistoryTurns,
		NewProvider: func(tools *mcp.Server) (llm.Brain, func(), error) {
			p, err := claudecode.New(claudecode.Options{
				Command:     bc.ClaudeCode.Command,
				Model:       bc.Model,
				System:      system,
				TurnTimeout: bc.TurnTimeout,
				Dir:         filepath.Join(d.cfg.DataDir, "brain", "claudecode"),
				Tools:       tools,
				Log:         d.log,
				Warm:        1,
			})
			if err != nil {
				return nil, nil, err
			}
			return p, p.Close, nil
		},
	}), nil
}
