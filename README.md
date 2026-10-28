# DOGGIE 🐕

A self-hosted, Jarvis-style personal assistant written in Go.

DOGGIE talks with you, runs many tasks in parallel without blocking the conversation, controls your home and learns how to do new things.

> **Status:** early. DOGGIE already chats in a terminal, starts tasks while it keeps talking and tells you when they end. Voice, home devices and WhatsApp come next.

## Goals

- **Never blocks the conversation.** Every action is an async task. Keep talking while DOGGIE works, and it tells you when it's done.
- **Interrupts like a human.** Notices are delivered at natural pauses, phrased in context.
- **Learns.** What it figures out once with an LLM, it replays directly next time.
- **One assistant, many channels:** voice at home, WhatsApp on the go, a web HUD and a terminal.
- **Yours.** Self-hosted. Your configuration and data live outside the code.

## What works today

- **Conversation.** The brain runs on the official [Claude Code](https://docs.anthropic.com/en/docs/claude-code) CLI with your own subscription. DOGGIE builds the whole context of every turn (persona, active tasks, recent turns), so the CLI process is disposable. Two warm processes wait ahead (`brain.warm`), and replies are streamed: the first words arrive in about a second, nearly all of it the model's. DOGGIE speaks as itself: asked about it, it names the model it thinks with, but it never repeats what the CLI adds about your account or machine. When your usage limit runs low or runs out, DOGGIE tells you so in your language, with when it renews.
- **Tasks that don't block.** Ask for something and DOGGIE starts a task, keeps talking, and tells you when it ends, fails or needs an answer. Tasks report progress, can be cancelled and time out. For now, demo tasks (`demo.sleep`, `demo.ask`) stand in for real devices.
- **Terminal chat.** Replies stream in above the line you type, which never breaks, with live task status and commands to see, answer and cancel tasks without the LLM.
- **Locked-down LLM.** The LLM has no shell, files or web. It can only use DOGGIE's tools, and each one declares a risk level that the code enforces, not the prompt.
- **Event bus.** An embedded NATS server with a persistent event log (7 days by default), TLS from DOGGIE's own CA and token auth. Every component talks only over the bus.
- **Restarts lose nothing.** Interrupted tasks are reported as failed, and sentences a restart left unanswered are answered after it.
- **Satellites.** Other processes or machines (a voice satellite, a chat bridge, a box that controls the lights) connect with their own token. Each may only speak for its own channels and run its own kinds of tasks.
- **Tasks in order.** Workers report to the task manager, which alone publishes task events: they are always in order, and no one can report on a task they don't run.

## Requirements

- Linux or macOS.
- [Go](https://go.dev/dl/) 1.27.1 or newer.
- The `claude` CLI, logged in with a Claude subscription: run `claude` once and log in. DOGGIE removes `ANTHROPIC_API_KEY` from the CLI's environment, so it always uses that login.

## Install

From a clone, into `~/.local/bin`, which needs no root:

```sh
git clone https://github.com/andr2116d/doggie-assistant.git
cd doggie-assistant
make install                      # or: make install PREFIX=/usr/local
```

If `~/.local/bin` isn't in your `PATH`, `make install` says so and shows the line to add to your shell profile. `make uninstall` removes it. Or, with Go alone, into `$(go env GOPATH)/bin`:

```sh
go install github.com/andr2116d/doggie-assistant/cmd/doggie@latest
```

Then configure it:

```sh
mkdir -p ~/.config/doggie
cp configs/doggie.example.yaml ~/.config/doggie/config.yaml
```

Edit `~/.config/doggie/config.yaml`: your name and profile, the assistant's persona, the language it speaks (`en`, `es`, ...). Without the file, DOGGIE starts with defaults. The example is also [in the repo](configs/doggie.example.yaml).

DOGGIE thinks with your `claude` login. Optionally, give it a backup login, maybe of another account, for when your usage limit is spent:

```sh
doggie login
```

It runs `claude setup-token`, which opens the browser (log in there with the account to use), asks for the token it prints and tries it. While your limit is spent, turns run on the backup; once yours renews, DOGGIE comes back to it. The token is kept in `~/.local/share/doggie/`, readable by you only, and used from DOGGIE's next start. `doggie logout` forgets it.

## Quick start

From any directory:

```sh
doggie
```

It opens a chat with DOGGIE, and starts DOGGIE first if it isn't running on this machine; quitting the chat (Ctrl+D) stops the DOGGIE it started. `doggie` alone is `doggie chat`, and takes its flags.

To run DOGGIE on its own, so chats come and go:

```sh
doggie run
```

In a terminal, `doggie run` opens a chat with DOGGIE right there,
 and its logs go to `~/.local/share/doggie/doggie.log`. Ctrl+D quits the chat and stops DOGGIE. To print the logs instead, e.g. under a service manager, use `doggie run --no-chat`.

### Talk to it

The chat fills the terminal. Type a sentence and press Enter, and keep typing while DOGGIE answers or works: replies flow in as they are written, the tasks in progress show in a panel on the right, and notices arrive when they happen:

```
 ◆ DOGGIE   ● online · terminal                                          Wed Oct 28 · 21:40
                                                                    │
                     Remind me in 10 seconds to stretch, and tell   │ TASKS
                     me a short joke meanwhile.                     │
                                                                    │ ● t1 Remind to stretch
  DOGGIE                                                            │   ██████████─────────  50%
  ╭──────────────────────────────────────────────────────────╮      │   5/10 s
  │ On it: the timer is running. Meanwhile, a joke: why did  │      │
  │ the math book look sad? It had too many problems.         │      │
  ╰──────────────────────────────────────────────────────────╯      │
 ╭──────────────────────────────────────────────────────────────────────────────────────────╮
 │ ›                                                                                        │
 ╰──────────────────────────────────────────────────────────────────────────────────────────╯
  /help commands · ↑ history · Ctrl+D quit                                       Tab: panel
```

- What you say goes on the right; DOGGIE's replies on the left, in violet bubbles, as markdown (**bold**, *italic*, `code`, lists, blocks of code). While it thinks, three dots move below the last message, with what it is doing.
- The input box takes several lines: Alt+Enter or Ctrl+J start a new one. A command being typed shows the rest of it in grey; Tab or Right takes it.
- Page Up and Page Down, Shift+Up and Shift+Down, Ctrl+Home and Ctrl+End, and the mouse wheel scroll the conversation. Scrolled up, the view stays where it is and tells how many messages came below.
- Drag with the mouse to mark text: letting go copies it, without the bubbles' frames. Shift-drag (Option-drag in iTerm2) selects with the terminal instead.
- The panel is open from the start, with the tasks and the last notices in rounded cards; Tab hides or shows it. It widens on wide terminals, and under 100 columns it goes and the tasks show below the box instead. A task that waits for your answer is amber.
- The chat's own words follow `language` in the config (English and Spanish for now). Colors follow the terminal: 24-bit with `COLORTERM=truecolor`, 256 or the terminal's own 16 otherwise, and none with `NO_COLOR` set.
- At rest the chat uses no CPU: it draws only what changed, and only when something did. Leaving it puts the terminal back as it was, with one line telling whether DOGGIE stopped or keeps running.

Piped, without a terminal, the chat reads lines and prints plain `doggie>` lines instead.

On opening, the chat shows the last 5 turns of the conversation (`-history`, `0` for none).

Commands start with `/` and are run by the chat itself, without the LLM:

| Command | What it does |
|---|---|
| `/tasks` | The tasks in progress; `/tasks all` adds the last ones that ended |
| `/answer <id> [text]` | Answer a task's question yourself. The only way for the questions DOGGIE may not answer for you, e.g. a confirmation a sensitive task asks for. Without the text, the chat asks for it unseen, as for a password, e.g. a PIN. Answers are never kept for Up and Down |
| `/cancel <id> [why]` | Cancel a task |
| `/help` | The list of commands |
| `//text` | Say text that starts with `/` |

More ways to talk to it:

```sh
doggie                                       # another chat, e.g. from another terminal
doggie say "What time is it in Tokyo?"       # one sentence to a running DOGGIE, then its replies
echo "Tell me a short joke" | doggie         # piped: each line, then the replies
```

Chats on the same channel share the conversation: each one shows what the others say.

**One DOGGIE per data dir.** If none runs on this machine, `doggie chat` starts one in its own process, and stops it when you quit. If DOGGIE goes away without being stopped on purpose (it crashed, or the chat running it closed), a chat still open takes it over. `doggie run` takes DOGGIE over from a chat that runs it, and the chat stays open.

### Tasks

Start, follow and stop tasks yourself, without the brain. `task start` prints the new task's ID (`t1`, `t2`, ...):

```sh
doggie task start demo.sleep '{"seconds": 30}'
doggie task list -active
doggie task get <id>
doggie task cancel <id>
doggie task start demo.ask '{"question": "Which color?", "options": ["red", "blue"]}'
doggie task answer <id> blue
```

Watch everything that happens on the bus, from 10 minutes ago and then live:

```sh
doggie events -since 10m
```

### Satellites

A satellite gets its own token and may only speak for its channels and run its kinds of tasks, as a worker named after it. The token is shown once and saved for this machine:

```sh
doggie satellite add -channel voice:kitchen kitchen
doggie satellite add -kind lights.set -kind lights.off lights
doggie satellite list
doggie say -as kitchen -channel voice:kitchen "Hello from the kitchen"
doggie satellite remove kitchen
```

A running DOGGIE applies the changes within seconds, without a restart. The shared token, used by `say`, `task` and `events` by default, may only speak for the `terminal` channel, and runs no tasks.

### Where things live

| What | Where |
|---|---|
| Config | `~/.config/doggie/config.yaml` (`$XDG_CONFIG_HOME`) |
| Data: event log, bus token, TLS CA, satellites, `doggie.log` | `~/.local/share/doggie/` (`$XDG_DATA_HOME`) |

To connect from another machine, set `bus.listen` to `0.0.0.0:4222` in the config, copy `tls/ca.pem` from the data dir, and pass the token and CA through the environment:

```sh
DOGGIE_BUS_USER=kitchen DOGGIE_BUS_TOKEN=<token> DOGGIE_BUS_CA=ca.pem \
  doggie say -server tls://<doggie-host>:4222 -channel voice:kitchen "Hello"
```

For the shared token, leave out `DOGGIE_BUS_USER` and pass the contents of `bus.token` from the data dir.

## Architecture

- A modular monolith in Go with an embedded NATS event bus. Components never import each other, so any of them can move to its own process. Heavy or fragile pieces run as satellites.
- `doggie run` starts every component: `heartbeat`, `tasks` (the task manager), `demo` (demo tasks) and `brain`. Pick some with `--only` or `--except`.
- A three-level brain: local reflexes → learned skills → LLM (through official CLIs, with tools exposed over MCP). Only the LLM level exists so far.

Planned: home control through Home Assistant, a local and free voice pipeline (VAD, wake word, turn detection, STT, TTS), WhatsApp and a web HUD.

## Development

```sh
make build   # bin/doggie, to try changes without installing
make lint    # gofmt + go vet
make test    # go test -race ./...
```

Tests that run the real `claude` CLI spend your subscription, so they are opt-in:

```sh
DOGGIE_REAL_CLAUDE=1 go test -run Real ./...
DOGGIE_REAL_CLAUDE=1 go test -run TestRealLatency -v ./internal/brain/   # latency report
```

## License

[MIT](LICENSE)
