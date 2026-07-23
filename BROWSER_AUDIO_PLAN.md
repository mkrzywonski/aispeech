# Browser audio backend plan

## Goal

Let the aispeech hub use the **browser's** microphone and speaker as an
additional audio option, so voice works when the hub and the user are on
different machines:

- **Remote / SSH:** the hub runs on a home PC; the user is elsewhere with the web
  UI reached over an `ssh -L` tunnel. Audio should use the *work* PC's devices.
- **Windows + WSL2:** the hub runs in WSL; the browser is on Windows. Browser
  audio removes the unreliable WSLg-mic dependency entirely.

## Principles (decisions)

- **Additive, not a replacement.** Local (malgo) devices stay in the list. The
  browser mic/speaker appear as extra entries — "Browser (this tab)" — in the
  existing Microphone/Speaker dropdowns. Input and output are chosen
  independently.
- **No WebRTC.** Use `getUserMedia` for capture (that API is part of the same web
  media family, usable standalone), but not the WebRTC transport stack
  (`RTCPeerConnection`/ICE/STUN/TURN/SRTP). Transport is a single **WebSocket**
  on the port already served. See rationale below.
- **VAD in the browser.** The browser endpoints speech (as the Go energy-VAD does
  today, ported to an `AudioWorklet`) and sends **discrete utterance clips**, so
  a mic that stays hot for hours transmits audio proportional to how much you
  *talk*, not how long it is *on*. whisper stays utterance-based.
- **Reuse the engine boundaries.** Browser audio is an alternative implementation
  of the existing `Recorder` and playback (`player`) seams. The routing, VAD-fed
  whisper, session, and speak/queue logic are unchanged — only *where samples
  enter and leave* changes.

## Why not WebRTC

WebRTC earns its complexity for real-time interactive media (a live call): ultra
-low latency, loss concealment, and NAT traversal. This app has none of those
needs, and WebRTC would actively hurt here:

- **Turn-based PTT, not a call.** We send a complete utterance, whisper
  transcribes it, the agent replies with a discrete clip. Reliable, ordered,
  lossless delivery (TCP/WebSocket) is *better* for this than SRTP.
- **whisper is utterance-based.** We must segment before every transcription
  regardless, so the browser's VAD boundary is the natural transmit unit.
- **No NAT to punch through.** It's browser ↔ hub over an already-established
  channel; ICE/STUN/TURN buy nothing.
- **Tunnel compatibility (the decider).** WebRTC media is UDP with ICE-negotiated
  ports; a single-port `ssh -L` tunnel won't carry it (you'd need TURN or more
  tunneling). A WebSocket rides the exact port already forwarded.

"Streaming" and "WebRTC" are not the same thing: continuous audio can stream over
a WebSocket. With browser-side VAD we avoid continuous transmission entirely; even
if VAD were server-side, a WebSocket stream would still be simpler and
tunnel-friendly.

## Architecture

### Transport: one WebSocket

A single WebSocket (e.g. `GET /ws`), same origin, guarded like the other mutating
routes (known browser session + Host/Origin allowlist). It carries:

- **Text frames (JSON):** control — capture start/stop, format negotiation,
  playback begin/end, errors, keepalive.
- **Binary frames:** audio payloads — utterance PCM in (browser→hub), TTS/sound
  WAV out (hub→browser). A short binary header (or a preceding JSON envelope)
  identifies stream/format.

Device selection stays on the existing HTTP `/api/audio`; the WebSocket carries
only audio and capture control.

### Capture path (browser mic → hub)

1. Browser `getUserMedia({audio})` (allowed because the tunneled page is
   `http://localhost`, a secure context).
2. `AudioWorklet` resamples to **16 kHz mono float32** and runs a lightweight VAD
   (start with the ported energy VAD; Silero-VAD via ONNX-Runtime-Web is a later
   accuracy upgrade).
3. On a detected utterance, the worklet streams that utterance's frames over the
   WebSocket as they are captured (pipelined, so upload overlaps speaking), then
   sends an "utterance end" marker on VAD-end.
4. The hub's **BrowserRecorder** (implements `engine.Recorder`) assembles each
   utterance into a `Segment` and emits it on its channel — exactly what
   `MalgoRecorder` does. The `Service` loop transcribes (whisper on the hub host)
   and routes as today.
5. Capture is gated by the PTT dialog: `Service.StartDialog()`/`Stop()` send a
   control message telling the browser to start/stop the worklet.

