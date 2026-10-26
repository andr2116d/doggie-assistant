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
// only from its own channels (task.cancel.from.<channel>), follow what
// DOGGIE says as it is written (out.speak.delta), and ask the brain for a
// conversation's last turns (brain.history).
//
// With kinds, it also runs those kinds of tasks as a worker named name: it
// gets their run requests and the controls of the tasks it accepts, and
// reports on them, all under its name. It cannot run other kinds, report
// as another worker, forge the manager's events (only the manager publishes
// task events), the brain's (out.speak, brain.turn) or the core's
// (system.*), impersonate the manager, dispatch tasks to workers, or alter
// the event log. Invalid channels and kinds are left out, and without a
// valid name it runs no tasks.
func SatellitePermissions(name string, channels, kinds []string) bus.Permissions {
	var publish, subscribe []string
	for _, c := range channels {
		if s, err := UtteranceOn(c); err == nil {
			publish = append(publish, s.Wire())
		}
		if cmd, err := CancelTaskOn(c); err == nil {
			publish = append(publish, cmd.Wire())
		}
	}
	if bus.ValidSatelliteName(name) {
		for _, k := range kinds {
			if bus.ValidName(k) {
				subscribe = append(subscribe, RunTask.With(k).Wire())
			}
		}
	}
	if len(subscribe) > 0 {
		publish = append(publish,
			WorkerAccept.With(name).Wire(), WorkerProgress.With(name).Wire(), WorkerAsk.With(name).Wire(),
			WorkerDone.With(name).Wire(), WorkerFailed.With(name).Wire(), TaskAlive.With(name).Wire(),
		)
		subscribe = append(subscribe, ControlTask.With(name).Wire()+".>")
	}
	return bus.Permissions{
		Publish: append(publish,
			StartTask.Wire(), AnswerTask.Wire(), ListTasks.Wire(), GetTask.Wire(), BrainHistory.Wire(),
		),
		Subscribe: append(subscribe, bus.AllEvents, OutSpeakDelta.Wire()),
	}
}
