package main

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/config"
	"github.com/andr2116d/doggie-assistant/internal/demo"
	"github.com/andr2116d/doggie-assistant/internal/heartbeat"
	"github.com/andr2116d/doggie-assistant/internal/supervisor"
	"github.com/andr2116d/doggie-assistant/internal/task/manager"
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
	build func(d deps) supervisor.Component
}

// factories lists every component DOGGIE knows how to run.
func factories() []factory {
	return []factory{
		{name: "heartbeat", build: func(d deps) supervisor.Component {
			return heartbeat.New(d.conn, d.log, time.Minute)
		}},
		{name: "tasks", build: func(d deps) supervisor.Component {
			return manager.New(d.conn, d.log, manager.DefaultOptions())
		}},
		{name: "demo", build: func(d deps) supervisor.Component {
			return demo.New(d.conn, d.log)
		}},
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
		comps = append(comps, f.build(deps{cfg: cfg, log: log.With("component", f.name), conn: conn}))
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
