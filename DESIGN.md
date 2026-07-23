# aispeech — Design Document

*Working title. A local, vendor-agnostic voice layer for terminal AI agents.*

---

## 1. Summary

**aispeech** is a standalone application that gives any MCP-capable AI coding
agent (Claude Code first; Codex and Gemini later) two new capabilities —
**`speak()`** and **`listen()`** — over a local MCP server. It provides a
browser-based GUI for audio device selection, voice/engine configuration, and
live management of the connected AI sessions.

It follows the same philosophy as its cousin **aish**: *sit beside the vendor's
native TUI and extend it — never replace it.* aispeech does not wrap a PTY, does
not render a frontend, and does not try to be a screen reader. The AI keeps its
native TUI exactly as-is; aispeech simply adds a spoken output channel and a
spoken input channel alongside the keyboard.

Speech-to-text and text-to-speech run **locally**, with no cloud dependency.

---

## 2. Goals and non-goals

### Goals

- Add `speak()` and `listen()` to an unmodified AI agent via MCP.
- Run the GUI on **Windows** (agent TUI under PowerShell *or* WSL2) and on
  **Linux** (NixOS and others).
- **Local-only** STT and TTS.
- Route spoken input to the correct session by a **session-word** prefix, with
  sticky focus.
- **Privacy-first** capture: the microphone is only hot on demand — click the
  mic (or press Space) to start; it goes cold after a configurable idle timeout.
  Persistent mute and a pause/resume control for speech output.
- **Browser-bound authorization** so a local process (or a confused agent that
  `curl`s the hub) cannot obtain a pairing secret, start listening, or inject
  voice, plus a Host allowlist against DNS-rebinding web pages.
- Single-binary distribution per platform (goreleaser + nix flake, as in aish).

### Non-goals

- No PTY wrapping / no injecting characters into the TUI input line.
- No screen-reading of everything the AI prints. `speak()` voices only terse,
  agent-chosen responses.
- No cloud STT/TTS in v1.
- No merge or shared fate with aish (inspiration only).
- No global multi-machine networking; localhost / same-host + WSL only.

---

## 3. Architecture

```
                        ┌───────────────────────────────────────────┐
                        │                 aispeech                    │
                        │                                             │
   Browser (local UI) ──┼──▶ Web UI server (HTTP, embedded assets)    │
                        │            │                                │
                        │            ▼                                │
                        │      Controller / focus state machine       │
                        │        │            │            │          │
                        │        ▼            ▼            ▼          │
                        │   Audio engine   Router      MCP server     │
                        │   (malgo)        (session-   (Streamable    │
                        │    │   ▲          word match) HTTP)         │
                        │    ▼   │              │           ▲         │
                        │  STT   TTS            │           │         │
                        │ (whisper (piper       │           │         │
                        │  .cpp)   binary)      │           │         │
                        └────────┼──────────────┼───────────┼─────────┘
                                 │              │           │
                              speakers/mic   focus/UI   MCP (localhost TCP)
                                                            │
                                                    ┌───────┴────────┐
                                                    │  AI agent(s)   │
                                                    │  (Claude Code) │
                                                    │  native TUI    │
                                                    └────────────────┘
```

### Components

- **MCP server** — Streamable HTTP transport (multi-client, persistent). Exposes
  the tool contract in §5. Each connected agent is one *session*.
- **Web UI server** — serves the local GUI (embedded assets) and the control API
  (device/level selection, focus, session list, pairing-token issuance, model
  download, agent install). Mutating routes are guarded (§7).
- **Audio engine** — cross-platform capture and playback via `malgo`
  (miniaudio: WASAPI on Windows, ALSA/PulseAudio on Linux). Owns device
  selection, the microphone on/off lifecycle, and playback (live volume/mute,
  interruptible for pause).
- **STT** — `whisper.cpp` invoked as a child process. Energy-based VAD endpoints
  utterances within a listening dialog (Silero VAD is a possible future upgrade).
- **TTS** — `piper` invoked as a child process; text in, WAV out, played via the
  audio engine. `speak()` calls are serialized FIFO.
- **Router** — normalizes a transcript, matches a leading **session-word**
  against session names, sets focus, and delivers the command to the focused
  session (or drops it, per §6).
- **Model store** (`modelstore`) — a curated catalog of whisper/piper models with
  a progress-tracked, atomically-installing downloader; engines hot-reload when a
  model is selected or downloaded.
