package main

import (
	"cmp"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/andr2116d/doggie-assistant/internal/bus/events"
	"github.com/andr2116d/doggie-assistant/internal/config"
	"github.com/andr2116d/doggie-assistant/internal/console"
)

// chatScreen is the UI the chat opens on its terminal, closed when it ends.
type chatScreen interface {
	chatUI
	Close() error
}

// openChatUI opens the chat's UI: a full screen on a terminal, plain lines
// otherwise.
func openChatUI(cfg config.Config) (chatScreen, error) {
	if !console.IsTerminal(os.Stdin, os.Stdout) {
		return console.Plain(os.Stdin, os.Stdout), nil
	}
	return console.OpenScreen(os.Stdin, os.Stdout, screenOptions(cfg))
}

// screenOptions are the full screen's options for cfg's assistant and
// language.
func screenOptions(cfg config.Config) console.ScreenOptions {
	name := cfg.Assistant.Name
	if name == "" {
		name = assistantName
	}
	return console.ScreenOptions{
		Name: name, MaxRunes: events.MaxUtteranceRunes, Forget: isAnswer,
		Commands: words(cfg.Language).commands, Texts: words(cfg.Language).screen,
	}
}

// chatWords are the chat's own words in one language.
type chatWords struct {
	screen   console.ScreenTexts
	thinking string
	// reconnecting is the connection while it is lost; elsewhere labels
	// what the user said in another chat of the same channel.
	reconnecting, elsewhere string
	// waiting tells a task waits for an answer; others how many tasks run
	// in other conversations.
	waiting string
	others  func(n int) string
	// tr puts the chat's other texts in the language.
	tr translator
	// commands are the chat's commands, as the menu shows them.
	commands []console.Command
}

// words are the chat's words in lang (BCP 47), English when it has none in
// that language.
func words(lang string) chatWords {
	if base, _, _ := strings.Cut(strings.ToLower(lang), "-"); base == "es" {
		return chatWords{
			screen: console.ScreenTexts{
				Online: "en línea", Offline: "sin conexión", Channel: "terminal",
				Hints: "/help comandos · ↑ historial · Ctrl+D salir", HintsRight: "Tab: panel",
				Tasks: "TAREAS", Notices: "AVISOS", NoTasks: "sin tareas en curso", TooSmall: "Agranda la ventana\npara hablar conmigo",
				Newer: func(n int) string {
					if n == 1 {
						return "↓ 1 nuevo"
					}
					return fmt.Sprintf("↓ %d nuevos", n)
				},
				Clock: spanishClock, Copied: "✓ copiado",
			},
			thinking: "pensando", reconnecting: "reconectando…", elsewhere: "tú, en otra ventana",
			waiting: "espera tu respuesta", tr: spanish,
			commands: []console.Command{
				{Name: "/tasks", Hint: "las tareas en curso"},
				{Name: "/tasks all", Hint: "también las últimas que terminaron"},
				{Name: "/answer", Hint: "responder tú la pregunta de una tarea", Args: true},
				{Name: "/cancel", Hint: "cancelar una tarea", Args: true},
				{Name: "/help", Hint: "la lista de comandos"},
			},
			others: func(n int) string {
				if n == 1 {
					return "+1 en otra conversación"
				}
				return fmt.Sprintf("+%d en otras conversaciones", n)
			},
		}
	}
	return chatWords{
		screen: console.ScreenTexts{
			Online: "online", Offline: "offline", Channel: "terminal",
			Hints: "/help commands · ↑ history · Ctrl+D quit", HintsRight: "Tab: panel",
			Tasks: "TASKS", Notices: "NOTICES", NoTasks: "no tasks running", TooSmall: "Make the window bigger\nto talk with me",
			Newer:  func(n int) string { return fmt.Sprintf("↓ %d new", n) },
			Clock:  func(t time.Time) string { return t.Format("Mon Jan 2 · 15:04") },
			Copied: "✓ copied",
		},
		thinking: "thinking", reconnecting: "reconnecting…", elsewhere: "you, elsewhere",
		waiting: "waits for your answer",
		commands: []console.Command{
			{Name: "/tasks", Hint: "the tasks in progress"},
			{Name: "/tasks all", Hint: "also the last ones that ended"},
			{Name: "/answer", Hint: "answer a task's question yourself", Args: true},
			{Name: "/cancel", Hint: "cancel a task", Args: true},
			{Name: "/help", Hint: "the list of commands"},
		},
		others: func(n int) string {
			if n == 1 {
				return "+1 in another conversation"
			}
			return fmt.Sprintf("+%d in other conversations", n)
		},
	}
}

