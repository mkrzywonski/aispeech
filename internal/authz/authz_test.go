package authz

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTokenLifecycle(t *testing.T) {
	s := NewStore(time.Minute)

	// A token cannot be issued without a known browser session.
	if _, ok := s.IssueToken("nope"); ok {
		t.Fatal("issued token for unknown browser")
	}

	cookie := s.NewBrowser()
	tok, ok := s.IssueToken(cookie)
	if !ok || tok == "" {
		t.Fatal("issue failed for known browser")
	}

	// First consume binds the originating browser.
	got, ok := s.ConsumeToken(tok)
	if !ok || got != cookie {
		t.Fatalf("consume = (%q,%v), want (%q,true)", got, ok, cookie)
	}
	// Second consume must fail (single-use).
	if _, ok := s.ConsumeToken(tok); ok {
		t.Fatal("token reused")
	}
	// Unknown/garbage token fails generically.
	if _, ok := s.ConsumeToken("GARBAGE"); ok {
		t.Fatal("garbage token accepted")
	}
}

func TestTokenExpiry(t *testing.T) {
	s := NewStore(time.Minute)
	now := time.Now()
	s.now = func() time.Time { return now }
	cookie := s.NewBrowser()
	tok, _ := s.IssueToken(cookie)

	now = now.Add(2 * time.Minute) // past expiry
	if _, ok := s.ConsumeToken(tok); ok {
		t.Fatal("expired token accepted")
	}
}

func TestTokensAreDistinct(t *testing.T) {
	s := NewStore(time.Minute)
	c1, c2 := s.NewBrowser(), s.NewBrowser()
	if c1 == c2 {
		t.Fatal("browser ids collided")
	}
	t1, _ := s.IssueToken(c1)
	t2, _ := s.IssueToken(c1)
	if t1 == t2 {
		t.Fatal("tokens collided")
	}
	// A token binds only its originating browser.
	got, _ := s.ConsumeToken(t1)
	if got != c1 {
		t.Fatalf("token bound to %q, want %q", got, c1)
	}
	_ = c2
}

func TestAllowedHostsAndGuard(t *testing.T) {
	allowed := AllowedHosts("127.0.0.1:7071")
	for _, h := range []string{"127.0.0.1:7071", "localhost:7071", "[::1]:7071"} {
		if !allowed[h] {
			t.Fatalf("host %q should be allowed", h)
		}
	}
	if allowed["evil.com:7071"] {
		t.Fatal("attacker host allowed")
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	g := HostGuard(next, allowed)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://localhost:7071/", nil)
	req.Host = "localhost:7071"
	g.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("allowed host rejected: %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "http://evil.com:7071/", nil)
	req.Host = "evil.com:7071" // DNS-rebinding attacker
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("attacker host not blocked: %d", rec.Code)
	}
}

func TestOriginAllowed(t *testing.T) {
	allowed := AllowedHosts("127.0.0.1:7071")
	if !OriginAllowed("", allowed) {
		t.Fatal("empty origin (curl/MCP) should pass")
	}
	if !OriginAllowed("http://localhost:7071", allowed) {
		t.Fatal("same-origin should pass")
	}
	if OriginAllowed("http://evil.com", allowed) {
		t.Fatal("cross-origin attacker should fail")
	}
}
