# aispeech — local voice hub for terminal AI agents

Give your terminal AI coding agents a **voice** — speech-to-text in, terse
text-to-speech out — over MCP, managed from a small local web UI. Multiple
agents (Claude Code, Codex, Gemini) can connect to one hub at once, and you
direct your speech to a specific one by saying its name.

aispeech **extends** the agent's native TUI instead of replacing it: it's an MCP
server the agent connects to, exactly like a tool. You keep using Claude Code
(or any MCP client) the way you always do; aispeech just adds two capabilities —
`listen()` and `speak()` — and a GUI to drive them. Speech runs **fully local**
(whisper.cpp + piper); no cloud, no accounts.

> Status: early but working. Linux is the primary target (packaged for NixOS);
> the Go code is cross-platform. Claude Code is the best-tested client.

---

## How it works

- aispeech runs a persistent **MCP server over HTTP** plus a **browser UI** on
  `127.0.0.1:7071`.
- Each AI agent connects and, after a one-time **pairing** step, gains two tools:
  - **`listen()`** — blocks until you speak a command routed to that session.
  - **`speak(text)`** — says a short reply aloud (agents are asked to keep it terse).
- You talk using **push-to-talk**: press *Start listening*, and the mic stays hot
  while the conversation is active, going cold after a configurable idle timeout.
  (A constant-listen mode is available; the mic is only ever on while you ask.)
- **Session-word routing**: each connected agent has a name that doubles as a
  wake word. Say *"Claude, run the tests"* and *"Codex, open a PR"* and each
  utterance is routed to the matching session. Focus is sticky until you switch
  it (by voice or in the UI). Speech to a session that isn't listening is
  dropped (never queued) with a clear signal in the UI.

Everything is visible and controllable in the web UI: connected sessions,
pairing, focus, a live transcript of what was recognized and where it routed,
audio devices/levels, and model management.

---

## Requirements

- **Linux** with a running audio server (PipeWire/PulseAudio) — or Windows with
  the hub running natively (see [WSL2](#windows--wsl2)).
- **[whisper.cpp](https://github.com/ggerganov/whisper.cpp)** (`whisper-cli`) for STT.
- **[piper](https://github.com/rhasspy/piper)** for TTS.
- To build from source: **Go ≥ 1.25** and a C compiler (cgo, for audio).

The Nix flake bundles `whisper-cpp` and `piper-tts` for you.

---

## Install & run

### Nix (recommended on NixOS)

```sh
nix run github:mkrzywonski/aispeech          # run it
# or develop against it (puts audio libs + engines on PATH for `go run`):
nix develop && go run ./cmd/aispeech
```

### From source

```sh
git clone https://github.com/mkrzywonski/aispeech.git
cd aispeech
go build -o aispeech ./cmd/aispeech
./aispeech
```

Building needs cgo (audio via [malgo](https://github.com/gen2brain/malgo)); it
`dlopen`s ALSA/PulseAudio at runtime, so those client libraries must be on the
loader path. The Nix package/dev shell handle this; on other distros install
`alsa-lib`/`libpulseaudio`.

Then open **http://127.0.0.1:7071**.

---

## First run

1. **Install speech models** (Settings → Models): pick a whisper model and a
   piper voice from the built-in catalog and click **Download** — they install
   and activate without a restart. (Or point at existing files under *Advanced*.)
2. **Connect an agent** (Settings → Connect AI agents): click **Install** for
   Claude Code / Codex / Gemini. This registers the `aispeech mcp-proxy` stdio
   bridge in the agent's config.
3. **Restart the agent** and ask it to use voice. It appears at the top of the
   UI with an 8-character **pairing code** — read that code to the agent (or
   tell it to call `pair`) and it's connected.
4. Press **Start listening** (or the spacebar) and talk.

---

## Connecting agents (details)

aispeech registers a small stdio bridge — `aispeech mcp-proxy` — into each
agent's MCP config. Each agent spawns its own proxy, which forwards to the shared
hub over HTTP. This works with any MCP client regardless of its HTTP support.

The GUI does this for you, but the equivalent manual commands are:

```sh
# Claude Code
claude mcp add aispeech --scope user -- aispeech mcp-proxy --name claude --url http://127.0.0.1:7071/mcp
```

Codex (`~/.codex/config.toml`) and Gemini (`~/.gemini/settings.json`) get an
equivalent `command`/`args` entry.

---

## Configuration

Settings persist to `~/.config/aispeech/config.json` (models download to
`~/.local/share/aispeech/models`). Most things are editable in the UI:

- Input/output audio devices, speaker volume, mic gain (with **Test speaker** /
  **Test mic** helpers).
- STT model, TTS voice, language, and binary paths.
- **PTT timeout** (minutes) — how long the mic stays hot after activity.

Flags: `--addr host:port` (override bind address), `--dev-inject` (enable a
dev-only transcript-injection endpoint for routing tests — never use in normal
operation).

---

## Security

The MCP endpoint is localhost HTTP. To stop a rogue local process from feeding
the agent or siphoning your transcribed microphone audio, every session must
complete a **pairing handshake**: the hub shows a code, you relay it to the agent
in its TUI, and the agent calls `pair(code)`. Only then does `listen()`/`speak()`
work. The pairing binds "the session in my UI" to "the agent in front of me".
Bind only to loopback; there is no unauthenticated text-injection API.

---

## Windows / WSL2

Run the hub **natively on Windows** (where the audio devices are) and let an
agent inside **WSL2** connect over `localhost` — this avoids WSL2's unreliable
microphone passthrough. WSL2 *mirrored* networking keeps the bind on
`127.0.0.1`. (Windows/macOS builds are supported by the Go code but less tested
than Linux.)

---

## Architecture

```
Browser UI ──┐
             ├─ HTTP ─ aispeech hub ─ MCP (HTTP) ─┬─ Claude Code (+ mcp-proxy)
audio ── malgo ┤        │  session registry        ├─ Codex        (+ mcp-proxy)
STT  ── whisper.cpp     │  wake-word router         └─ Gemini       (+ mcp-proxy)
TTS  ── piper           └  speak() / listen()
```

- `internal/session` — pairing, focus, session-word routing, transcript log.
- `internal/engine` — capture + VAD, whisper/piper drivers (hot-swappable).
- `internal/mcpserver` — the MCP tool contract over Streamable HTTP.
- `internal/modelstore` — model catalog + progress-tracked downloader.
- `internal/mcpinstall` + `cmd/aispeech` (`mcp-proxy`) — agent install & bridge.
- `internal/web` — control UI and JSON API.

See [DESIGN.md](DESIGN.md) for the full design rationale.

---

## Related

Cousin project: **[aish](https://github.com/mkrzywonski/aish)** — an AI-shared
terminal. Same "extend the TUI, don't replace it" philosophy.

## License

GPL-3.0-only. See [LICENSE](LICENSE).
