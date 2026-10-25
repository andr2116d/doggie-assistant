package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

// Limits on what the LLM may send. Its input is untrusted: it may have been
// steered by a prompt injection.
const (
	maxTaskIDBytes     = 64
	maxDescriptionRune = 200
	maxInputBytes      = 16 << 10
	maxAnswerRunes     = 2000
	maxReasonRunes     = 500
	defaultListLimit   = 20
	maxListLimit       = 50
)

// Limits on what the tools return, so a chatty worker can't flood the
// prompt.
const (
	maxResultBytes = 2 << 10
	maxTextRunes   = 500
)

// errNeedsConfirmation refuses sensitive actions. They will need a person's
// confirmation on a trusted channel; until that flow exists they are refused.
var errNeedsConfirmation = errors.New("this action is sensitive and needs the user's confirmation, which is not available yet: tell the user you can't do it")

// kind is a task kind the tools may start, with its compiled input schema.
type kind struct {
	events.TaskKind
	schema *jsonschema.Resolved
}

func compileKinds(list []events.TaskKind) (map[string]kind, error) {
	kinds := make(map[string]kind, len(list))
	for _, k := range list {
		if !bus.ValidName(k.Name) {
			return nil, fmt.Errorf("mcp: invalid task kind name %q", k.Name)
		}
		if _, dup := kinds[k.Name]; dup {
			return nil, fmt.Errorf("mcp: task kind %q listed twice", k.Name)
		}
		switch k.Risk {
		case events.RiskRead, events.RiskAct, events.RiskSensitive:
		default:
			return nil, fmt.Errorf("mcp: task kind %q has unknown risk %q", k.Name, k.Risk)
		}
		var s jsonschema.Schema
		if err := json.Unmarshal(k.InputSchema, &s); err != nil {
			return nil, fmt.Errorf("mcp: task kind %q: input schema: %w", k.Name, err)
		}
		resolved, err := s.Resolve(nil)
		if err != nil {
			return nil, fmt.Errorf("mcp: task kind %q: input schema: %w", k.Name, err)
		}
		kinds[k.Name] = kind{TaskKind: k, schema: resolved}
	}
	return kinds, nil
}

// addTool registers a tool with its risk level. The handler only runs for a
// client bound to a turn, and never for a sensitive tool.
func addTool[In, Out any](s *Server, t *sdk.Tool, risk events.Risk, h func(ctx context.Context, turn Turn, in In) (Out, error)) {
	s.tools[t.Name] = risk
	if risk == events.RiskRead {
		t.Annotations = &sdk.ToolAnnotations{ReadOnlyHint: true}
	}
	sdk.AddTool(s.mcp, t, func(ctx context.Context, req *sdk.CallToolRequest, in In) (*sdk.CallToolResult, Out, error) {
		var zero Out
		turn, err := s.turnOf(req)
		if err != nil {
			s.log.Warn("tool called outside a turn", "tool", t.Name)
			return nil, zero, err
		}
		if risk == events.RiskSensitive {
			return nil, zero, errNeedsConfirmation
		}
		out, err := h(ctx, turn, in)
		if err != nil {
			s.log.Info("tool failed", "tool", t.Name, "thread", turn.ThreadID, "err", err)
			return nil, zero, err
		}
		s.log.Debug("tool called", "tool", t.Name, "thread", turn.ThreadID)
		return nil, out, nil
	})
}

type startTaskIn struct {
	Kind        string         `json:"kind" jsonschema:"the task kind, one of those listed in this tool's description"`
	Description string         `json:"description" jsonschema:"what the task does, in a few words in the user's language, e.g. Open Netflix on the TV"`
	Input       map[string]any `json:"input,omitempty" jsonschema:"the task's input, matching the kind's input schema"`
}

type startTaskOut struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
}

type listTasksIn struct {
	IncludeFinished bool `json:"include_finished,omitempty" jsonschema:"also list tasks that are over (done, failed or cancelled)"`
	Limit           int  `json:"limit,omitempty" jsonschema:"how many of the most recent tasks to list, 1-50 (default 20)"`
}

type listTasksOut struct {
	Tasks []taskView `json:"tasks"`
	// Total is how many tasks matched before the limit.
	Total int `json:"total"`
}

type taskRefIn struct {
	TaskID string `json:"task_id" jsonschema:"the task's ID, e.g. t7"`
}

type cancelTaskIn struct {
	TaskID string `json:"task_id" jsonschema:"the task's ID, e.g. t7"`
	Reason string `json:"reason,omitempty" jsonschema:"why it is cancelled"`
}

type answerTaskIn struct {
	TaskID string `json:"task_id" jsonschema:"the ID of the task waiting for an answer"`
	Answer string `json:"answer" jsonschema:"the user's answer to the task's question"`
}

type okOut struct {
	OK bool `json:"ok"`
}

