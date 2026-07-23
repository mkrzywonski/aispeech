// Package session tracks connected AI agents, the pairing handshake, input
// focus, and delivery of transcribed utterances to a listening session.
//
// Identity is the per-connection MCP session id supplied by the transport; a
// separate bearer token is unnecessary because the Streamable HTTP transport
// already isolates and authenticates each connection. Pairing simply flips a
// connection from "pending" to "paired"; an unpaired connection can do nothing
// but pair.
package session

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Utterance is a transcribed command routed to a target session.
type Utterance struct {
	Text   string
	Target string // resolved session name
}

// Session is one connected agent.
type Session struct {
	ID          string    // MCP connection id (identity)
	ClientName string    // from MCP clientInfo, e.g. "claude"
	Name       string    // display name / session-word (user-renamable)
	Paired     bool      // completed the pairing handshake
	Browser    string    // bound browser-session cookie (set on pair)
	Voice      string    // assigned TTS voice model path ("" = default)
	Connected  time.Time // first seen

	pairFails int // failed pair attempts (rate limiting)

	// listen holds the channel of an outstanding listen() call, or nil when the
	// session is not currently listening. Guarded by Registry.mu.
	listen chan Utterance
}

// Notice is a transient event surfaced in the UI (e.g. "claude isn't listening").
type Notice struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"` // info | warn
	Text string    `json:"text"`
}

// Transcript is a recognized utterance and what routing did with it.
type Transcript struct {
	At      time.Time `json:"at"`
	Text    string    `json:"text"`             // recognized speech
	Target  string    `json:"target,omitempty"` // session it was routed to
	Outcome string    `json:"outcome"`          // delivered | dropped | focus | no-session
}

// Registry is the concurrency-safe source of truth for sessions and focus.
type Registry struct {
	mu          sync.Mutex
	byID        map[string]*Session
	focusID     string
	voiceSeq    int          // rotates voice assignment when all are in use
	notices     []Notice     // ring buffer, newest last
	transcripts []Transcript // ring buffer, newest last
}

// New returns an empty Registry.
func New() *Registry {
	return &Registry{byID: make(map[string]*Session)}
}

// Attach returns the session for id, creating a pending one on first contact.
// clientName seeds the default display name. No secret is generated here: a
// session is authorized only when an agent presents a browser-issued token to
// Pair.
func (r *Registry) Attach(id, clientName string) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.byID[id]; ok {
		return s
	}
	if clientName == "" {
		clientName = "agent"
	}
	s := &Session{
		ID:         id,
		ClientName: clientName,
		Name:       r.uniqueNameLocked(clientName),
		Connected:  time.Now(),
	}
	r.byID[id] = s
	r.noticeLocked("info", fmt.Sprintf("%s connected — waiting to pair", s.Name))
	return s
}

// Pair marks a session authorized and binds it to the browser session cookie
// that issued the consumed pairing token. The caller is responsible for
// validating/consuming the token first.
func (r *Registry) Pair(id, cookie string) (*Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.byID[id]
	if s == nil {
		return nil, fmt.Errorf("unknown session")
	}
	if s.Paired {
		return s, nil
	}
	s.Paired = true
	s.Browser = cookie
	if r.focusID == "" {
		r.focusID = s.ID // first paired session takes focus
	}
	r.noticeLocked("info", fmt.Sprintf("%s paired", s.Name))
	return s, nil
}

// PairAttemptsExceeded reports whether a session has hit the failed-attempt
// limit and further pairing should be refused.
func (r *Registry) PairAttemptsExceeded(id string, limit int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.byID[id]
	return s != nil && s.pairFails >= limit
}

// NotePairFailure records a failed pair attempt and returns the running count.
func (r *Registry) NotePairFailure(id string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.byID[id]
	if s == nil {
		return 0
	}
	s.pairFails++
	r.noticeLocked("warn", fmt.Sprintf("%s: invalid pairing token", s.Name))
	return s.pairFails
}