- **Agent install** (`mcpinstall`) + **`aispeech mcp-proxy`** — registers a stdio
  bridge in each agent's config (Claude Code, Codex, Gemini) that forwards to the
  shared HTTP hub; the GUI drives install/remove.
- **Authorization** (`authz`) — the browser-session principal, single-use pairing
  tokens, and the Host/Origin allowlist (see §7).

### Language and stack

**Go.** Rationale:

- One static binary per platform; reuse aish's goreleaser + nix flake patterns,
  so NixOS and Windows distribution are already solved paths.
- Cross-platform audio via `malgo` with no per-OS branching in app code.
- Local STT/TTS by shelling out to `whisper.cpp` and `piper` binaries — no Python
  runtime, no ML packaging pain on NixOS or Windows.
- The MCP server and the web UI are both just HTTP servers; UI assets embed via
  `embed.FS` for a true single-file ship.

---

## 4. Interaction model

### Push-to-talk dialogs

- The user **clicks the mic icon** (or presses **Space**) to toggle listening.
  The mic turns red (its off-state shows a red circle-with-slash), a page-level
  border appears, and the tab title/favicon go red — an unmissable "hot" cue.
- Within the dialog, utterances are endpointed by VAD; the user can issue
  multiple commands hands-free.
- The dialog **times out** after a configurable idle period (a whole-minute field
  in the mic row; default 3 min); activity resets the timer.
- The engine also has an always-on "constant" mode; it is not currently surfaced
  in the UI. A true global (unfocused) hotkey and a hold-to-talk variant remain
  future work (§11).

> **PTT hotkey scope (v1 limitation).** A browser page can only capture a hotkey
> while focused. v1 ships **page-focused PTT** (button / spacebar in the UI). A
> true global hotkey needs OS-level registration (Windows `RegisterHotKey`, X11
> grabs; **Wayland/NixOS is compositor-specific and hard**) and is deferred to a
> future optional native helper. See §11.

### Session-word routing and focus

- Session names double as **session-words**. If an utterance begins with a
  session name (normalized match), the remainder is the command and that session
  becomes the **focused** session.
- A bare session-word ("Claude") with no remainder is a **focus switch only**.
- Focus is **sticky**: it stays on the last-selected session until changed by
  another session-word or by a selection in the UI. Subsequent commands need no
  prefix.
- There is **no wake-word to activate the mic** — activation is PTT/constant
  mode. Session-words only *route and switch focus* once already listening.

### Speaking

- `speak()` voices **terse, agent-chosen** text — not everything printed. The
  tool description instructs brevity; a hard character cap (config) enforces it.
- Concurrent `speak()` calls are **serialized FIFO** — one audio output can't
  play two replies at once.
- The speaker row has a **mute** toggle (click the speaker icon; persisted across
  restarts, so a reload never blares) and a **pause/resume** control that cuts
  the current utterance and holds further output. Volume and mute apply live,
  mid-utterance.
- A suggested `CLAUDE.md` etiquette line will steer the agent: *speak short
  confirmations and answers; let detail scroll in the TUI.*

---

## 5. MCP tool contract

Server registered as `aispeech` (tools appear to the agent as `mcp__aispeech__*`).
All tools except `pair` require the session to be **paired** (see §7); calls
before pairing return an error with guidance.

| Tool | Params | Returns | Notes |
|---|---|---|---|
| `pair` | `token: string` | `{ ok, session_id, name }` or error | Authorizes this connection with a browser-issued token the user copies from the UI and pastes in (see §7). Failed attempts are rate-limited. `name` defaults to the MCP `clientInfo` name (e.g. `claude`); user-renamable in the UI. |
| `converse` | `text: string`, `timeout_seconds?: int` | `{ status, text, session }` | **Speak-then-listen** in one call — speak `text`, then wait for the next command. The natural way to stay in a voice dialog; the server's `instructions` steer the model to call it before ending each turn. |
| `listen` | `timeout_seconds?: int` | `{ status, text, session }` | **Long-poll.** Waits for an utterance routed to *this* focused session, or a `timeout`/`cancelled` status. Use when there's no spoken reply to give yet. |
| `speak` | `text: string` | `{ ok, spoken_chars, truncated }` | Speak without waiting. FIFO-serialized to the selected output; returns after playback. Enforces the character cap. |
| `play_sound` | `sound?: string`, `file?: string` | `{ ok, played }` | Play a notification sound: a built-in (`chime`, `success`, `error`, `alert`, `alarm`, `ding`) or a WAV file path. Respects volume/mute. |
| `end_session` | — | `{ ok }` | Drops this voice channel; releases focus if held. The agent's escape hatch. |
| `status` | — | `{ paired, name, focused, listening_mode, mic_active, other_sessions }` | Lets the agent reason about whether speaking/listening is currently useful. |

