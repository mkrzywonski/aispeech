// Package authz provides the browser-bound authorization primitives for
// pairing: a browser-session principal (distinct from an MCP agent), single-use
// pairing tokens issued only to a known browser session, and a Host/Origin
// allowlist that defends the localhost hub against DNS-rebinding and malicious
// web pages.
//
// A pairing token is a capability. It is created only when a real browser
// session asks for one, shown once, stored server-side as a hash, expires
// quickly, and is consumed atomically on first use. A process that can only make
// HTTP requests (no browser session, no user clipboard/keystrokes) cannot obtain
// one, so it cannot pair, listen, or inject voice.
package authz

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/hex"
	"sync"
	"time"
)

// DefaultTokenTTL is how long a pairing token remains valid.
const DefaultTokenTTL = 5 * time.Minute

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// Store holds browser sessions and pairing tokens. Safe for concurrent use.
type Store struct {
	mu       sync.Mutex
	browsers map[string]time.Time  // cookie id -> last seen
	tokens   map[string]*tokenRec  // token hash (hex) -> record
	ttl      time.Duration
	now      func() time.Time
}

type tokenRec struct {
	cookie string
	expiry time.Time
}

// NewStore returns a Store. ttl <= 0 uses DefaultTokenTTL.
func NewStore(ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = DefaultTokenTTL
	}
	return &Store{
		browsers: make(map[string]time.Time),
		tokens:   make(map[string]*tokenRec),
		ttl:      ttl,
		now:      time.Now,
	}
}

// NewBrowser mints a new browser-session cookie id and records it.
func (s *Store) NewBrowser() string {
	id := randToken(16)
	s.mu.Lock()
	s.browsers[id] = s.now()
	s.mu.Unlock()
	return id
}

// KnownBrowser reports whether cookie identifies a live browser session,
// refreshing its last-seen time.
func (s *Store) KnownBrowser(cookie string) bool {
	if cookie == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.browsers[cookie]; !ok {
		return false
	}
	s.browsers[cookie] = s.now()
	return true
}

// IssueToken creates a single-use pairing token bound to a known browser
// session. The plaintext token is returned once; only its hash is stored.
func (s *Store) IssueToken(cookie string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.browsers[cookie]; !ok {
		return "", false
	}
	s.gcLocked()
	tok := randToken(16) // 128 bits
	s.tokens[hashToken(tok)] = &tokenRec{cookie: cookie, expiry: s.now().Add(s.ttl)}
	return tok, true
}

// ConsumeToken atomically validates and consumes a token, returning the browser
// cookie it was bound to. Invalid, expired, and already-consumed tokens all
// return ("", false) — no distinguishing oracle.
func (s *Store) ConsumeToken(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	h := hashToken(tok)
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.tokens[h]
	if !ok {
		return "", false
	}
	delete(s.tokens, h) // single-use: consume regardless of outcome
	if s.now().After(rec.expiry) {
		return "", false
	}
	if _, live := s.browsers[rec.cookie]; !live {
		return "", false
	}
	return rec.cookie, true
}

// gcLocked drops expired tokens. Caller holds s.mu.
func (s *Store) gcLocked() {
	now := s.now()
	for h, rec := range s.tokens {
		if now.After(rec.expiry) {
			delete(s.tokens, h)
		}
	}
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("authz: crypto/rand failed: " + err.Error())
	}
	return b32.EncodeToString(b)
}

// ConstantTimeEqual compares two secrets without leaking length-independent
// timing. Exposed for callers that compare non-hashed secrets.
func ConstantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