// Detach removes a session (on disconnect or revoke).
func (r *Registry) Detach(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.byID[id]
	if s == nil {
		return
	}
	delete(r.byID, id)
	if r.focusID == id {
		r.focusID = ""
	}
	r.noticeLocked("info", fmt.Sprintf("%s disconnected", s.Name))
}

// Listen registers an outstanding listen() for a paired session and blocks
// until an utterance is routed to it, the timeout elapses, or ctx is cancelled.
// status is one of: "ok", "timeout", "cancelled", "unpaired".
func (r *Registry) Listen(ctx context.Context, id string, timeout time.Duration) (u Utterance, status string) {
	r.mu.Lock()
	s := r.byID[id]
	if s == nil || !s.Paired {
		r.mu.Unlock()
		return Utterance{}, "unpaired"
	}
	ch := make(chan Utterance, 1)
	s.listen = ch
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		if s.listen == ch {
			s.listen = nil
		}
		r.mu.Unlock()
	}()

	select {
	case u := <-ch:
		return u, "ok"
	case <-time.After(timeout):
		return Utterance{}, "timeout"
	case <-ctx.Done():
		return Utterance{}, "cancelled"
	}
}

// Deliver routes an already-transcribed utterance. It resolves a leading
// session-word (switching focus), then delivers the remainder to the focused
// session — or drops it with a UI notice if there is no focus or the target is
// not currently listening (no buffering, by design).
func (r *Registry) Deliver(text string) {
	heard := strings.TrimSpace(text)
	if heard == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	command := heard

	// Session-word matching on the leading token.
	if id, rest, ok := r.matchLeadingNameLocked(heard); ok {
		r.focusID = id
		s := r.byID[id]
		r.noticeLocked("info", fmt.Sprintf("focus → %s", s.Name))
		if strings.TrimSpace(rest) == "" {
			r.transcriptLocked(heard, s.Name, "focus") // focus switch only
			return
		}
		command = strings.TrimSpace(rest)
	}

	focus := r.byID[r.focusID]
	if focus == nil {
		r.noticeLocked("warn", "no session selected")
		r.transcriptLocked(heard, "", "no-session")
		return
	}
	if focus.listen == nil {
		r.noticeLocked("warn", fmt.Sprintf("%s isn't listening", focus.Name))
		r.transcriptLocked(heard, focus.Name, "dropped")
		return
	}
	focus.listen <- Utterance{Text: command, Target: focus.Name}
	focus.listen = nil
	r.noticeLocked("info", fmt.Sprintf("%s ◀ %q", focus.Name, truncate(command, 60)))
	r.transcriptLocked(heard, focus.Name, "delivered")
}

// SetFocus selects the focused session from the UI.
func (r *Registry) SetFocus(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.byID[id]
	if s == nil || !s.Paired {
		return fmt.Errorf("no such paired session")
	}
	r.focusID = id
	r.noticeLocked("info", fmt.Sprintf("focus → %s", s.Name))
	return nil
}

// Voice returns the TTS voice assigned to a session ("" = default).
func (r *Registry) Voice(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.byID[id]; s != nil {
		return s.Voice
	}
	return ""
}

// SetVoice assigns a specific voice to a session (from the UI).
func (r *Registry) SetVoice(id, voice string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.byID[id]; s != nil {
		s.Voice = voice
	}
}

// AssignVoice gives a session a distinct voice from available, preferring one no
// other session is using, so agents sound different. No-op once assigned or when
// no voices are installed.
func (r *Registry) AssignVoice(id string, available []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.byID[id]
	if s == nil || s.Voice != "" || len(available) == 0 {
		return
	}
	used := make(map[string]bool)
	for _, o := range r.byID {
		if o.ID != id && o.Voice != "" {
			used[o.Voice] = true
		}
	}
	for _, v := range available {
		if !used[v] {
			s.Voice = v
			return
		}
	}
	s.Voice = available[r.voiceSeq%len(available)] // all in use: rotate
	r.voiceSeq++
}

