package events

import "encoding/json"

// Risk is how much harm an action can do. It decides whether the action needs
// a person's confirmation (ADR-0003). It is declared here, in reviewed code,
// never by the worker that runs the action or by the LLM that asks for it.
type Risk string

const (
	// RiskRead only looks at the world.
	RiskRead Risk = "read"
	// RiskAct changes the world in ways that are easy to undo (lights, TV).
	RiskAct Risk = "act"
	// RiskSensitive needs a person's confirmation on a trusted channel
	// (locks, messages to other people, purchases).
	RiskSensitive Risk = "sensitive"
)

// TaskKind describes a kind of task the brain may start.
type TaskKind struct {
	Name string
	// Description tells the LLM what the task does and when to use it.
	Description string
	Risk        Risk
	// InputSchema is the JSON Schema of the task's input.
	InputSchema json.RawMessage
}

// Task kinds of the demo worker, which stand in for real devices until they
// exist.
var (
	KindDemoSleep = TaskKind{
		Name: "demo.sleep",
		Description: "Pretend to perform a real-world action, such as opening an app on the TV, " +
			"until real device tools exist. It takes the given number of seconds and reports progress every second.",
		Risk: RiskAct,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"seconds": {"type": "integer", "minimum": 1, "maximum": 3600, "description": "How long the action takes (default 5)."}
			},
			"additionalProperties": false
		}`),
	}
	KindDemoAsk = TaskKind{
		Name: "demo.ask",
		Description: "Pretend to perform an action that needs a decision from the user midway: " +
			"the task asks the question, waits for the answer and returns it.",
		Risk: RiskAct,
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"question": {"type": "string", "minLength": 1, "maxLength": 500, "description": "The question the task will ask."},
				"options": {"type": "array", "items": {"type": "string", "maxLength": 100}, "maxItems": 10, "description": "Possible answers, if any."}
			},
			"required": ["question"],
			"additionalProperties": false
		}`),
	}
)

// TaskKinds returns the task kinds the brain may start.
func TaskKinds() []TaskKind {
	return []TaskKind{KindDemoSleep, KindDemoAsk}
}
