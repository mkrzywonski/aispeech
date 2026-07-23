# Browser-bound pairing plan

## Goal

Authorize voice input only when the person at an AI TUI deliberately binds
that AI connection to one specific browser UI tab. A process that can merely
make HTTP requests to the local hub must not be able to view pairing secrets,
start listening, or inject voice commands into an AI session.

## Review decisions (2026-07-23)

Adopted from review; these supersede any conflicting detail below.

- **Host-header allowlist (new, required).** Reject any request whose `Host` is
  not an allowlisted `127.0.0.1:PORT` / `localhost:PORT` (plus the configured
  bind host). This is the primary defense against DNS-rebinding / malicious web
  pages — the realistic remote attacker against a localhost service. Combine
  with an `Origin` check on mutating routes.
- **WSL2 resolved.** The app runs in WSL; the browser runs on Windows and
  reaches the UI over the forwarded port. The hub may bind beyond pure loopback,
  so the Host/Origin allowlist must include the reachable host and carries the
  real weight (localhost cookies can't use `Secure`/`__Host-`).
- **Token format.** 128-bit, base32 (uppercase, terminal/paste-safe),
  single-use, 5-minute expiry, hashed at rest. It is copied (not manually
  relayed), so length is not a burden; keep the strong entropy.
- **Browser principal durability.** The `HttpOnly` cookie (browser profile) is
  the durable principal; the per-tab id is a refinement. A reload re-binds a
  fresh tab id to the existing cookie session rather than forcing a re-pair.
- **Agent binding durability (v1).** Re-pair per MCP connection: an agent
  restart is a new session needing a new token. A future optional "persistent
  agent credential" (issued on first pair, stored by the proxy in a `0600` file,
  re-presented on reconnect) can remove that friction within the threat model —
  deferred.
- **Drop pre-selected pending agent.** The agent that calls `pair(token)` is the
  binding target; there is no separate UI pre-selection.
- **UI is now a security principal.** Rigorously escape all agent- and
  STT-derived content (session names, transcripts) and add a CSP; an XSS would
  defeat the model.
- **`mcp-proxy` is on the critical path.** `pair(token)` flows through the stdio
  bridge; add a test that pairing works through it.

### Phasing

- **Phase 1 (closes the hole):** browser-session identity + browser-issued
  token, `pair(token)`, remove the pairing code from `/api/state`, Host/Origin
  allowlist, per-connection attempt limit + expiry, and gate mic-control
  endpoints (`ptt/start`, `ptt/stop`, constant-listen) on a valid browser
  session. Update tool guidance and the copy-token UI.
- **Phase 2 (defense in depth):** full per-tab↔agent binding, CSRF tokens,
  per-binding focus/rename/revoke gating, cross-session response filtering,
  replace/revoke UX, and the exhaustive verification matrix.

## Security boundary

The browser UI is a first-class principal, separate from an MCP agent
connection.

- A browser tab receives a unique, opaque UI-session identity.
- A pairing token is a separate capability for that UI session. It is
  cryptographically random, short-lived, single-use, and copied by an explicit
  user action in the UI.
- The user pastes the token into the AI TUI. The AI supplies it only to its own
  `pair(token)` call.
- On success, the hub binds exactly that browser UI session to exactly that MCP
  session.
- Only the bound browser session may control the active voice channel.

This protects against ordinary local processes that can connect to
`127.0.0.1`. It does not protect against a process that can read the user's
clipboard, browser profile, terminal, or keystrokes; such a process already
has the user's local authority.

## Session model

### Browser session

Create a browser principal on initial UI load.

- Issue an opaque browser cookie with `HttpOnly`, `SameSite=Strict`, and an
  appropriate localhost-safe cookie configuration.
- Create a distinct per-tab ID in `sessionStorage`; send it with requests in a
  dedicated header. Cookies alone are shared by tabs in one browser profile.
- The server records the browser-cookie identity and tab ID together as one UI
  session. A tab reload may create a new per-tab ID and must either resume
  safely or require a new pairing.
- Require CSRF protection for state-changing browser requests. Check a
  per-session CSRF token and same-origin `Origin` where available.

### Pairing token

Generate a separate token when the user clicks **Copy pairing token**.

- Generate at least 128 bits with `crypto/rand`; encode it as URL-safe base64
  (about 22 characters) or an equally strong, paste-friendly representation.
- Store only a keyed hash or cryptographic hash server-side.
- Bind it to one browser UI session, one expiry (suggested: 5 minutes), and
  optionally one selected pending agent session.
- Atomically consume it on the first successful pairing attempt.
- Never include it in URLs, shared UI state, notices, logs, MCP status, or
  error messages.

### Agent session

An MCP connection remains its own agent session. `pair(token)` must authorize
only the MCP session which called it; it must never accept an agent session ID
from tool input or authorize another connection.

After a successful pair, record the browser UI-session ID on the agent session.
Allow one active browser binding per agent connection and one active agent
binding per browser tab. Define an explicit UI action for replacing either
binding; do not silently steal an existing binding.