// Rename changes a session's display name / session-word.
func (r *Registry) Rename(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.byID[id]
	if s == nil {
		return fmt.Errorf("no such session")
	}
	s.Name = name
	return nil
}

// --- snapshot for the UI ---

// SessionView is a read-only projection of a Session for the UI/API.
type SessionView struct {
	ID         string `json:"id"`
	ClientName string `json:"client_name"`
	Name       string `json:"name"`
	Paired     bool   `json:"paired"`
	Listening  bool   `json:"listening"`
	Focused    bool   `json:"focused"`
	Voice      string `json:"voice"`
}

// Get returns a locked snapshot of one session.
func (r *Registry) Get(id string) (SessionView, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.byID[id]
	if s == nil {
		return SessionView{}, false
	}
	return SessionView{
		ID:         s.ID,
		ClientName: s.ClientName,
		Name:       s.Name,
		Paired:     s.Paired,
		Listening:  s.listen != nil,
		Focused:    s.ID == r.focusID,
		Voice:      s.Voice,
	}, true
}

// Snapshot returns the current sessions, recent notices, and recent transcripts
// for the UI.
func (r *Registry) Snapshot() ([]SessionView, []Notice, []Transcript) {
	r.mu.Lock()
	defer r.mu.Unlock()
	views := make([]SessionView, 0, len(r.byID))
	for _, s := range r.byID {
		views = append(views, SessionView{
			ID:         s.ID,
			ClientName: s.ClientName,
			Name:       s.Name,
			Paired:     s.Paired,
			Listening:  s.listen != nil,
			Focused:    s.ID == r.focusID,
			Voice:      s.Voice,
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	notices := append([]Notice(nil), r.notices...)
	transcripts := append([]Transcript(nil), r.transcripts...)
	return views, notices, transcripts
}

// --- internal helpers (call with r.mu held) ---

func (r *Registry) matchLeadingNameLocked(text string) (id, rest string, ok bool) {
	// Longest-name-first so multi-word names win over prefixes.
	type nm struct{ id, name string }
	var names []nm
	for _, s := range r.byID {
		if s.Paired {
			names = append(names, nm{s.ID, s.Name})
		}
	}
	sort.Slice(names, func(i, j int) bool { return len(names[i].name) > len(names[j].name) })
	lower := strings.ToLower(text)
	for _, n := range names {
		key := strings.ToLower(n.name)
		if lower == key {
			return n.id, "", true
		}
		// Match "<name> <rest>" or "<name>, <rest>".
		if strings.HasPrefix(lower, key) {
			after := text[len(n.name):]
			trimmed := strings.TrimLeft(after, " ,.:—-")
			if len(after) != len(trimmed) || after == "" {
				return n.id, trimmed, true
			}
		}
	}
	return "", "", false
}

func (r *Registry) uniqueNameLocked(base string) string {
	taken := map[string]bool{}
	for _, s := range r.byID {
		taken[strings.ToLower(s.Name)] = true
	}
	if !taken[strings.ToLower(base)] {
		return base
	}
	for i := 2; ; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		if !taken[strings.ToLower(cand)] {
			return cand
		}
	}
}

func (r *Registry) noticeLocked(kind, text string) {
	r.notices = append(r.notices, Notice{At: time.Now(), Kind: kind, Text: text})
	const maxNotices = 50
	if len(r.notices) > maxNotices {
		r.notices = r.notices[len(r.notices)-maxNotices:]
	}
}

func (r *Registry) transcriptLocked(text, target, outcome string) {
	r.transcripts = append(r.transcripts, Transcript{At: time.Now(), Text: text, Target: target, Outcome: outcome})
	const maxTranscripts = 100
	if len(r.transcripts) > maxTranscripts {
		r.transcripts = r.transcripts[len(r.transcripts)-maxTranscripts:]
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
