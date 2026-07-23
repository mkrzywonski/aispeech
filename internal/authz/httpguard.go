package authz

import (
	"net"
	"net/http"
	"net/url"
)

// AllowedHosts returns the set of acceptable Host/Origin authorities for a hub
// bound at bindAddr. It always permits the loopback names on the bound port;
// when bindAddr names a specific host (e.g. a WSL-reachable IP) that authority
// is permitted too. A DNS-rebinding attacker's page sends its own hostname in
// the Host header, which will not be in this set.
func AllowedHosts(bindAddr string) map[string]bool {
	host, port, err := net.SplitHostPort(bindAddr)
	if err != nil {
		port = bindAddr // best effort
	}
	set := map[string]bool{}
	add := func(h string) {
		if h != "" {
			set[net.JoinHostPort(h, port)] = true
		}
	}
	add("127.0.0.1")
	add("localhost")
	add("::1")
	if host != "" && host != "0.0.0.0" && host != "::" {
		add(host)
	}
	return set
}

// HostGuard rejects any request whose Host authority is not allowed. This is the
// primary defense against DNS rebinding for a localhost service.
func HostGuard(next http.Handler, allowed map[string]bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Host] {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// OriginAllowed reports whether an Origin header value is acceptable. An empty
// Origin (non-browser clients such as curl or the MCP transport) is allowed
// here; browser cross-origin requests carry an Origin that must match the
// allowlist. Mutating UI routes combine this with a required browser session.
func OriginAllowed(origin string, allowed map[string]bool) bool {
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return allowed[u.Host]
}
