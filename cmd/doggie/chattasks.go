package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus"
	"github.com/andr2116d/doggie-assistant/internal/bus/events"
)

// minStatusInterval bounds how often the status line changes, so a task
// reporting progress many times a second doesn't keep redrawing it.
const minStatusInterval = 500 * time.Millisecond

// maxBoardTasks bounds the tasks a board follows.
const maxBoardTasks = 256

// taskBoard follows the tasks in progress through their events, for the
// chat's status line and for the questions only the user may answer. Only
// the chat loop uses it.
type taskBoard struct {
	thread string
	tasks  map[string]*boardTask
	// ended are tasks seen ending: a late event or an old list mustn't
	// bring them back.
	ended recentIDs
	// looking are the tasks being looked up, with their events since.
	looking map[string][]bus.Envelope
	// refreshing are known tasks being looked up again, since what their
	// events say is unclear, with how many times.
	refreshing map[string]int
	// told are the questions the user was told they must answer, by task
	// and question.
	told recentIDs
}

type boardTask struct {
	info events.TaskInfo
	// seen is when the chat last heard of the task, on its own clock.
	seen time.Time
}

func newTaskBoard(thread string) *taskBoard {
	return &taskBoard{thread: thread, tasks: map[string]*boardTask{}, looking: map[string][]bus.Envelope{}, refreshing: map[string]int{}}
}

// boardList is a list of the active tasks, asked for at asked.
type boardList struct {
	tasks []events.TaskInfo
	asked time.Time
	err   error
}

// boardLookup is a task looked up after an event about a task the board
// didn't know, or didn't understand.
type boardLookup struct {
	id   string
	info events.TaskInfo
	err  error
}

// listActive asks for the active tasks in the background.
func listActive(ctx context.Context, conn *bus.Conn, out chan<- boardList) {
	asked := time.Now()
	go func() {
		lctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		list, err := bus.Request(lctx, conn, events.ListTasks, events.TaskListRequest{ActiveOnly: true})
		select {
		case out <- boardList{tasks: list.Tasks, asked: asked, err: err}:
		case <-ctx.Done():
		}
	}()
}

// merge takes a list asked for at l.asked. Tasks heard of since are kept
// as the events left them. It returns the questions of this conversation
// the user must answer that the board didn't know of, e.g. asked before
// the chat opened.
func (b *taskBoard) merge(l boardList) []string {
	if l.err != nil {
		return nil
	}
	var questions []string
	listed := map[string]bool{}
	for _, info := range l.tasks {
		listed[info.ID] = true
		if b.ended.has(info.ID) || info.Status.Terminal() {
			continue
		}
		if t, ok := b.tasks[info.ID]; ok && !t.seen.Before(l.asked) {
			continue
		}
		b.put(info)
		if q := b.tell(info); q != "" {
			questions = append(questions, q)
		}
	}
	for id, t := range b.tasks {
		if !listed[id] && t.seen.Before(l.asked) {
			delete(b.tasks, id) // it ended while the chat wasn't listening
		}
	}
	return questions
}

// tell returns what to tell the user about a task's question, once: only a
// question of this conversation they must answer themselves.
func (b *taskBoard) tell(info events.TaskInfo) string {
	if info.Status != events.StatusWaitingInput || info.ThreadID != b.thread || !events.PersonMustAnswer(info, events.TaskKinds()) {
		return ""
	}
	if !b.told.add(info.ID + "\x00" + info.Question) {
		return ""
	}
	return onlyYouAnswer(info)
}

// clear forgets the tasks, e.g. when the connection is lost: what is known
// of them may be stale.
func (b *taskBoard) clear() {
	clear(b.tasks)
	clear(b.looking)
	clear(b.refreshing)
}

func (b *taskBoard) put(info events.TaskInfo) {
	if _, ok := b.tasks[info.ID]; !ok && len(b.tasks) >= maxBoardTasks {
		return
	}
	b.tasks[info.ID] = &boardTask{info: info, seen: time.Now()}
}

