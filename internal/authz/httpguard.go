package authz

import (
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// Allower reports whether a Host/Origin authority (host or host:port) is
// acceptable. It permits loopback names, any IP literal, this machine's
// hostname, and configured trusted hosts. That blocks DNS-rebinding (which
// arrives with an attacker *domain* in the Host header) while still allowing
// legitimate access via localhost, a LAN IP, an SSH-tunnel port, or the host's
// own name.
type Allower func(authority string) bool

// NewAllower builds an Allower for a hub bound at bindAddr, plus any extra
// trusted hostnames (from config).
func NewAllower(bindAddr string, trusted []string) Allower {
	hosts := map[string]bool{}
	add := func(h string) {
		h = strings.ToLower(strings.Trim(h, "[]"))
		if h != "" && h != "0.0.0.0" && h != "::" {
			hosts[h] = true
		}
	}
	if h, _, err := net.SplitHostPort(bindAddr); err == nil {
		add(h)
	}
	if hn, err := os.Hostname(); err == nil {
		add(hn)
	}
	for _, t := range trusted {
		if h, _, err := net.SplitHostPort(t); err == nil {
			add(h)
		} else {
			add(t)
		}
	}
	return func(authority string) bool {
		h, _, err := net.SplitHostPort(authority)
		if err != nil {
			h = authority
		}
		h = strings.ToLower(strings.Trim(h, "[]"))
		switch {
		case h == "":
			return false
		case h == "localhost" || h == "127.0.0.1" || h == "::1":
			return true
		case net.ParseIP(h) != nil: // any IP literal (LAN IP, etc.)
			return true
		default:
			return hosts[h] // machine hostname or a configured trusted host
		}
	}
}

// HostGuard rejects any request whose Host authority is not allowed — the
// primary defense against DNS rebinding for a localhost service.
func HostGuard(next http.Handler, allow Allower) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allow(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// OriginAllowed reports whether an Origin header value is acceptable. An empty
// Origin (non-browser clients such as curl or the MCP transport) passes; a
// browser cross-origin request carries an Origin that must satisfy the same
// allowlist.
func OriginAllowed(origin string, allow Allower) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return allow(u.Host)
}
