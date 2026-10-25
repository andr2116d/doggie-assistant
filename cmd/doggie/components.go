package main

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/config"
	"github.com/andr2116d/doggie-assistant/internal/heartbeat"
	"github.com/andr2116d/doggie-assistant/internal/supervisor"
)

// components returns every component DOGGIE knows how to run.
func components(_ config.Config, log *slog.Logger) []supervisor.Component {
	return []supervisor.Component{
		heartbeat.New(log.With("component", "heartbeat"), time.Minute),
	}
}

// selectComponents applies the --only and --except filters.
func selectComponents(all []supervisor.Component, only, except string) ([]supervisor.Component, error) {
	switch {
	case only != "" && except != "":
		return nil, errors.New("--only and --except are mutually exclusive")
	case only == "" && except == "":
		return all, nil
	}

	known := make(map[string]bool, len(all))
	for _, c := range all {
		known[c.Name()] = true
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

	var selected []supervisor.Component
	for _, c := range all {
		if filter[c.Name()] == keep {
			selected = append(selected, c)
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

func names(cs []supervisor.Component) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Name()
	}
	return out
}