### `listen()` semantics

`listen()` returns a transcript **only when all hold**:

1. this session is **paired**,
2. this session currently holds **focus**,
3. the user is in a **listening mode** (PTT dialog open or constant), and
4. an utterance is captured and endpointed to this session.

Utterances routed to a session that is **not currently in a `listen()` call**
are **dropped**, and the UI signals "*‹name› isn't listening*" (see §6). No
buffering — a delayed command is worse than a dropped one.

---

## 6. Routing and focus state machine

**Per-session state:** `name`, `paired`, `listening` (has an outstanding
`listen()` call), `focused`.

**Global state:** `listening_mode ∈ {idle, ptt_dialog, constant}`, `mic_active`,
`focused_session`.

**On each endpointed utterance:**

1. Transcribe (whisper.cpp).
2. Normalize; test the leading token(s) against known session names.
   - **Match** → set `focused_session` to that session. `command = remainder`.
     If `remainder` is empty → focus switch only; stop.
   - **No match** → `command = whole utterance`; target = current
     `focused_session`.
3. Deliver `command` to the target:
   - target has an outstanding `listen()` → **fulfill it** (return transcript).
   - target has **no** outstanding `listen()` → **drop** + UI signal
     "*‹name› isn't listening*".
   - **no focused session** → UI signal "*no session selected*".

**Mic lifecycle:** hot only during `ptt_dialog` (until idle timeout) or
`constant`. Always reflected by a UI mic indicator.

**Barge-in** (speaking over TTS to interrupt): deferred to v2.

---

## 7. Security model

### Threat model

The MCP endpoint is localhost TCP (and must be reachable from WSL2). Risks:

- A rogue local process connects as a fake session and **siphons transcribed
  microphone audio** via `listen()`.
- A rogue process **injects prompts** into the AI by getting text into the
  `listen()` channel.
- A rogue process plays arbitrary audio via `speak()`.

### Browser-bound pairing (browser → human → AI → hub)

Superseding an earlier design in which the hub displayed an agent-generated
pairing code in the world-readable `/api/state` (any local process — including a
confused agent that `curl`s the hub — could read the code and self-pair). The
current model makes the **browser UI a separate principal** and never exposes a
pairing secret over a readable endpoint.

1. On first load, `GET /` issues an `HttpOnly`, `SameSite=Strict` browser-session
   cookie. The agent connects over MCP and is told it is **unpaired**: *"Ask the
   user to click 'Copy pairing token' in the aispeech UI and paste it to you."*
2. The user clicks **Copy pairing token**; `POST /api/pair/token` (guarded by the
   browser cookie + Origin) mints a single-use, 128-bit, 5-minute token, stored
   server-side only as a hash and copied to the clipboard.
3. The user pastes the token into the agent's TUI; the agent calls `pair(token)`.
4. The hub atomically consumes the token and binds that MCP session to the
   browser session that issued it. Only then do `listen`/`speak` work.

Because the token is created only by an explicit action in a cookied, same-origin
browser and is never returned by a readable endpoint, a **confused or malicious
agent cannot obtain it by making HTTP requests** — it must be handed the token by
the human. Failed `pair` attempts are rate-limited per connection.

**Threat-model limit:** on a single-user host, a process with full user authority
(reading the clipboard, keystrokes, or scripting the whole browser flow) can
still pair — localhost offers no defense there. What this reliably stops is
accidental agent self-pairing, cross-origin/DNS-rebinding web pages, and passive
reads of a pairing secret.

### Additional controls

- **Host allowlist.** Every request whose `Host` isn't the loopback/bound
  authority is rejected (`authz.HostGuard`) — the primary defense against DNS
  rebinding. Mutating UI routes additionally require a same-origin `Origin` and a
  known browser cookie.
- **Bind narrowly.** Localhost by default; when a WSL-reachable bind is needed,
  bind to that specific host — **never `0.0.0.0`**. Binding is defense-in-depth;
  the Host/Origin allowlist and the pairing token are the real gates.
- **`listen()` input originates only from the audio→STT pipeline.** There is **no
  unauthenticated "inject text" API** on the localhost surface — that would be
  the exact prompt-injection pipeline we refuse to build.
