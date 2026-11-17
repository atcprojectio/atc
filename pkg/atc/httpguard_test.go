package atc

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	mcp_server "github.com/atcprojectio/atc/pkg/atc/mcp"
)

func TestOriginAllowed(t *testing.T) {
	allowed := []string{"https://ops.example.com"}
	tests := []struct {
		name, origin, host string
		want               bool
	}{
		{"no origin (non-browser client)", "", "atc.internal:8088", true},
		{"same origin dashboard", "http://atc.internal:8088", "atc.internal:8088", true},
		{"allow-listed origin", "https://ops.example.com", "atc.internal:8088", true},
		{"foreign origin", "https://evil.example", "atc.internal:8088", false},
		{"localhost tool to loopback server", "http://localhost:6274", "127.0.0.1:8092", true},
		{"localhost origin to remote server", "http://localhost:6274", "atc.internal:8092", false},
		{"null origin", "null", "atc.internal:8088", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, originAllowed(tt.origin, tt.host, allowed))
		})
	}
}

func TestIsLoopbackListenAddress(t *testing.T) {
	assert.True(t, IsLoopbackListenAddress("127.0.0.1"))
	assert.True(t, IsLoopbackListenAddress("::1"))
	assert.True(t, IsLoopbackListenAddress("[::1]"))
	assert.True(t, IsLoopbackListenAddress("localhost"))
	assert.False(t, IsLoopbackListenAddress(""))
	assert.False(t, IsLoopbackListenAddress("0.0.0.0"))
	assert.False(t, IsLoopbackListenAddress("10.1.2.3"))
}

func TestRequestGuard(t *testing.T) {
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})

	newGuard := func(auth AuthConfig, withCORS bool) http.Handler {
		a := &Atc{Cfg: Config{Auth: auth}}
		a.Cfg.Server.AllowedOrigins = []string{"https://ops.example.com"}
		return a.requestGuard(withCORS)(next)
	}
	do := func(h http.Handler, method, host, path, origin string) *httptest.ResponseRecorder {
		reached = false
		req := httptest.NewRequest(method, "http://"+host+path, nil)
		req.Host = host
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	t.Run("DNS rebinding: unauthenticated local server rejects foreign Host", func(t *testing.T) {
		h := newGuard(AuthConfig{}, false)
		rr := do(h, http.MethodPost, "attacker.example:8088", "/api/overrides", "")
		assert.Equal(t, http.StatusForbidden, rr.Code)
		assert.False(t, reached)

		rr = do(h, http.MethodPost, "127.0.0.1:8088", "/api/overrides", "")
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("health probes bypass the Host check", func(t *testing.T) {
		rr := do(newGuard(AuthConfig{}, false), http.MethodGet, "10.0.0.7:8088", "/ready", "")
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("opt-in unauthenticated mode skips the Host check", func(t *testing.T) {
		rr := do(newGuard(AuthConfig{AllowUnauthenticated: true}, false), http.MethodGet, "atc.internal:8088", "/api/services", "")
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("cross-site request is rejected", func(t *testing.T) {
		h := newGuard(AuthConfig{Enabled: true}, false)
		rr := do(h, http.MethodPost, "atc.internal:8088", "/api/overrides", "https://evil.example")
		assert.Equal(t, http.StatusForbidden, rr.Code)
		assert.False(t, reached)
	})

	t.Run("MCP: no wildcard CORS, allowed origin is echoed", func(t *testing.T) {
		h := newGuard(AuthConfig{Enabled: true}, true)
		rr := do(h, http.MethodPost, "atc.internal:8092", "/mcp", "https://ops.example.com")
		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, "https://ops.example.com", rr.Header().Get("Access-Control-Allow-Origin"))

		rr = do(h, http.MethodPost, "atc.internal:8092", "/mcp", "https://evil.example")
		assert.Equal(t, http.StatusForbidden, rr.Code)
		assert.Empty(t, rr.Header().Get("Access-Control-Allow-Origin"))
	})

	t.Run("MCP: preflight is answered without reaching auth or handler", func(t *testing.T) {
		h := newGuard(AuthConfig{Enabled: true}, true)
		rr := do(h, http.MethodOptions, "atc.internal:8092", "/mcp", "https://ops.example.com")
		assert.Equal(t, http.StatusNoContent, rr.Code)
		assert.False(t, reached)
	})
}

// TestMCPSessionBoundToToken verifies that an MCP session created with one token cannot be
// used with another valid token (which would otherwise run tools with the first caller's
// Consul permissions).
func TestMCPSessionBoundToToken(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &Atc{
		Cfg:        Config{Auth: AuthConfig{Enabled: true, StaticKeys: []string{"key-alice", "key-bob"}}},
		logger:     logger,
		coreLogger: logger,
	}
	srv := httptest.NewServer(a.authMiddleware(a.bindMCPSession(mcp_server.NewHandler(a))))
	defer srv.Close()

	post := func(token, sessionID, body string) *http.Response {
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("X-ATC-Token", token)
		if sessionID != "" {
			req.Header.Set("Mcp-Session-Id", sessionID)
			req.Header.Set("Mcp-Protocol-Version", "2025-06-18")
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		return resp
	}

	initResp := post("key-alice", "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	_ = initResp.Body.Close()
	require.Equal(t, http.StatusOK, initResp.StatusCode)
	sessionID := initResp.Header.Get("Mcp-Session-Id")
	require.NotEmpty(t, sessionID)

	listTools := `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`

	bobResp := post("key-bob", sessionID, listTools)
	_ = bobResp.Body.Close()
	assert.NotEqual(t, http.StatusOK, bobResp.StatusCode, "another token must not be able to use alice's session")

	aliceResp := post("key-alice", sessionID, listTools)
	_ = aliceResp.Body.Close()
	assert.Equal(t, http.StatusOK, aliceResp.StatusCode)
}
