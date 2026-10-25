# DOGGIE 🐕

A self-hosted, Jarvis-style personal assistant written in Go.

DOGGIE talks with you, runs many tasks in parallel without blocking the conversation, controls your home and learns how to do new things.

> **Status:** early. DOGGIE already talks over the terminal, starts tasks while it keeps talking and tells you when they end. Voice, home devices and WhatsApp come next.

## Goals

- **Never blocks the conversation.** Every action is an async task. Keep talking while DOGGIE works, and it tells you when it's done.
- **Interrupts like a human.** Notices are delivered at natural pauses, phrased in context.
- **Learns.** What it figures out once with an LLM, it replays directly next time.
- **One assistant, many channels:** voice at home, WhatsApp on the go, a web HUD and a terminal.
- **Yours.** Self-hosted. Your configuration and data live outside the code.

## What works today

- **Conversation.** The brain runs on the official [Claude Code](https://docs.anthropic.com/en/docs/claude-code) CLI with your own subscription. DOGGIE builds the whole context of every turn (persona, active tasks, recent turns), so the CLI process is disposable. A warm process waits ahead, and replies are streamed: the first words arrive in about a second.
- **Tasks that don't block.** Ask for something and DOGGIE starts a task, keeps talking, and tells you when it ends, fails or needs an answer. Tasks report progress, can be cancelled and time out. For now, demo tasks (`demo.sleep`, `demo.ask`) stand in for real devices.
- **Locked-down LLM.** The LLM has no shell, files or web. It can only use DOGGIE's tools, and each one declares a risk level that the code enforces, not the prompt.
- **Event bus.** An embedded NATS server with a persistent event log (7 days by default), TLS from DOGGIE's own CA and token auth. Every component talks only over the bus.
- **Restarts lose nothing.** Interrupted tasks are reported as failed, and sentences a restart left unanswered are answered after it.
- **Satellites.** Other processes or machines (a voice satellite, a chat bridge) connect with their own token, and each may only speak for its own channels.

## Requirements

- Linux or macOS.
- [Go](https://go.dev/dl/) 1.27.1 or newer.
- The `claude` CLI, logged in with a Claude subscription: run `claude` once and log in. DOGGIE removes `ANTHROPIC_API_KEY` from the CLI's environment, so it always uses that login.

## Quick start

Build and configure:

```sh
git clone https://github.com/andr2116d/doggie-assistant.git
cd doggie-assistant
make build
mkdir -p ~/.config/doggie
cp configs/doggie.example.yaml ~/.config/doggie/config.yaml
```

Edit `~/.config/doggie/config.yaml`: your name and profile, the assistant's persona, the language it speaks (`en`, `es`, ...). Without the file, DOGGIE starts with defaults.

Run DOGGIE and leave it running:

```sh
./bin/doggie run
```

### Talk to it

From another terminal:

```sh
./bin/doggie say "Hi! What can you do?"
./bin/doggie say "Open Netflix on the TV, it takes 5 seconds, and tell me a joke meanwhile"
```

`say` prints the replies as they are written, then keeps listening for 20 s (`-wait`) for notices about the tasks they started:

```
doggie> On it.
doggie> Netflix is on its way. Meanwhile: I told the TV a joke about a sci-fi movie, but it was too far ahead of its time. It hasn't gotten the punchline yet.
doggie> Netflix is open on the TV, Alex. Enjoy the show, and I hope it's something with spaceships.
```

### Tasks

Start, follow and stop tasks yourself, without the brain. `task start` prints the new task's ID (`t1`, `t2`, ...):

```sh
./bin/doggie task start demo.sleep '{"seconds": 30}'
./bin/doggie task list -active
./bin/doggie task get <id>
./bin/doggie task cancel <id>
./bin/doggie task start demo.ask '{"question": "Which profile?", "options": ["Alex", "Kids"]}'
./bin/doggie task answer <id> Alex
```

Watch everything that happens on the bus, from 10 minutes ago and then live:

```sh
./bin/doggie events -since 10m
```

### Satellites

A satellite gets its own token and may only speak for its channels. The token is shown once and saved for this machine:

```sh
./bin/doggie satellite add -channel voice:kitchen kitchen
./bin/doggie satellite list
./bin/doggie say -as kitchen -channel voice:kitchen "Hello from the kitchen"
./bin/doggie satellite remove kitchen
```

A running DOGGIE applies the changes within seconds, without a restart. The shared token, used by `say`, `task` and `events` by default, may only speak for the `terminal` channel.

### Where things live

| What | Where |
|---|---|
| Config | `~/.config/doggie/config.yaml` (`$XDG_CONFIG_HOME`) |
| Data: event log, bus token, TLS CA, satellites | `~/.local/share/doggie/` (`$XDG_DATA_HOME`) |

To connect from another machine, set `bus.listen` to `0.0.0.0:4222` in the config, copy `tls/ca.pem` from the data dir, and pass the token and CA through the environment:

```sh
DOGGIE_BUS_USER=kitchen DOGGIE_BUS_TOKEN=<token> DOGGIE_BUS_CA=ca.pem \
  ./bin/doggie say -server tls://<doggie-host>:4222 -channel voice:kitchen "Hello"
```

For the shared token, leave out `DOGGIE_BUS_USER` and pass the contents of `bus.token` from the data dir.

## Architecture

- A modular monolith in Go with an embedded NATS event bus. Components never import each other, so any of them can move to its own process. Heavy or fragile pieces run as satellites.
- `doggie run` starts every component: `heartbeat`, `tasks` (the task manager), `demo` (demo tasks) and `brain`. Pick some with `--only` or `--except`.
- A three-level brain: local reflexes → learned skills → LLM (through official CLIs, with tools exposed over MCP). Only the LLM level exists so far.

Planned: home control through Home Assistant, a local and free voice pipeline (VAD, wake word, turn detection, STT, TTS), WhatsApp and a web HUD.

## Development

```sh
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