// event applies a task event. It returns the question the user must answer
// themselves, if the event asks one in this conversation, and the ID of a
// task to look up, if the board doesn't know it or what the event means.
func (b *taskBoard) event(env bus.Envelope) (question string, lookup string) {
	var ref events.TaskRef
	if env.Type == events.TaskCreated.Type() {
		ev, err := bus.Decode(env, events.TaskCreated)
		if err != nil || ev.Data.ID == "" || b.ended.has(ev.Data.ID) {
			return "", ""
		}
		b.put(ev.Data)
		return "", ""
	}
	if err := json.Unmarshal(env.Data, &ref); err != nil || ref.TaskID == "" || b.ended.has(ref.TaskID) {
		return "", ""
	}
	if q, ok := b.looking[ref.TaskID]; ok {
		if len(q) < maxBoardTasks {
			b.looking[ref.TaskID] = append(q, env)
		}
		return "", ""
	}
	t, ok := b.tasks[ref.TaskID]
	if !ok {
		if terminal(env.Type) {
			b.ended.add(ref.TaskID)
			return "", ""
		}
		if len(b.looking) >= maxBoardTasks {
			return "", ""
		}
		b.looking[ref.TaskID] = []bus.Envelope{env}
		return "", ref.TaskID
	}
	question, refresh := b.apply(t, env)
	if _, ok := b.refreshing[t.info.ID]; refresh && !ok && len(b.refreshing) < maxBoardTasks {
		b.refreshing[t.info.ID] = 1
		lookup = t.info.ID
	}
	return question, lookup
}

// looked takes a lookup and applies the events that waited for it. again
// is a task to look up once more, a little later.
func (b *taskBoard) looked(l boardLookup) (questions []string, again string) {
	if n, ok := b.refreshing[l.id]; ok {
		if b.refreshed(l, n) {
			b.refreshing[l.id] = n + 1
			return nil, l.id
		}
		delete(b.refreshing, l.id)
		return nil, ""
	}
	queued, ok := b.looking[l.id]
	delete(b.looking, l.id)
	switch {
	case !ok || l.err != nil || b.ended.has(l.id):
		return nil, ""
	case l.info.Status.Terminal():
		b.ended.add(l.id)
		return nil, ""
	}
	b.put(l.info)
	for _, env := range queued {
		t, ok := b.tasks[l.id]
		if !ok {
			break
		}
		if q, _ := b.apply(t, env); q != "" {
			questions = append(questions, q)
		}
	}
	return questions, ""
}

// refreshed takes what the manager says of a task the board knows, on the
// n-th look. It reports whether to look again: the manager may not have
// heard of a question the board has, the first time.
func (b *taskBoard) refreshed(l boardLookup, n int) (again bool) {
	t, ok := b.tasks[l.id]
	switch {
	case !ok || l.err != nil:
	case l.info.Status.Terminal():
		delete(b.tasks, l.id)
		b.ended.add(l.id)
	case n == 1 && l.info.Status == events.StatusRunning && t.info.Status == events.StatusWaitingInput:
		return true
	default:
		t.info.Status, t.info.Question, t.info.Options = l.info.Status, l.info.Question, l.info.Options
		t.info.Progress, t.info.Percent = l.info.Progress, l.info.Percent
	}
	return false
}

func terminal(typ string) bool {
	return typ == events.TaskDone.Type() || typ == events.TaskFailed.Type() || typ == events.TaskCancelled.Type()
}

// apply updates a known task with one of its events. refresh means what
// the event says is unclear, so the task must be looked up.
func (b *taskBoard) apply(t *boardTask, env bus.Envelope) (question string, refresh bool) {
	t.seen = time.Now()
	switch env.Type {
	case events.TaskRunning.Type():
		// It follows an answer, or the start, which may arrive after
		// the task asked its first question.
		if t.info.Status == events.StatusWaitingInput {
			return "", true
		}
		t.info.Status = events.StatusRunning
	case events.TaskProgress.Type():
		var d events.TaskProgressData
		if json.Unmarshal(env.Data, &d) == nil {
			t.info.Status, t.info.Progress, t.info.Percent = events.StatusRunning, d.Message, d.Percent
		}
	case events.TaskNeedsInput.Type():
		var d events.TaskNeedsInputData
		if json.Unmarshal(env.Data, &d) != nil {
			return "", false
		}
		t.info.Status, t.info.Question, t.info.Options = events.StatusWaitingInput, d.Question, d.Options
		return b.tell(t.info), false
	default:
		if terminal(env.Type) {
			delete(b.tasks, t.info.ID)
			b.ended.add(t.info.ID)
		}
	}
	return "", false
}

