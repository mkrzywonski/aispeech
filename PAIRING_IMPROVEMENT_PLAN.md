# Pairing-flow improvement plan

## Objective

Make voice pairing self-explanatory to an MCP agent and low-friction to the
person using the AI TUI, while preserving the deliberate browser → human → TUI
confirmation that authorizes voice input.

This is a follow-up to `BROWSER_PAIRING_PLAN.md`, whose browser-issued,
single-use token remains the security foundation.

## Security invariant

Pairing must establish that the same person who controls the authorized
aispeech browser UI also deliberately supplied confirmation to the intended AI
TUI session. A browser-only **Approve this agent** action is not sufficient:
a different browser client could approve an arbitrary waiting session without
the TUI operator seeing or controlling that action.

The required flow remains:

```text
authorized browser UI → one-time pairing token → human paste into AI TUI
→ agent calls pair(token) → hub binds browser principal and MCP session
```

The token is a human-mediated, out-of-band confirmation. It is not a general
credential for an agent to retrieve or a user to share. The scope excludes
malware or another process with the same OS-user authority; see `DESIGN.md` §7.

## Problems to solve

1. **MCP discovery is weak.** An unpaired agent has no concise, structured
   explanation of why pairing exists or what it must ask the user to do.
2. **The runtime guidance is not verified.** The server sets initialization
   instructions, but the observed MCP `initialize` response did not contain an
   `instructions` field. Treat delivery as broken until an integration test
   proves the client receives it.
3. **Current messages are inconsistent.** The `pair` description refers to a
   browser token, while the shared unpaired error still says "8-character
   pairing code". The actual token is 128-bit base32 with a five-minute TTL.
4. **A hub that is down when Codex starts leaves no usable voice tools.** The
   stdio proxy currently connects once and exits if the hub is unavailable.
5. **Every hub restart requires a full token flow.** That is safe but annoying;
   any resumption design must not make a browser cookie alone sufficient.

## Required agent behavior

This contract must be available to an agent from the MCP connection itself. It
must not depend on an aispeech README, the current repository, or the agent
inferring intent from the MCP server's name.

When the user says an unambiguous phrase such as “let's talk by voice,” “start
a voice conversation,” or “talk about this by voice,” the agent must:

1. Recognize that this is a request to use the available voice-channel MCP
   capability, even in an unrelated project.
2. Call the safe discovery tool (`status` or a dedicated `pairing_status`) to
   create/inspect its voice session.
3. If unpaired, explain in one sentence that pairing is required to confirm
   that the person speaking also controls this AI TUI. Ask the user to copy a
   fresh pairing token from the authorized aispeech browser UI and paste it
   directly into this TUI.
4. Call `pair(token)` only with that user-supplied token. Never search for or
   retrieve the token through any other channel.
5. Once paired, begin with `converse` (or `listen` if no spoken greeting is
   appropriate). Use `converse` after each spoken request to keep the dialogue
   open until the user asks to stop or the channel is revoked/cancelled.

The MCP initialization instructions and tool descriptions should use this exact
decision sequence. A short version belongs in server instructions; `status`,
`pair`, and `converse` descriptions provide the detail at call time. If the MCP
transport cannot reliably deliver initialization instructions, introduce one
purpose-built discovery tool whose description and result carry this contract.

## Phase 1 — clarify and harden the existing flow

### MCP contract

- Verify that `initialize` exposes server instructions through the Go MCP SDK.
  If it does not, add an explicit read-only `pairing_status` tool (or enrich
  `status`) as the reliable discovery path.
- Put the **Required agent behavior** decision sequence in the initialization
  instructions. The first sentence must identify aispeech as the mechanism to
  use when a user asks for a voice conversation; it must not assume repository
  documentation is present.
- Make the unpaired status structured and actionable. Suggested fields:

  ```json
  {
    "paired": false,
    "pairing_required": true,
    "security_purpose": "Pairing confirms that the browser controller also controls this AI TUI session.",
    "next_action": "Ask the user to copy a pairing token from the aispeech UI and paste it directly into this TUI.",
    "token_source": "user_pasted_only"
  }
  ```

- Update `pair` and every unpaired error to say:
  - pairing is a browser-and-TUI co-control check;
  - accept only a token directly provided by the user in this conversation;
  - never obtain a token through HTTP, state endpoints, files, the clipboard,
    or another tool;
  - `listen`, `converse`, `speak`, and `play_sound` cannot operate until pair
    succeeds.
- Return a generic validation failure, but retain the actionable safe remedy:
  ask the user for a fresh UI-issued token. Do not reveal whether a candidate
  was expired, consumed, or valid for a different browser principal.
- Keep `status` usable while unpaired so it creates a pending session and gives
  the agent a safe first call.

### Browser UI

- Label the control **Copy pairing token for this AI TUI** and explain its
  purpose in one sentence: “Pasting this into the AI TUI confirms that you
  control both this UI and that session.”