## Proposed flow

1. The user opens the local UI. The server establishes a browser-tab session.
2. The UI shows a **Pair an AI agent** panel and a **Copy pairing token**
   button. Copying creates or rotates a short-lived pairing token for that tab.
3. The agent connects through MCP and discovers that it is unpaired. Its tool
   guidance tells it to ask the user to paste the browser UI's pairing token
   into the AI TUI.
4. The user pastes the token into the TUI.
5. The agent calls `pair(token)`.
6. The hub validates and consumes the token, then atomically binds the current
   MCP session and the originating browser-tab session.
7. The UI displays the paired agent. The agent reports success.
8. Only that bound UI session can start/stop listening, select focus, or make
   other voice-channel control changes for the pair.

The copied token is intentionally a user-mediated secret. The agent must never
query the hub's UI/state endpoint to obtain it.

## API and UI changes

### Browser API

- Add an endpoint to establish or resume a UI session.
- Add an authenticated, CSRF-protected endpoint to create/rotate a pairing
  token for the current UI session.
- Add an authenticated endpoint to retrieve the current tab's non-secret
  pairing state (pending, paired agent name, expiry). Do not return a token
  after its initial copy response.
- Require the browser session on `ptt/start`, `ptt/stop`, constant-listen,
  focus, rename, revoke, and other state-changing UI endpoints.
- Filter state responses: a UI session may see its own pairing details; it
  should not receive another UI session's pairing data or secrets.

### MCP API

- Change `pair` input semantics from an agent-generated/displayed code to a
  browser-issued `token`.
- Update its description to explicitly require pasting a token supplied by the
  user in the AI TUI, never obtaining it from HTTP.
- Return generic pairing failures. Do not distinguish invalid, expired,
  consumed, or already-bound tokens.
- Keep `listen`, `speak`, and `status` unavailable until pairing succeeds.

### UI

- Replace the global pending-code list with browser-session-aware pairing
  status.
- Provide a deliberate Copy button, token expiry feedback, and a rotate-token
  action.
- Clearly show which agent is bound to this browser tab and provide an explicit
  revoke/unpair action.
- Clearly explain that the token must be pasted into the agent's terminal, not
  shared through another local process.

## Voice-input authorization

Pairing alone is insufficient if any localhost client can still call the PTT
or focus endpoints. Associate the currently active capture/dialog with the
browser-tab session that started it.

- Reject PTT and constant-listen requests from unpaired browser sessions.
- Ensure the active dialog belongs to the caller's bound session and agent.
- Stop capture and clear the active dialog when the browser binding is revoked,
  expires, or is replaced.
- Keep routing tied to the selected, paired agent session; do not route audio
  merely because an MCP session exists.

## Abuse and lifecycle controls

- Limit failed `pair` calls per MCP connection (for example, five attempts),
  then close or invalidate that connection's pending pairing state.
- Keep token validation constant-time where applicable and return a generic
  failure response.
- Expire pending agent sessions, pairing tokens, and abandoned browser-tab
  sessions.
- Cap pending sessions/tokens to bound memory and limit local connection-flood
  damage.
- Prefer per-connection limits over a tight global pairing limit, which could
  make legitimate pairing easy to deny.
- Log security events without logging token values: creation, expiry, failed
  validation count, pair success, revoke, and replacement.

## Implementation order

1. Add browser-tab session and pairing-token types, storage, expiry, and unit
   tests in `internal/session` (or a dedicated authorization package).
2. Add middleware that resolves browser identity and enforces CSRF/origin
   checks for mutating UI routes.
3. Add token-copy and per-browser pairing-status API endpoints.
4. Change MCP `pair` to validate and consume the browser-issued token, binding
   the current MCP session to its browser-tab session.
5. Gate PTT, focus, revocation, and other voice-control operations on the
   binding.
6. Update the web UI and agent-install/tool guidance.
7. Remove the existing globally exposed agent pairing code from state, notices,
   documentation, and tests.
8. Update README and DESIGN.md with the browser-bound authorization model and
   its threat-model limits.

## Verification

Unit and integration tests should cover:

- Different browser tabs receive distinct UI-session identities and tokens.
- A token pairs only its originating browser tab and the MCP session making the
  successful call.
- The same token cannot be reused, including under concurrent pair attempts.
- Expired, malformed, guessed, and consumed tokens yield the same generic
  result and do not pair an agent.
- A fake MCP connection cannot pair or affect an existing agent connection.
- An unbound browser session and a raw `curl` request cannot start PTT, switch
  focus, revoke another binding, or inject voice input.
- A second browser/tab cannot view another session's pairing secret or control
  its voice channel.
- Revoking a binding immediately stops active capture and prevents subsequent
  routing.
- Existing multi-agent session-word routing still works after authorized
  browser binding.

## Migration note

The current implementation has no browser-session identity and exposes
agent-generated pairing codes through shared state. Treat this as a security
model replacement, not a small alteration to the existing code field. Do not
preserve compatibility with the old globally visible pairing-code endpoint.