// taskView is what the LLM sees of a task.
type taskView struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Description string `json:"description,omitempty"`
	Status      string `json:"status"`
	// ThisThread is true for tasks requested in the current conversation.
	ThisThread bool     `json:"this_thread"`
	CreatedAt  string   `json:"created_at"`
	Progress   string   `json:"progress,omitempty"`
	Percent    int      `json:"percent,omitempty"`
	Question   string   `json:"question,omitempty"`
	Options    []string `json:"options,omitempty"`
	Result     any      `json:"result,omitempty"`
	Error      string   `json:"error,omitempty"`
	// Note explains a missing result.
	Note string `json:"note,omitempty"`
}

func (s *Server) addTaskTools() {
	addTool(s, &sdk.Tool{Name: "start_task", Description: s.startTaskDescription(), InputSchema: startTaskSchema()}, events.RiskAct, s.startTask)
	addTool(s, &sdk.Tool{
		Name:        "list_tasks",
		Description: "List the tasks, most recent last: by default only the active ones (pending, running or waiting for an answer).",
	}, events.RiskRead, s.listTasks)
	addTool(s, &sdk.Tool{
		Name:        "get_task",
		Description: "Get the current state of one task: progress, question, result or error.",
	}, events.RiskRead, s.getTask)
	addTool(s, &sdk.Tool{
		Name:        "cancel_task",
		Description: "Cancel an active task, and its subtasks.",
	}, events.RiskAct, s.cancelTask)
	addTool(s, &sdk.Tool{
		Name:        "answer_task",
		Description: "Give a task waiting for input the user's answer to its question. Only answer with what the user actually said; never make up an answer.",
	}, events.RiskAct, s.answerTask)
}

// startTaskSchema is start_task's input schema. It lets input be null:
// LLMs often send null for an optional argument.
func startTaskSchema() *jsonschema.Schema {
	s, err := jsonschema.For[startTaskIn](nil)
	if err != nil {
		panic(fmt.Sprintf("mcp: start_task schema: %v", err))
	}
	input := s.Properties["input"]
	input.Type, input.Types = "", []string{"null", "object"}
	return s
}

func (s *Server) startTaskDescription() string {
	var b strings.Builder
	b.WriteString("Start a task in the background, such as an action on a device. It returns the task_id at once while the task keeps running: " +
		"never wait for it or say it is done. You are told when the task finishes, fails or needs an answer.\n\nTask kinds:")
	for _, k := range s.ordered {
		var schema bytes.Buffer
		if err := json.Compact(&schema, k.InputSchema); err != nil {
			schema.Write(k.InputSchema)
		}
		fmt.Fprintf(&b, "\n- %s (risk: %s): %s Input schema: %s", k.Name, k.Risk, k.Description, schema.String())
	}
	return b.String()
}

func (s *Server) startTask(ctx context.Context, turn Turn, in startTaskIn) (startTaskOut, error) {
	k, ok := s.kinds[in.Kind]
	if !ok {
		return startTaskOut{}, fmt.Errorf("unknown task kind %q: use one of those listed in the tool description", in.Kind)
	}
	if k.Risk == events.RiskSensitive {
		return startTaskOut{}, errNeedsConfirmation
	}
	desc, err := cleanLine(in.Description, "description", maxDescriptionRune)
	if err != nil {
		return startTaskOut{}, err
	}
	if desc == "" {
		return startTaskOut{}, errors.New("description must not be empty")
	}
	input := in.Input
	if input == nil {
		input = map[string]any{}
	}
	if err := k.schema.Validate(input); err != nil {
		return startTaskOut{}, fmt.Errorf("invalid input for %s: %w", k.Name, err)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return startTaskOut{}, fmt.Errorf("encode input: %w", err)
	}
	if len(raw) > maxInputBytes {
		return startTaskOut{}, fmt.Errorf("input is too large (%d bytes, max %d)", len(raw), maxInputBytes)
	}
	ref, err := bus.Request(ctx, s.conn, events.StartTask, events.TaskSpec{
		Kind:        k.Name,
		Input:       raw,
		Description: desc,
		ThreadID:    turn.ThreadID,
	})
	if err != nil {
		return startTaskOut{}, busError(err)
	}
	return startTaskOut{TaskID: ref.TaskID, Status: "started"}, nil
}

func (s *Server) listTasks(ctx context.Context, turn Turn, in listTasksIn) (listTasksOut, error) {
	limit := in.Limit
	switch {
	case limit == 0:
		limit = defaultListLimit
	case limit < 0 || limit > maxListLimit:
		return listTasksOut{}, fmt.Errorf("limit must be between 1 and %d", maxListLimit)
	}
	list, err := bus.Request(ctx, s.conn, events.ListTasks, events.TaskListRequest{ActiveOnly: !in.IncludeFinished, Limit: limit})
	if err != nil {
		return listTasksOut{}, busError(err)
	}
	out := listTasksOut{Tasks: make([]taskView, 0, len(list.Tasks)), Total: list.Total}
	for _, t := range list.Tasks {
		out.Tasks = append(out.Tasks, s.view(t, turn))
	}
	return out, nil
}