### Playback path (hub → browser speaker)

1. `speak()`/`play_sound()` synthesize/generate mono PCM on the hub as now.
2. Instead of `AudioContext.Play`, a **BrowserPlayer** (implements the `player`
   seam used by `PiperTTS`/sounds) sends the PCM (as a WAV/PCM binary frame) over
   the WebSocket and waits for the browser to report playback complete — so the
   FIFO queue's "returns after playback" contract still holds.
3. The browser plays the clip via Web Audio (an `AudioBufferSourceNode`), on the
   work PC's speaker, respecting the UI volume/mute (applied browser-side, or the
   gain already applied server-side).

### Device selection & "which tab owns audio"

- The Microphone/Speaker dropdowns gain a **"Browser (this tab)"** option beside
  the enumerated local devices. Selecting it switches that direction's backend to
  the browser; a local device switches it back to malgo. The two directions are
  independent.
- The hub is one process but "the browser mic/speaker" lives in a specific tab —
  the tab whose WebSocket is the audio endpoint. First cut: the tab that selects a
  Browser device claims that direction (tied to its browser-session principal from
  pairing); last-selection-wins if multiple tabs, shown clearly in the UI.
- Runtime switch: like the hot-swappable STT/TTS, the `Service` swaps the active
  recorder/player when the selection changes.

## Integration with the engine

- `Recorder` seam: add `BrowserRecorder` next to `MalgoRecorder`. The `Service`
  holds the currently-selected recorder; `StartDialog` starts whichever is active.
- Playback seam: the `player` interface (`Play(pcm, sampleRate)`) already abstracts
  output; add `BrowserPlayer` next to `*AudioContext`. `PiperTTS` and the sound
  path take whichever is active.
- A small **audio router** owns the WebSocket per browser session and exposes
  `Recorder`/`player` implementations backed by it, plus the malgo ones, and
  swaps them based on the `/api/audio` selection.
- No change to `session` routing, the FIFO play queue, the VAD-to-whisper loop, or
  the MCP tool contract.

## Security

- The WebSocket is same-origin and passes through the **Host allowlist** and a
  known-**browser-session** check, like other mutating routes.
- Audio in flows only from a paired/authorized browser session's WebSocket; there
  is still no unauthenticated audio-injection path.
- Reuse the CSP/escaping posture from the pairing work (the UI is a principal).

## Secure context / tunnel notes

- `getUserMedia` requires a secure context, but **`http://localhost` / `127.0.0.1`
  qualify** — so a page reached over `ssh -L` (which the browser sees as
  `localhost`) can access the work PC's mic without HTTPS.
- The Host allowlist already accepts tunneled `localhost`, so it won't block this.

## Phasing

1. **Browser speaker (playback).** WebSocket + `BrowserPlayer` + "Browser" as a
   Speaker option + Web Audio playback. Smallest slice; you immediately *hear*
   replies on the remote machine.
2. **Browser mic (capture).** `getUserMedia` + `AudioWorklet` (16 kHz + energy
   VAD) + `BrowserRecorder` + "Browser" as a Microphone option. Pipelined
   utterance upload.
3. **Polish.** Runtime direction switching, multi-tab ownership UX, reconnect
   handling, volume/mute applied browser-side, latency tuning.

## Open questions / future

- **Silero VAD in the browser** (ONNX-Runtime-Web) for robustness in noisy rooms,
  replacing the energy VAD.
- **Echo cancellation / duplex.** With browser mic + browser speaker, enable
  `getUserMedia` echo cancellation so TTS isn't re-transcribed; or half-duplex
  (pause capture during playback).
- **Live partial transcripts** (captioning-style) would need a *streaming*
  recognizer, not whisper-cli — a separate feature. Even then, stream over the
  WebSocket, not WebRTC.
- **Format/bandwidth.** Raw 16 kHz mono PCM is ~256 kbit/s while speaking; fine
  over a tunnel, but Opus encoding in the browser is an option if needed.

## Verification

- Browser speaker: `speak()`/`play_sound()` play on the browser machine; the FIFO
  queue still serializes and `speak` returns after playback.
- Browser mic: spoken utterances arrive as `Segment`s, transcribe on the hub host,
  and route to the focused session; a hot mic during silence sends no audio.
- Switching a direction between a local device and Browser takes effect without
  restart; local-only operation is unchanged when no browser audio is selected.
- Works end to end over an `ssh -L` tunnel to `http://localhost:PORT`.
