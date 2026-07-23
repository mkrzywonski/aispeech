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
- **Privacy-first** capture: the microphone is only hot on demand (push-to-talk)
  or in an explicit, opt-in constant-listen mode.
- A **positive-authorization** handshake so no local process can silently feed
  the AI or siphon transcribed audio.
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
- **Web UI server** — serves the local GUI (embedded assets) and a small control
  API (device/voice selection, focus, session list, pairing approval display).
- **Audio engine** — cross-platform capture and playback via `malgo`
  (miniaudio: WASAPI on Windows, ALSA/PulseAudio on Linux). Owns device
  selection and the microphone on/off lifecycle.
- **STT** — `whisper.cpp` invoked as a child process (or via its Go bindings).
  VAD-based endpointing (Silero VAD or equivalent) to segment utterances within
  a listening dialog.
- **TTS** — `piper` invoked as a child process; text in, WAV out, played via the
  audio engine.
- **Router** — normalizes a transcript, matches a leading **session-word**
  against session names, sets focus, and delivers the command to the focused
  session (or drops it, per §6).
- **Controller / focus state machine** — the single source of truth for focus,
  listening mode, mic state, and per-session listening status.

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

### Push-to-talk dialogs (default)

- The user presses PTT to **open a listening dialog**. The mic goes hot and a
  prominent mic-on indicator appears in the UI.
- Within the dialog, utterances are endpointed by VAD; the user can issue
  multiple commands hands-free.
- The dialog **times out** after a configurable idle period (default ~3 min),
  after which the mic goes cold. Activity resets the timer.
- A **hold-to-talk** variant (record only while held) is available as an option
  for the most conservative privacy posture.

### Constant-listen mode (opt-in, per session)

- The mic stays hot indefinitely. Off by default; must be explicitly enabled.
  The mic-on indicator is always visible while active.

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
- A suggested `CLAUDE.md` etiquette line will steer the agent: *speak short
  confirmations and answers; let detail scroll in the TUI.*

---

## 5. MCP tool contract

Server registered as `voice` (tools appear to the agent as `mcp__voice__*`).
All tools except `pair` require the session to be **paired** (see §7); calls
before pairing return an `unpaired` result with guidance.

| Tool | Params | Returns | Notes |
|---|---|---|---|
| `pair` | `code: string` | `{ ok, session_id, name }` or error | Authorizes this connection. `name` defaults to the MCP `clientInfo` name (e.g. `claude`); user-renamable in the UI. |
| `listen` | `timeout_seconds?: int` | `{ text, session }` on an utterance routed here; `{ status: "timeout" }` otherwise | **Long-poll.** Blocks until an utterance is endpointed and routed to *this* focused session, or timeout. Emits MCP `progress` notifications as keepalive. On timeout the agent may call again or stop. |
| `speak` | `text: string` | `{ ok, spoken_chars, truncated }` | Enqueues TTS to the selected output; returns after playback (bounded). Enforces the character cap. |
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

### Positive-authorization pairing (human → AI → hub)

The pairing secret flows **from the human, through the AI's TUI, back to the
hub** — which is precisely what proves the person driving the UI also controls
the TUI in front of them.

1. The agent connects and (via any tool) is told it is **unpaired**: *"Ask the
   user to read you the pairing code shown in the aispeech UI, then call
   `pair(code)`."* The hub creates a **pending** session and shows its code in
   the UI, labeled with the MCP `clientInfo` name (e.g. `claude`).
2. The user reads the code from the UI and tells the agent in its TUI.
3. The agent calls `pair(code)`.
4. On match, the hub authorizes the connection and issues a **memory-only bearer
   token**. Only that connection, presenting the token, is the authorized
   session.

A rogue client can call `pair()` but cannot make the user type the UI's code into
*it*, so it is never authorized — it receives no audio and reaches no agent.

- Codes: 8-char base32, **short expiry**, **limited attempts** before
  regeneration (brute-force resistance on localhost).
- Tokens are memory-only; reconnects re-challenge (aish-style). Nothing persists
  to disk.

### Additional controls

- **Bind narrowly.** Localhost only on Linux/PowerShell. For WSL, prefer
  **mirrored networking** so the bind stays `127.0.0.1`; otherwise bind to the
  WSL adapter host IP only — **never `0.0.0.0`**. (See §8.) Network binding is
  defense-in-depth; the pairing/token is the real gate.
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

The GUI and audio must run where the audio devices are: **on the Windows host**
(WSL2 microphone passthrough via WSLg is unreliable). The agent may run on the
host (PowerShell) or in WSL2; both connect to the same host-side hub.

| Agent location | Hub location | Transport reach |
|---|---|---|
| Linux (native) | same host | `127.0.0.1:PORT` |
| Windows PowerShell | Windows host | `127.0.0.1:PORT` |
| Windows WSL2 (**mirrored** networking) | Windows host | `127.0.0.1:PORT` (localhost shared) — **recommended** |
| Windows WSL2 (NAT networking) | Windows host | Windows reachable via the WSL default-gateway IP; hub binds to the WSL adapter host IP only |

**Recommendation:** enable WSL2 **mirrored networking** (`networkingMode =
mirrored`) so the agent in WSL reaches the hub at plain `localhost` and the hub
binds only to `127.0.0.1`. The doc/install flow will detect and guide this.

---

## 9. Configuration

Persisted config (with sane defaults):

- Audio input device, output device.
- STT: model path/size (whisper.cpp), language.
- TTS: piper voice model, rate/volume.
- PTT: mode (dialog vs hold), dialog idle timeout (default ~3 min).
- `speak()` character cap.
- Server port; WSL networking note.
- Per-session: display name (session-word), constant-listen toggle.

---

## 10. Phased build plan

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

- **Global PTT hotkey.** Page-focused PTT ships first. A cross-platform global
  hotkey needs a native helper; Wayland/NixOS is the hard case. Evaluate an
  optional always-on-top mic widget or a HID foot-pedal path.
- **VAD choice.** Silero VAD (ONNX) vs `webrtcvad` vs whisper.cpp's built-in
  endpointing — pick during Phase 1 on latency/accuracy.
- **whisper.cpp: child process vs Go bindings.** Start with child process for
  packaging simplicity; revisit bindings if latency demands it.
- **speak() return timing.** Return after playback (bounded) vs fire-and-forget
  with a status poll — decide during Phase 2 from how the agent loop feels.
- **Barge-in.** Interrupting TTS by speaking; deferred but keep the audio engine
  design compatible.
- **Project name / branding.** "aispeech" is a working title.