- Show token lifetime and a clear retry state after expiry or failure; never
  display token values in shared state, logs, notices, or transcript history.
- Display an unambiguous paired state: agent name, pairing time, active focus,
  and a revoke action. Do not add a browser-only approval button.

### Availability and lifecycle

- Make `mcp-proxy` retry/reconnect to the hub with bounded backoff instead of
  exiting permanently when Codex starts before aispeech.
- On a successful reconnect, re-run MCP initialization and surface a clear
  unpaired state; do not silently grant voice access.
- Keep current per-connection pairing behavior for this phase. It is the
  security-preserving baseline and avoids prematurely persisting secrets.

## Phase 2 — bind the confirmation more tightly

Bind a newly issued token to both its browser principal and an opaque pending
MCP-session challenge.

1. `status` creates a pending pairing challenge for the calling MCP session and
   returns only a non-secret challenge label.
2. The browser UI lists pending session labels and issues a token only for a
   deliberately selected label.
3. `pair(token)` consumes the token only when the caller is that exact pending
   MCP session.

This prevents a token intentionally copied for one waiting TUI from pairing a
different session. It does **not** replace the copy/paste step: browser-side
selection is a usability aid and binding constraint, not the proof of TUI
control.

## Phase 3 — optional trusted-session resumption

Do not resume from a browser cookie alone. If restart friction remains worth
solving, use two continuations of the original trust relationship:

- the existing browser principal cookie; and
- a stable private key owned by `mcp-proxy`, with only its public key persisted
  by the hub.

On first manual pairing, register the proxy public key for that browser
principal. On a later hub/proxy restart, the proxy signs a fresh hub challenge;
the hub resumes only when it can match the established browser principal and
verify the signature. Store the proxy private key outside model context, use
restrictive local permissions (or an OS keychain where available), rotate or
expire registrations, and provide an obvious browser revoke-all action.

Before implementation, decide the liveness policy:

- **Convenience-first:** valid browser principal + proxy proof resumes
  automatically after a hub restart.
- **Presence-first:** the browser must also be open and make a fresh
  same-origin confirmation after a restart; this is one click or an automatic
  page-side confirmation, but never a TUI-only resume.

Either policy remains outside the same-user-malware threat boundary. Full manual
pairing remains the fallback whenever the browser profile, proxy key, or trust
record changes.

## Implementation map

| Area | Phase 1 work | Later work |
| --- | --- | --- |
| `internal/mcpserver` | Instructions delivery test; truthful tool/error text; structured unpaired status | Pending challenge and session-bound token validation |
| `internal/authz` | Preserve one-time hashed token semantics | Challenge records; browser+proxy durable registrations |
| `internal/session` | Explicit pending/unpaired view state | Challenge ownership and resumption identity |
| `internal/web` | Clear copy/retry/paired UI states | Select pending session; resume/revoke controls |
| `cmd/aispeech/proxy.go` | Reconnect/backoff behavior | Proxy key generation, challenge signing, key storage |
| Docs/config | Align README, DESIGN, and install guidance; approve `converse` for smooth dialogs | Document retention, expiry, and revocation policy |

## Verification and acceptance criteria

### Security

- A raw localhost HTTP client cannot obtain a token, pair, start capture, or
  inject text.
- Cross-origin and invalid-Host requests cannot drive mutating UI routes.
- A token is single-use, short-lived, stored only hashed, and never appears in
  logs, `/api/state`, MCP status, or errors.
- A token for pending session A cannot pair session B after Phase 2.
- A browser-only action cannot pair a session without the token reaching that
  session's TUI and being presented through `pair`.

### Agent experience

- In a fresh Codex session opened in an unrelated, empty project, the prompt
  “let's talk about it by voice” leads the agent to the voice MCP discovery
  tool without searching the project for documentation.
- A newly connected agent can call `status` and receives the security purpose,
  exact next step, and safe token source without reading repository docs.
- Initialization instructions, if supported by the transport, are asserted in
  an end-to-end proxy test.
- After pairing, the agent can use `converse` continuously without a further
  pairing prompt; a revoked session gets a clear unpaired response.

### Reliability

- Start Codex/proxy before the hub, then start the hub: tools become available
  after bounded retry without restarting Codex.
- Hub restart while Codex is open yields an explicit unpaired baseline in Phase
  1, and only the selected resumption policy in Phase 3.

## Rollout order

1. Correct the contradictory MCP text and add MCP/proxy integration coverage.
2. Implement structured pairing status and browser copy/retry wording.
3. Implement proxy reconnection and test startup ordering.
4. Re-test the full human path with a fresh Codex session.
5. Decide whether Phase 2 session binding is necessary before adding it.
6. Decide the Phase 3 persistence/liveness policy only after observing the
   Phase 1 flow in normal use.
