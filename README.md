# DOGGIE 🐕

A self-hosted, Jarvis-style personal assistant written in Go.

DOGGIE talks with you, runs many tasks in parallel without blocking the conversation, controls your home and learns how to do new things.

> **Status:** early design. Nothing runs yet.

## Goals

- **Never blocks the conversation.** Every action is an async task. Keep talking while DOGGIE works, and it tells you when it's done.
- **Interrupts like a human.** Notices are delivered at natural pauses, phrased in context.
- **Learns.** What it figures out once with an LLM, it replays directly next time.
- **One assistant, many channels:** voice at home, WhatsApp on the go, a web HUD and a terminal.
- **Yours.** Self-hosted. Your configuration and data live outside the code.

## Architecture (planned)

- A modular monolith in Go with an embedded NATS event bus. Heavy or fragile pieces run as satellites.
- A three-level brain: local reflexes → learned skills → LLM (via official CLIs, with tools exposed over MCP).
- Home control through Home Assistant.
- A local, free voice pipeline (VAD, wake word, turn detection, STT, TTS).

## License

[MIT](LICENSE)
