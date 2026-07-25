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
	"unicode"
	"unicode/utf8"
)

// Utterance is a transcribed command routed to a target session.
type Utterance struct {
	Text   string
	Target string // resolved session name
}

// Session is one connected agent.
type Session struct {
	ID         string    // MCP connection id (identity)
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

// LogEntry is one line in the unified activity log. Kind determines which
// optional fields are meaningful:
//
//	recognized — a transcribed utterance; Session is the routed target,
//	             Outcome is one of delivered | dropped | focus | no-session.
//	speech     — a spoken (TTS) reply; Session is the agent that spoke.
//	notice     — a session/pairing event; Level is info | warn.
type LogEntry struct {
	At      time.Time `json:"at"`
	Kind    string    `json:"kind"` // recognized | speech | notice
	Text    string    `json:"text"`
	Session string    `json:"session,omitempty"`
	Outcome string    `json:"outcome,omitempty"`
	Level   string    `json:"level,omitempty"`
}

// Registry is the concurrency-safe source of truth for sessions and focus.
type Registry struct {
	mu       sync.Mutex
	byID     map[string]*Session
	focusID  string
	voiceSeq int        // rotates voice assignment when all are in use
	log      []LogEntry // unified activity log ring buffer, newest last
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
	// Default the display name (and thus the spoken wake word) to a single word
	// derived from the client id, e.g. "claude-code" -> "claude". A one-word name
	// is far easier to say and to match against transcribed speech.
	base := firstWord(clientName)
	if base == "" {
		base = "agent"
	}
	s := &Session{
		ID:         id,
		ClientName: clientName,
		Name:       r.uniqueNameLocked(base),
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

// UnpairAll voids every session's pairing. It is called when the UI operator
// slot changes hands so a new browser cannot inherit agents the previous
// operator paired: sessions stay connected but must pair again (with the new
// operator's token) before voice works. Focus is cleared and any pending
// listen() stops receiving routed speech.
func (r *Registry) UnpairAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.byID {
		if !s.Paired {
			continue
		}
		s.Paired = false
		s.Browser = ""
		s.listen = nil
		r.noticeLocked("info", fmt.Sprintf("%s unpaired — a new browser took over the hub", s.Name))
	}
	r.focusID = ""
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
// session — or drops it if there is no focus or the target is not currently
// listening (no buffering, by design). Each utterance and its outcome are
// recorded as a transcript for the UI; routing is not duplicated into the
// activity log. It reports whether a command was actually delivered to a
// listening, focused session. Focus-only utterances and dropped transcripts do
// not count as delivery.
func (r *Registry) Deliver(text string) bool {
	heard := strings.TrimSpace(text)
	if heard == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	command := heard

	// Session-word matching on the leading token.
	if id, rest, ok := r.matchLeadingNameLocked(heard); ok {
		r.focusID = id
		s := r.byID[id]
		if strings.TrimSpace(rest) == "" {
			r.transcriptLocked(heard, s.Name, "focus") // focus switch only
			return false
		}
		command = strings.TrimSpace(rest)
	}

	focus := r.byID[r.focusID]
	if focus == nil {
		r.transcriptLocked(heard, "", "no-session")
		return false
	}
	if focus.listen == nil {
		r.transcriptLocked(heard, focus.Name, "dropped")
		return false
	}
	focus.listen <- Utterance{Text: command, Target: focus.Name}
	focus.listen = nil
	r.transcriptLocked(heard, focus.Name, "delivered")
	return true
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
	if s == nil || s.Voice != "" {
		return
	}
	s.Voice = r.distinctVoiceLocked(id, available)
}

// distinctVoiceLocked returns a voice from available not used by any session
// other than id, falling back to a rotating pick when all are in use. Returns
// "" (the default) when nothing is installed. Caller holds r.mu.
func (r *Registry) distinctVoiceLocked(id string, available []string) string {
	if len(available) == 0 {
		return ""
	}
	used := make(map[string]bool)
	for _, o := range r.byID {
		if o.ID != id && o.Voice != "" {
			used[o.Voice] = true
		}
	}
	for _, v := range available {
		if !used[v] {
			return v
		}
	}
	v := available[r.voiceSeq%len(available)] // all in use: rotate
	r.voiceSeq++
	return v
}

// DropVoice clears the given voice from every session using it (e.g. after that
// voice was deleted) and reassigns each a distinct remaining voice, falling back
// to the default ("") when none remain. available must be the still-installed
// voices (not including the deleted one). No-op for the empty voice.
func (r *Registry) DropVoice(voice string, available []string) {
	if voice == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.byID {
		if s.Voice == voice {
			s.Voice = ""
			s.Voice = r.distinctVoiceLocked(s.ID, available)
		}
	}
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

// Snapshot returns the current sessions and the recent unified activity log
// (recognized speech, spoken replies, and notices in chronological order).
func (r *Registry) Snapshot() ([]SessionView, []LogEntry) {
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
	log := append([]LogEntry(nil), r.log...)
	return views, log
}

// Name returns a session's display name, or "" if unknown.
func (r *Registry) Name(id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.byID[id]; s != nil {
		return s.Name
	}
	return ""
}

// RecordSpeech logs a spoken (TTS) reply in the activity log, attributed to the
// speaking agent by name.
func (r *Registry) RecordSpeech(name, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logLocked(LogEntry{Kind: "speech", Session: name, Text: text})
}

// --- internal helpers (call with r.mu held) ---

// matchLeadingNameLocked matches a session's name against the leading words of
// text, treating hyphens, underscores, punctuation and whitespace all as word
// separators. This is what makes speech routing robust: a name like
// "claude-code" matches spoken "Claude code, ..." even though whisper writes no
// hyphen. The command remainder keeps its original casing and punctuation.
func (r *Registry) matchLeadingNameLocked(text string) (id, rest string, ok bool) {
	type nm struct {
		id     string
		tokens []string
	}
	var names []nm
	for _, s := range r.byID {
		if s.Paired {
			if t := wordTokens(s.Name); len(t) > 0 {
				names = append(names, nm{s.ID, t})
			}
		}
	}
	// Longest-name-first (by token count) so a multi-word name wins over a
	// shorter name that is a prefix of it.
	sort.Slice(names, func(i, j int) bool { return len(names[i].tokens) > len(names[j].tokens) })
	for _, n := range names {
		if remainder, matched := stripLeadingTokens(text, n.tokens); matched {
			return n.id, remainder, true
		}
	}
	return "", "", false
}

// isWordRune reports whether r is part of a word (letter or digit); everything
// else (spaces, hyphens, punctuation) separates words.
func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }

// wordTokens splits s into lowercase word tokens on any non-word runes, so
// "claude-code" and "claude code" both yield ["claude", "code"].
func wordTokens(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !isWordRune(r) })
}

// firstWord returns the first word token of s in its original casing, or "".
func firstWord(s string) string {
	start, end := -1, -1
	for i, r := range s {
		if isWordRune(r) {
			if start < 0 {
				start = i
			}
			end = i + utf8.RuneLen(r)
		} else if start >= 0 {
			break
		}
	}
	if start < 0 {
		return ""
	}
	return s[start:end]
}

// stripLeadingTokens reports whether the leading word tokens of text equal want
// (case-insensitively), and if so returns the remainder of text after those
// tokens with leading separators trimmed. Word boundaries ignore separators, so
// want ["claude","code"] matches "Claude-code, run it" -> rest "run it".
func stripLeadingTokens(text string, want []string) (string, bool) {
	pos := 0
	for _, w := range want {
		// Skip separators before the next token.
		for pos < len(text) {
			r, size := utf8.DecodeRuneInString(text[pos:])
			if isWordRune(r) {
				break
			}
			pos += size
		}
		start := pos
		for pos < len(text) {
			r, size := utf8.DecodeRuneInString(text[pos:])
			if !isWordRune(r) {
				break
			}
			pos += size
		}
		if start == pos || !strings.EqualFold(text[start:pos], w) {
			return "", false
		}
	}
	rest := strings.TrimLeftFunc(text[pos:], func(r rune) bool { return !isWordRune(r) })
	return rest, true
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

const maxLog = 200

func (r *Registry) logLocked(e LogEntry) {
	e.At = time.Now()
	r.log = append(r.log, e)
	if len(r.log) > maxLog {
		r.log = r.log[len(r.log)-maxLog:]
	}
}

func (r *Registry) noticeLocked(level, text string) {
	r.logLocked(LogEntry{Kind: "notice", Level: level, Text: text})
}

func (r *Registry) transcriptLocked(text, target, outcome string) {
	r.logLocked(LogEntry{Kind: "recognized", Text: text, Session: target, Outcome: outcome})
}
