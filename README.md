# aispeech — give your AI a voice

[![License: GPL-3.0-only](https://img.shields.io/badge/License-GPL--3.0--only-blue.svg)](LICENSE)

Give your terminal AI coding agents a **voice** — speech-to-text in, terse
text-to-speech out — over MCP, managed from a small local web UI. Multiple
agents (Claude Code, Codex, Gemini) can share one hub, and you direct speech to a
specific one by saying its name.

aispeech **extends** the agent's native TUI instead of replacing it: it's just an
MCP server the agent connects to, like any tool. You keep using Claude Code (or
any MCP client) exactly as before; aispeech adds a few voice tools — chiefly
`converse()` (speak a reply and wait for the next command), plus `listen()`,
`speak()`, and `play_sound()` — behind a one-time `pair` step. Speech runs
**fully local** (whisper.cpp + piper); no cloud, no accounts.

> Status: early but working. Linux is the primary target (packaged for NixOS);
> the Go code is cross-platform. Claude Code is the best-tested client.

---

## How it works

- aispeech runs a persistent **MCP server over HTTP** plus a **browser UI** on
  `127.0.0.1:7071`.
- Each agent connects and, after a one-time **pairing** step, gains the voice
  tools — `converse()` / `listen()` block until you speak a command routed to
  that session; `speak()` says a short reply; `play_sound()` plays a notification.
- **Push-to-talk**: click the mic (or press **Space**) to start; the mic stays
  hot while you talk and goes cold after a configurable idle timeout. The speaker
  row has **mute** (persisted across reloads) and **pause/resume** for a too-loud
  reply.
- **Session-word routing**: each agent's name doubles as a wake word. Say
  *"Claude, run the tests"* and *"Codex, open a PR"* and each utterance routes to
  the matching session. Focus is sticky until you switch it. Speech to a
  non-listening session is dropped (never queued), flagged in the UI.
- **Distinct voices**: each connected agent is auto-assigned its own TTS voice
  (in an order you control), so you can tell them apart by ear.

Everything is visible and controllable in the UI: connected sessions, pairing,
focus, a live transcript of what was recognized and where it routed, audio
devices/levels, notification sounds, and model/voice management.

---

## Requirements

- **Linux** with a running audio server (PipeWire/PulseAudio), or Windows via
  **WSL2** (see [Windows / WSL2](#windows--wsl2)).
- **[whisper.cpp](https://github.com/ggml-org/whisper.cpp)** (`whisper-cli`) for STT.
- **[piper](https://github.com/rhasspy/piper)** for TTS.
- To build from source: **Go ≥ 1.25** and a C compiler (cgo, for audio).

The Nix flake bundles the engines and audio libraries for you.

---

## Install & run

### Nix (recommended on NixOS)

```sh
nix run github:mkrzywonski/aispeech          # run it
# or develop against it (puts audio libs + engines on PATH for `go run`):
nix develop && go run ./cmd/aispeech
```

The flake bundles `whisper-cpp` and `piper-tts` and wires up audio. On any other
distro, follow **From source**.

### From source

You need a **Go ≥ 1.25** toolchain, a **C compiler** (aispeech uses cgo for
audio), and the **ALSA + PulseAudio client libraries** (loaded at runtime — the
`-dev` headers are not required). Then the two speech engines below.

**1. System packages**

```sh
# Debian / Ubuntu
sudo apt install build-essential libasound2 libpulse0
#   On Ubuntu 24.04+ libasound2 is provided by 'libasound2t64'.
#   Debian/Ubuntu's packaged Go is usually older than 1.25 — install the
#   official tarball instead (see note below).

# Fedora / RHEL / derivatives
sudo dnf install golang gcc alsa-lib pulseaudio-libs

# Arch
sudo pacman -S go base-devel alsa-lib libpulse
```

> If your distro's Go is older than 1.25, install the official toolchain:
> ```sh
> curl -LO https://go.dev/dl/go1.25.0.linux-amd64.tar.gz
> sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.25.0.linux-amd64.tar.gz
> export PATH=$PATH:/usr/local/go/bin      # add to ~/.profile to persist
> ```

**2. Build**

```sh
git clone https://github.com/mkrzywonski/aispeech.git
cd aispeech
go build -o aispeech ./cmd/aispeech
```

**3. Speech engines** — `whisper-cli` and `piper` are **not** in distro repos.
aispeech looks for them on your `PATH` (override the paths in Settings → Models →
Advanced).

```sh
# piper (TTS): grab the prebuilt Linux binary from
#   https://github.com/rhasspy/piper/releases   (piper_linux_x86_64.tar.gz)
tar -xzf piper_linux_*.tar.gz                 # creates ./piper/
export PATH=$PATH:$PWD/piper                  # keep the binary beside its libs

# whisper.cpp (STT): build from source — produces a 'whisper-cli' binary
git clone https://github.com/ggml-org/whisper.cpp
cd whisper.cpp && cmake -B build && cmake --build build --config Release -j
sudo install build/bin/whisper-cli /usr/local/bin/
```

> Older whisper.cpp checkouts name the CLI `main`; symlink it to `whisper-cli`
> or point Settings → Models → Advanced at it directly.

**4. Run**

```sh
./aispeech
```

Then open **http://127.0.0.1:7071**. You do **not** need to fetch speech models
manually — aispeech downloads them from a built-in catalog (see First run).

---

## First run

1. **Install speech models** (Settings → Models): pick a whisper model (e.g.
   *base.en*, 142 MB) and a piper voice (e.g. *en_US-lessac-medium*, 63 MB) from
   the built-in catalog and click **Download** — they install and activate
   without a restart. (Or point at existing files under *Advanced*.)
2. **Connect an agent** (Settings → Connect AI agents): click **Install** for
   Claude Code / Codex / Gemini. This registers the `aispeech mcp-proxy` stdio
   bridge in the agent's config.
3. **Restart the agent** and ask it to use voice. It appears at the top of the UI
   as *waiting to pair*. Click **Copy pairing token**, paste the token to the
   agent in its terminal, and it calls `pair` — connected.
4. Click the **mic** (or press **Space**) and talk.

---

## Connecting agents (details)

aispeech registers a small stdio bridge — `aispeech mcp-proxy` — into each
agent's MCP config; each agent spawns its own proxy, which forwards to the shared
hub over HTTP. This works with any MCP client regardless of its HTTP support. The
`--name` it's registered with becomes the agent's wake word. The GUI sets this up
for you; the equivalent manual command for Claude Code is:

```sh
claude mcp add aispeech --scope user -- aispeech mcp-proxy --name claude --url http://127.0.0.1:7071/mcp
```

Codex (`~/.codex/config.toml`) and Gemini (`~/.gemini/settings.json`) get an
equivalent `command`/`args` entry.

---

## Configuration

Settings persist to `~/.config/aispeech/config.json`; models download to
`~/.local/share/aispeech/models` and custom sounds live in
`~/.local/share/aispeech/sounds`. Most things are editable in the UI:

- Input/output audio devices; speaker volume and **mute** (persisted); mic gain —
  with **Test speaker** / **Test mic** helpers.
- STT model, TTS voice, language, and binary paths — pick or **download** from
  the built-in catalog.
- **Installed voices**: reorder by drag-and-drop (the order sets which agent gets
  which voice), delete, or preview any voice with a spoken sample.
- **Notification sounds**: preview the built-ins (`success`, `error`, `ding`) or
  upload your own WAVs; agents trigger them with `play_sound()`.
- **Disable microphone after** (whole minutes, on the mic row) — how long the mic
  stays hot before turning off.

Flags: `--addr host:port` (override bind address), `--version` / `-v`,
`--dev-inject` (dev-only transcript-injection endpoint for routing tests).

---

## Security

The hub is localhost HTTP, with **browser-bound** authorization:

- The browser gets an `HttpOnly` / `SameSite=Strict` session cookie on load.
- Pairing mints a single-use, short-lived token (hashed at rest) that you copy
  and paste to the agent. No pairing secret is ever readable over an endpoint, so
  a confused or rogue agent can't fetch one and self-pair.
- **Single UI operator**: only one browser session drives the hub at a time.
  Others are refused; if a new browser takes over after the previous operator
  goes idle, the prior operator's pairings are voided so it can't inherit agents
  it didn't pair.
- A **Host allowlist** rejects requests whose `Host` isn't the loopback/bound
  authority (defends against DNS-rebinding), and mutating routes require a
  same-origin request. `listen()` input comes only from the audio→STT pipeline —
  there is no text-injection API.

A process with full user authority (reading your clipboard, or scripting the
browser) can still pair — localhost offers no defense there. See
[DESIGN.md](DESIGN.md) §7 for the full threat model.

---

## Windows / WSL2

Because the UI is a web page, run the hub **inside WSL2** and open
`http://localhost:7071` from a Windows browser (WSL2 forwards `localhost`). Audio
comes from WSLg's PulseAudio; mic-capture reliability varies by setup. Native
Windows/macOS builds compile but are less tested than Linux.

---

## Browser audio (remote / tunnel)

When the hub runs on a different machine than you — over an `ssh -L` tunnel, or in
WSL2 with the browser on Windows — the local audio devices are on the *hub's*
machine. Select **"Browser"** as the Microphone and/or Speaker in Settings →
Audio to use *your* machine's devices instead. Audio streams over a WebSocket on
the same port (tunnel-friendly, no WebRTC), and each direction is independent, so
you can mix a local mic with a browser speaker or vice-versa.

---

## Architecture

```
Browser UI ──┐
             ├─ HTTP ─ aispeech hub ─ MCP (HTTP) ─┬─ Claude Code (+ mcp-proxy)
audio ── malgo ┤        │  session registry        ├─ Codex        (+ mcp-proxy)
STT  ── whisper.cpp     │  wake-word router         └─ Gemini       (+ mcp-proxy)
TTS  ── piper           └  converse() / speak() / listen()
```

- `internal/session` — agent sessions, focus, session-word routing, transcript log.
- `internal/engine` — capture + VAD, whisper/piper drivers (hot-swappable).
- `internal/browseraudio` — mic/speaker audio over a WebSocket for remote/tunnel use.
- `internal/mcpserver` — the MCP tool contract over Streamable HTTP.
- `internal/modelstore` — model/voice catalog + progress-tracked downloader.
- `internal/mcpinstall` + `cmd/aispeech` (`mcp-proxy`) — agent install & bridge.
- `internal/authz` — browser sessions, pairing tokens, single-operator gate, Host allowlist.
- `internal/web` — control UI and JSON API.

See [DESIGN.md](DESIGN.md) for the full design rationale.

---

## Related

Cousin project: **[aish](https://github.com/mkrzywonski/aish)** — an AI-shared
terminal. Same "extend the TUI, don't replace it" philosophy.

---

## License

GPL-3.0-only. See [LICENSE](LICENSE).