// onlyYouAnswer tells the user about a question DOGGIE may not answer for
// them.
func onlyYouAnswer(info events.TaskInfo) string {
	s := fmt.Sprintf("· %s asks: %q", info.ID, truncate(oneLineText(info.Question), 200))
	if len(info.Options) > 0 {
		s += " [" + truncate(oneLineText(strings.Join(info.Options, " | ")), 100) + "]"
	}
	return s + ". Only you may answer it: /answer " + info.ID + " <answer>"
}

// status is the status line: this conversation's tasks, and how many run
// elsewhere. Empty when nothing runs.
func (b *taskBoard) status() string {
	var mine []events.TaskInfo
	others := 0
	for _, t := range b.tasks {
		if t.info.ThreadID == b.thread {
			mine = append(mine, t.info)
		} else {
			others++
		}
	}
	slices.SortFunc(mine, func(x, y events.TaskInfo) int { return compareIDs(x.ID, y.ID) })
	var parts []string
	for _, info := range mine {
		parts = append(parts, taskStatus(info))
	}
	switch {
	case others > 0 && len(mine) > 0:
		parts = append(parts, fmt.Sprintf("+%d elsewhere", others))
	case others > 0:
		parts = append(parts, fmt.Sprintf("%s in other conversations", tasksCount(others)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "· " + strings.Join(parts, " · ")
}

// taskStatus is one task on the status line.
func taskStatus(info events.TaskInfo) string {
	what := info.Description
	if what == "" {
		what = info.Kind
	}
	s := info.ID + " " + truncate(oneLineText(what), 30)
	switch {
	case info.Status == events.StatusWaitingInput:
		return s + ": asks (/answer " + info.ID + " …)"
	case info.Progress != "":
		s += ": " + truncate(oneLineText(info.Progress), 30)
		if info.Percent > 0 && !strings.Contains(info.Progress, "%") {
			s += fmt.Sprintf(" (%d%%)", info.Percent)
		}
	}
	return s
}

// compareIDs orders task IDs like t2 before t10.
func compareIDs(a, b string) int {
	na, errA := strconv.Atoi(strings.TrimPrefix(a, "t"))
	nb, errB := strconv.Atoi(strings.TrimPrefix(b, "t"))
	if errA == nil && errB == nil && na != nb {
		return cmp.Compare(na, nb)
	}
	return strings.Compare(a, b)
}

// taskEvent follows a task event on the board, telling the user a question
// only they may answer. It reports whether it showed something.
func (c *chatState) taskEvent(ctx context.Context, conn *bus.Conn, env bus.Envelope) bool {
	question, lookup := c.board.event(env)
	if lookup != "" {
		c.lookup(ctx, conn, lookup, 0)
	}
	if question != "" {
		c.view.note(question)
	}
	c.showStatus()
	return question != ""
}

// showStatus shows the board on the status line, at most every
// minStatusInterval.
func (c *chatState) showStatus() {
	s := c.board.status()
	if s == c.statusShown || c.statusDue != nil {
		return
	}
	if wait := minStatusInterval - time.Since(c.statusAt); wait > 0 {
		c.statusDue = time.After(wait)
		return
	}
	c.ui.SetStatus(s)
	c.statusShown, c.statusAt = s, time.Now()
}

// recheckDelay is how long the board waits to look a task up again. A
// variable so tests can shrink it.
var recheckDelay = 500 * time.Millisecond

// lookup asks the manager for a task in the background, after a delay, and
// hands the board the answer.
func (c *chatState) lookup(ctx context.Context, conn *bus.Conn, id string, after time.Duration) {
	lookups := c.lookups
	go func() {
		select {
		case <-time.After(after):
		case <-ctx.Done():
			return
		}
		lctx, cancel := context.WithTimeout(ctx, commandTimeout)
		defer cancel()
		info, err := bus.Request(lctx, conn, events.GetTask, events.TaskRef{TaskID: id})
		select {
		case lookups <- boardLookup{id: id, info: info, err: err}:
		case <-ctx.Done():
		}
	}()
}

// answered notes that the user answered a task's question here, so the
// status line needn't wait for the manager to say so. A newer question
// stays.
func (b *taskBoard) answered(asked events.TaskInfo) {
	t, ok := b.tasks[asked.ID]
	if !ok || t.info.Status != events.StatusWaitingInput || t.info.Question != asked.Question {
		return
	}
	t.info.Status, t.info.Question, t.info.Options = events.StatusRunning, "", nil
	t.seen = time.Now()
}