func (s *Server) getTask(ctx context.Context, turn Turn, in taskRefIn) (taskView, error) {
	info, err := s.lookup(ctx, in.TaskID)
	if err != nil {
		return taskView{}, err
	}
	return s.view(info, turn), nil
}

func (s *Server) cancelTask(ctx context.Context, _ Turn, in cancelTaskIn) (okOut, error) {
	if err := checkTaskID(in.TaskID); err != nil {
		return okOut{}, err
	}
	reason, err := cleanLine(in.Reason, "reason", maxReasonRunes)
	if err != nil {
		return okOut{}, err
	}
	if _, err := bus.Request(ctx, s.conn, events.CancelTask, events.TaskCancelRequest{TaskID: in.TaskID, Reason: reason}); err != nil {
		return okOut{}, busError(err)
	}
	return okOut{OK: true}, nil
}

func (s *Server) answerTask(ctx context.Context, _ Turn, in answerTaskIn) (okOut, error) {
	answer := strings.TrimSpace(in.Answer)
	if answer == "" {
		return okOut{}, errors.New("answer must not be empty")
	}
	if utf8.RuneCountInString(answer) > maxAnswerRunes {
		return okOut{}, fmt.Errorf("answer is too long (max %d characters)", maxAnswerRunes)
	}
	info, err := s.lookup(ctx, in.TaskID)
	if err != nil {
		return okOut{}, err
	}
	// The LLM must not confirm sensitive actions on its own, so it may only
	// answer tasks whose kind is known not to be sensitive. Tasks started
	// elsewhere with other kinds are answered by a person directly.
	if k, ok := s.kinds[info.Kind]; !ok || k.Risk == events.RiskSensitive || info.Sensitive {
		return okOut{}, fmt.Errorf("task %s must be answered by the user directly, not through you", info.ID)
	}
	if _, err := bus.Request(ctx, s.conn, events.AnswerTask, events.TaskAnswer{TaskID: info.ID, Answer: answer}); err != nil {
		return okOut{}, busError(err)
	}
	return okOut{OK: true}, nil
}

func (s *Server) lookup(ctx context.Context, id string) (events.TaskInfo, error) {
	if err := checkTaskID(id); err != nil {
		return events.TaskInfo{}, err
	}
	info, err := bus.Request(ctx, s.conn, events.GetTask, events.TaskRef{TaskID: id})
	if err != nil {
		return events.TaskInfo{}, busError(err)
	}
	return info, nil
}

// view is what the LLM sees of a task. Results of sensitive tasks stay
// hidden, and long texts are cut.
func (s *Server) view(t events.TaskInfo, turn Turn) taskView {
	v := taskView{
		ID:          t.ID,
		Kind:        t.Kind,
		Description: truncate(t.Description, maxTextRunes),
		Status:      string(t.Status),
		ThisThread:  t.ThreadID != "" && t.ThreadID == turn.ThreadID,
		CreatedAt:   t.CreatedAt.Format(time.RFC3339),
		Progress:    truncate(t.Progress, maxTextRunes),
		Percent:     t.Percent,
		Question:    truncate(t.Question, maxTextRunes),
		Error:       truncate(t.Error, maxTextRunes),
	}
	for i, o := range t.Options {
		if i == 10 {
			break
		}
		v.Options = append(v.Options, truncate(o, 100))
	}
	switch {
	case len(t.Result) == 0:
	case t.Sensitive:
		v.Note = "the result is hidden: this is a sensitive task"
	case len(t.Result) > maxResultBytes:
		v.Note = fmt.Sprintf("the result is too long to show (%d bytes)", len(t.Result))
	default:
		if err := json.Unmarshal(t.Result, &v.Result); err != nil {
			v.Note = "the result is not valid JSON"
		}
	}
	return v
}

// busError turns a bus failure into an error the LLM can act on.
func busError(err error) error {
	var remote *bus.RemoteError
	switch {
	case errors.As(err, &remote):
		return errors.New(remote.Message)
	case errors.Is(err, bus.ErrNoHandler):
		return errors.New("the task manager is not running")
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("the task manager did not answer in time")
	}
	return err
}

func checkTaskID(id string) error {
	if id == "" || len(id) > maxTaskIDBytes || !bus.ValidName(id) {
		return fmt.Errorf("invalid task_id %q", truncate(id, 20))
	}
	return nil
}

// cleanLine trims s and checks it is one line of at most maxRunes.
func cleanLine(s, field string, maxRunes int) (string, error) {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
		return "", fmt.Errorf("%s must be a single line of text", field)
	}
	if utf8.RuneCountInString(s) > maxRunes {
		return "", fmt.Errorf("%s is too long (max %d characters)", field, maxRunes)
	}
	return s, nil
}

// truncate cuts s to at most maxRunes, marking the cut.
func truncate(s string, maxRunes int) string {
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxRunes]) + "…"
}
