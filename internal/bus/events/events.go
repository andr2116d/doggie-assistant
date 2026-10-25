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

// Why DOGGIE stops, in SystemStopping.
const (
	// StopOnPurpose: someone stopped it; clients leave it stopped.
	StopOnPurpose = "stopped"
	// StopHostClosed: the chat that ran it closed; another may take over.
	StopHostClosed = "host-closed"
	// StopHandover: it moves to another process, which starts it again.
	StopHandover = "handover"
)

// StoppingData says why DOGGIE is about to stop. A DOGGIE that crashes or
// is killed says nothing.
type StoppingData struct {
	Reason string `json:"reason"`
}

// SystemStopping is published by the core right before it stops.
var SystemStopping = bus.NewSubject[StoppingData]("system.stopping")

// SatellitePermissions is what a satellite or tool connecting over TCP may
// do: watch events, pass on what people say on its own channels
// (user.utterance.<channel>), use the task manager's commands, cancelling
// only from its own channels (task.cancel.from.<channel>), and run tasks
// as workers (report on them, beat, spawn subtasks). It cannot speak for
// other channels, forge the manager's events (task.created, running,
// cancelled), the brain's (out.speak, brain.turn) or the core's (system.*),
// impersonate the manager, dispatch tasks to workers, or alter the event
// log. It may follow what DOGGIE says as it is written (out.speak.delta),
// and ask the brain for a conversation's last turns (brain.history).
// Invalid channels are left out.
func SatellitePermissions(channels ...string) bus.Permissions {
	var byChannel []string
	for _, c := range channels {
		if s, err := UtteranceOn(c); err == nil {
			byChannel = append(byChannel, s.Wire())
		}
		if cmd, err := CancelTaskOn(c); err == nil {
			byChannel = append(byChannel, cmd.Wire())
		}
	}
	return bus.Permissions{
		Publish: append(byChannel,
			TaskProgress.Wire(), TaskNeedsInput.Wire(), TaskDone.Wire(), TaskFailed.Wire(),
			TaskAlive.Wire(),
			StartTask.Wire(), AnswerTask.Wire(), ListTasks.Wire(), GetTask.Wire(), BrainHistory.Wire(),
			ReportResult.Wire(),
		),
		Subscribe: []string{
			bus.AllEvents,
			OutSpeakDelta.Wire(),
			RunTask.Wire() + ".>",
			ControlTask.Wire() + ".>",
		},
	}
}
