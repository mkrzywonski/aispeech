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

func TestHostAllowerAndGuard(t *testing.T) {
	allow := NewAllower("127.0.0.1:7071", []string{"mybox.local"})
	// Loopback (any port, e.g. an SSH tunnel), IP literals, and trusted hosts pass.
	for _, h := range []string{
		"127.0.0.1:7071", "localhost:7071", "localhost:7072", "[::1]:7072",
		"192.168.1.5:7071", "10.0.0.2:9999", "mybox.local:1234",
	} {
		if !allow(h) {
			t.Fatalf("host %q should be allowed", h)
		}
	}
	// Attacker domains (DNS rebinding) are blocked.
	for _, h := range []string{"evil.com:7071", "evil.com", "attacker.example:7072"} {
		if allow(h) {
			t.Fatalf("host %q should be blocked", h)
		}
	}

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	g := HostGuard(next, allow)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://localhost:7072/", nil)
	req.Host = "localhost:7072" // tunneled port
	g.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("tunneled localhost rejected: %d", rec.Code)
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
	allow := NewAllower("127.0.0.1:7071", nil)
	if !OriginAllowed("", allow) {
		t.Fatal("empty origin (curl/MCP) should pass")
	}
	if !OriginAllowed("http://localhost:7072", allow) {
		t.Fatal("same-origin (tunnel port) should pass")
	}
	if !OriginAllowed("http://192.168.1.5:7071", allow) {
		t.Fatal("LAN-IP origin should pass")
	}
	if OriginAllowed("http://evil.com", allow) {
		t.Fatal("cross-origin attacker should fail")
	}
}
