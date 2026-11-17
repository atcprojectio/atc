package atc

import (
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// isLoopbackHost reports whether host (a hostname or IP, without port) refers to the local machine.
func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// IsLoopbackListenAddress reports whether a listen address only accepts local connections.
// An empty address means "all interfaces" and is therefore not loopback.
func IsLoopbackListenAddress(addr string) bool {
	if addr == "" {
		return false
	}
	return isLoopbackHost(addr)
}

func hostWithoutPort(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

// originAllowed decides whether a browser Origin may call this server. Requests without an
// Origin header (curl, CLI tools, native MCP clients) are not browser cross-origin requests
// and are always allowed here; authentication still applies to them.
func originAllowed(origin, requestHost string, allowed []string) bool {
	if origin == "" {
		return true
	}
	if slices.Contains(allowed, origin) {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	// Same-origin (e.g. the embedded dashboard calling its own API).
	if strings.EqualFold(u.Host, requestHost) {
		return true
	}
	// A local tool (e.g. MCP inspector on localhost) talking to a locally bound ATC.
	return isLoopbackHost(u.Hostname()) && isLoopbackHost(hostWithoutPort(requestHost))
}

// requestGuard rejects requests that browsers could be tricked into sending:
//   - cross-origin requests from origins that are not allowed (CSRF / malicious web pages);
//   - when running unauthenticated on loopback, requests whose Host header is not a loopback
//     name, which is what a DNS-rebinding attack looks like.
//
// When withCORS is set it also answers CORS preflights and sets CORS headers for allowed
// origins (used for the MCP listener, which browser-based MCP clients call cross-origin).
func (t *Atc) requestGuard(withCORS bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.cfgMu.RLock()
			allowedOrigins := t.Cfg.Server.AllowedOrigins
			authEnabled := t.Cfg.Auth.Enabled
			allowUnauth := t.Cfg.Auth.AllowUnauthenticated
			t.cfgMu.RUnlock()

			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("X-Frame-Options", "DENY")

			// Health probes carry no sensitive data and may come from orchestrators using a pod IP.
			if r.URL.Path == "/health" || r.URL.Path == "/ready" {
				next.ServeHTTP(w, r)
				return
			}

			if !authEnabled && !allowUnauth && !isLoopbackHost(hostWithoutPort(r.Host)) {
				writeJSONError(w, http.StatusForbidden, "forbidden: unexpected Host header for an unauthenticated local-only server")
				return
			}

			origin := r.Header.Get("Origin")
			if !originAllowed(origin, r.Host, allowedOrigins) {
				writeJSONError(w, http.StatusForbidden, "forbidden: origin not allowed")
				return
			}

			if withCORS && origin != "" {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Mcp-Session-Id, Mcp-Protocol-Version, Last-Event-ID, Authorization, X-Consul-Token, X-ATC-Token")
				w.Header().Set("Access-Control-Expose-Headers", "Mcp-Session-Id")
				if r.Method == http.MethodOptions {
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}

			next.ServeHTTP(w, r)
		})
	}
}