// spanishClock says t as "mar 28 oct · 21:40".
func spanishClock(t time.Time) string {
	days := [...]string{"dom", "lun", "mar", "mié", "jue", "vie", "sáb"}
	months := [...]string{"ene", "feb", "mar", "abr", "may", "jun", "jul", "ago", "sep", "oct", "nov", "dic"}
	return fmt.Sprintf("%s %d %s · %s", days[t.Weekday()], t.Day(), months[t.Month()-1], t.Format("15:04"))
}

// replyScreen shows each block of a reply as a bubble of its own, as a
// full screen does.
type replyScreen interface {
	Stream(id, text string)
	Reply(id, text string)
	Notice(id string, kind console.NoticeKind, text string)
	Said(text string)
	SaidElsewhere(label, text string)
	SetConnection(online bool, status string)
}

// newScreenView is a replyView that shows replies on s, a bubble per block.
func newScreenView(s replyScreen) *replyView {
	v := newReplyView(nil)
	v.screen = s
	return v
}

// screenDelta shows a piece of block k on the screen.
func (v *replyView) screenDelta(k blockKey, text string) {
	v.screen.Stream(k.id(), text)
	v.open = &k
}

// screenBlock ends block k on the screen with its whole text.
func (v *replyView) screenBlock(k blockKey, text string) {
	v.screen.Reply(k.id(), text)
	if v.open != nil && *v.open == k {
		v.open = nil
	}
}

// online shows on the screen, if there is one, whether the chat is
// connected.
func (c *chatState) online(up bool) {
	if s := c.view.screen; s != nil {
		status := ""
		if !up {
			status = c.wordsOr().reconnecting
		}
		s.SetConnection(up, status)
	}
}

// wordsOr is the chat's words, English if none were set.
func (c *chatState) wordsOr() chatWords {
	if c.o.words.thinking == "" {
		return words("")
	}
	return c.o.words
}

// turnOnScreen shows a past turn on the screen as it showed live: what was
// said, the tasks it heard of and the replies.
func (c *chatState) turnOnScreen(id string, t events.BrainTurnData) {
	s := c.view.screen
	for _, in := range t.Inputs {
		switch {
		case in.Task != nil:
			s.Notice("", console.NoticeInfo, taskNotice(c.wordsOr().tr, in.Task))
		case in.Said != "" && (in.Channel == "" || in.Channel == c.o.channel):
			s.Said(in.Said)
		case in.Said != "":
			s.SaidElsewhere(in.Channel, in.Said)
		}
	}
	for i, r := range t.Replies {
		if r = strings.TrimSpace(r); r != "" {
			s.Reply(fmt.Sprintf("%s#%d", id, i), r)
		}
	}
	if why := failure(c.wordsOr().tr, t); why != "" {
		s.Notice("", console.NoticeBad, c.wordsOr().tr.f("the turn failed: %s", why))
	}
}

// taskUI shows the tasks in a panel, as a full screen does.
type taskUI interface {
	SetTasks(tasks []console.TaskView, more string)
}

// views are the board's tasks of this conversation as the panel shows
// them, and what to say about the others.
func (b *taskBoard) views(w chatWords) ([]console.TaskView, string) {
	var views []console.TaskView
	others := 0
	var mine []events.TaskInfo
	for _, t := range b.tasks {
		if t.info.ThreadID == b.thread {
			mine = append(mine, t.info)
		} else {
			others++
		}
	}
	slices.SortFunc(mine, func(x, y events.TaskInfo) int { return compareIDs(x.ID, y.ID) })
	for _, info := range mine {
		v := console.TaskView{ID: info.ID, Title: oneLineText(cmp.Or(info.Description, info.Kind)), Percent: info.Percent}
		switch {
		case info.Status == events.StatusWaitingInput:
			v.Waiting, v.Detail = true, w.waiting+" · /answer "+info.ID
		case info.Progress != "":
			v.Detail = oneLineText(info.Progress)
		}
		views = append(views, v)
	}
	more := ""
	if others > 0 {
		more = w.others(others)
	}
	return views, more
}

// say shows one of the chat's own notes, in the user's language.
func (c *chatState) say(kind console.NoticeKind, format string, args ...any) {
	c.view.notice(kind, "· "+c.wordsOr().tr.f(format, args...))
}
