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

// SatellitePermissions is what satellites and tools connecting over TCP may
// do: watch events, pass on what people say (user.utterance), use the task
// manager's commands, and run tasks as workers (report on them, beat, spawn
// subtasks). They cannot forge the manager's events (task.created, running,
// cancelled), the brain's (out.speak) or the core's (system.*), impersonate
// the manager, dispatch tasks to workers, or alter the event log.
func SatellitePermissions() bus.Permissions {
	return bus.Permissions{
		Publish: []string{
			UserUtterance.Wire(),
			TaskProgress.Wire(), TaskNeedsInput.Wire(), TaskDone.Wire(), TaskFailed.Wire(),
			TaskAlive.Wire(),
			StartTask.Wire(), CancelTask.Wire(), AnswerTask.Wire(), ListTasks.Wire(), GetTask.Wire(),
			ReportResult.Wire(),
		},
		Subscribe: []string{
			bus.AllEvents,
			RunTask.Wire() + ".>",
			ControlTask.Wire() + ".>",
		},
	}
}