- **Exclusive input focus.** Multiple sessions may be paired, but exactly one
  holds focus and receives the current utterance.
- **Both sides can cut the channel.** The agent can call `end_session()`; the
  user can revoke any session in the UI (aish's `Ctrl-] k` analogue).
- **`speak()` is local-audio-only**, with a character cap so a misbehaving agent
  can't monologue.

---

## 8. Windows / WSL2 topology

Because the control surface is a **web UI**, the hub can run wherever is
convenient and the browser reaches it over `localhost`:

- **Linux / NixOS:** run the hub; open `http://127.0.0.1:7071` locally.
- **Windows + WSL2:** run the hub **inside WSL** and open the UI from a Windows
  browser at `http://localhost:7071` (WSL2 forwards `localhost`). Audio comes
  from WSLg's PulseAudio; microphone-capture reliability varies by setup.
- **Windows native (PowerShell):** run the hub on Windows directly.

The agent connects to the hub the same way in every case — the `mcp-proxy` stdio
bridge, or a direct HTTP MCP client. Wherever the hub binds beyond pure loopback
(e.g. a WSL-reachable interface), the **Host allowlist** (§7) is what keeps the
surface safe; never bind `0.0.0.0` without it.

---

## 9. Configuration

Persisted config (`~/.config/aispeech/config.json`, with sane defaults):

- Bind address; audio input/output device.
- Levels: output volume, input gain, and **muted** (persisted so a reload is
  never loud).
- STT: whisper binary + model path, language. TTS: piper binary + voice path.
- Models directory (downloads land here; default `~/.local/share/aispeech/models`).
- PTT dialog idle timeout; `speak()` character cap.

Session display names (session-words) are set live in the UI, not persisted.

---

## 10. Phased build plan

> **Status:** Phases 0–4 are implemented, plus extras beyond the original plan —
> a model catalog + downloader with hot-reload, GUI agent-install via the
> `mcp-proxy` stdio bridge, mute/pause playback controls, and browser-bound token
> pairing with a Host allowlist (§7). The phases below are kept for context.

**Phase 0 — skeleton.** Go project; embedded web UI shell; MCP Streamable-HTTP
server that a Claude Code session can connect to and list `voice` tools; config
load/save. Reuse aish MCP-server patterns.

**Phase 1 — audio + engines.** `malgo` capture/playback; device enumeration in
the UI; `whisper.cpp` transcription of a captured clip; `piper` playback of a
string. Prove local STT and TTS end to end from the UI (no agent yet).

**Phase 2 — the two tools, single session.** Implement `pair`, `listen`,
`speak`, `end_session`, `status`. Full pairing handshake + token. PTT dialog with
VAD endpointing. Claude Code can pair, `listen()` for a spoken command, act, and
`speak()` a terse reply. **This is the first usable prototype.**

**Phase 3 — routing + focus.** Multiple sessions; session-word matching; sticky
focus; drop-with-signal for non-listening targets; UI session list with focus
selection and revoke. Prove *"Claude, …"* vs *"Codex, …"* routing (both Claude
Code instances at first).

**Phase 4 — hardening + platforms.** WSL2 mirrored-networking flow; Windows
packaging; constant-listen mode; config polish; `speak()` etiquette guidance.

**Later.** Codex (app-server/MCP) and Gemini; global PTT hotkey helper; barge-in;
pluggable cloud engines behind the STT/TTS interface (kept optional).

---

## 11. Open questions / future work

- **Global PTT hotkey.** Page-focused PTT (mic click / Space) ships; a
  cross-platform *global* hotkey and a hold-to-talk variant remain future work
  (Wayland/NixOS is the hard case). Evaluate a native helper or a HID foot-pedal.
- **VAD.** Energy-based endpointing is implemented; Silero VAD (ONNX) is a
  possible accuracy upgrade, especially in noisy rooms.
- **whisper.cpp / piper** run as child processes (chosen for packaging
  simplicity); Go bindings remain a latency option.
- **`speak()`** returns after playback and calls are FIFO-serialized. The
  stop/pause control covers manual interruption; barge-in *by voice* over TTS is
  still future work.
- **Pairing hardening (Phase 2).** Per-tab↔agent binding with focus/revoke
  gating, cross-session transcript filtering, and CSRF tokens.
- **Persistent agent credential** so an agent restart doesn't require re-pairing
  (proxy stores an issued credential in a `0600` file).
- **Project name / branding.** "aispeech" is a working title.
