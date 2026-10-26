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

- **Conversation.** The brain runs on the official [Claude Code](https://docs.anthropic.com/en/docs/claude-code) CLI with your own subscription. DOGGIE builds the whole context of every turn (persona, active tasks, recent turns), so the CLI process is disposable. A warm process waits ahead, and replies are streamed: the first words arrive in about a second. DOGGIE speaks as itself: asked about it, it names the model it thinks with, but it never repeats what the CLI adds about your account or machine. When your usage limit runs low or runs out, DOGGIE tells you so in your language, with when it renews.
- **Tasks that don't block.** Ask for something and DOGGIE starts a task, keeps talking, and tells you when it ends, fails or needs an answer. Tasks report progress, can be cancelled and time out. For now, demo tasks (`demo.sleep`, `demo.ask`) stand in for real devices.
- **Terminal chat.** Replies stream in above the line you type, which never breaks, with live task status and commands to see, answer and cancel tasks without the LLM.
- **Locked-down LLM.** The LLM has no shell, files or web. It can only use DOGGIE's tools, and each one declares a risk level that the code enforces, not the prompt.
- **Event bus.** An embedded NATS server with a persistent event log (7 days by default), TLS from DOGGIE's own CA and token auth. Every component talks only over the bus.
- **Restarts lose nothing.** Interrupted tasks are reported as failed, and sentences a restart left unanswered are answered after it.
- **Satellites.** Other processes or machines (a voice satellite, a chat bridge) connect with their own token, and each may only speak for its own channels.

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

Type a sentence and press Enter. Keep typing while DOGGIE answers or works: replies show as they are written, notices about tasks arrive when they happen, and the tasks in progress show on a line just above the one you type:

```
you> Remind me in 10 seconds to stretch, and tell me a short joke meanwhile.
doggie> On it.
doggie> Timer's running. Meanwhile: I told my computer I needed a break, and it sent me a vacation of cookies I couldn't refuse. I'll let you know when it's time to stretch.
· t1 Remind to stretch in 10 secon…: 5/10 s (50%)
you> _
```

A moment later the status line is gone and the notice is in. Then a sensitive task, started here with `doggie task start -thread terminal -sensitive demo.ask ...`, asks for a confirmation only you may give:

```
doggie> Time to stretch, Alex. Your ten seconds are up, and the joke was better than the cookies deserved.
· t2 asks: "Pay 40 EUR now?" [yes | no]. Only you may answer it: /answer t2 <answer>
doggie> The electricity bill task is asking whether to pay 40 EUR now. Only you can answer that, so please reply to it yourself, directly, with yes or no. I can't answer it for you.
you> /answer t2 yes
· answered t2
```

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

A satellite gets its own token and may only speak for its channels. The token is shown once and saved for this machine:

```sh
doggie satellite add -channel voice:kitchen kitchen
doggie satellite list
doggie say -as kitchen -channel voice:kitchen "Hello from the kitchen"
doggie satellite remove kitchen
```

A running DOGGIE applies the changes within seconds, without a restart. The shared token, used by `say`, `task` and `events` by default, may only speak for the `terminal` channel.

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
