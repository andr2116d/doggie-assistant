// Package events is the catalog of DOGGIE's bus events and commands: the
// contract shared by all components. Components never import each other,
// only this catalog.
package events

import "github.com/andr2116d/doggie-assistant/internal/bus"

// HeartbeatData reports that the core process is alive.
type HeartbeatData struct {
	UptimeS int64 `json:"uptime_s"`
}

// Heartbeat is published periodically by the core.
var Heartbeat = bus.NewSubject[HeartbeatData]("system.heartbeat")
