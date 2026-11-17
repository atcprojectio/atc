package atc

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const delegatedTokenCacheTTL = 30 * time.Second

func (t *Atc) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.cfgMu.RLock()
		authEnabled := t.Cfg.Auth.Enabled
		staticKeys := t.Cfg.Auth.StaticKeys
		tokenDelegation := t.Cfg.Auth.ConsulTokenDelegation
		consulAddr := t.Cfg.ConsulAddr
		consulDC := t.Cfg.ConsulDC
		t.cfgMu.RUnlock()

		clientIP := extractClientIP(r)
		ctx := context.WithValue(r.Context(), clientIPContextKey, clientIP)
		r = r.WithContext(ctx)

		span := trace.SpanFromContext(r.Context())
		span.SetAttributes(
			attribute.Bool("auth.enabled", authEnabled),
		)

		if !authEnabled {
			next.ServeHTTP(w, r)
			return
		}

		// Bypass health and readiness check endpoints
		path := r.URL.Path
		if path == "/ready" || path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		token := ""

		// 1. Extract from Authorization header (Bearer token)
		authHeader := r.Header.Get("Authorization")
		if strings.HasPrefix(authHeader, "Bearer ") {
			token = strings.TrimPrefix(authHeader, "Bearer ")
		}

		// 2. Extract from custom headers
		if token == "" {
			token = r.Header.Get("X-Consul-Token")
		}
		if token == "" {
			token = r.Header.Get("X-ATC-Token")
		}

		// Tokens are deliberately NOT accepted from the query string: URLs end up in access
		// logs, traces (otelhttp records them), browser history and Referer headers.

		if token == "" {
			span.SetStatus(codes.Error, "unauthorized: missing token")
			span.SetAttributes(
				attribute.String("auth.status", "failure"),
				attribute.String("auth.error", "missing_token"),
			)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized: missing token"})
			return
		}

		authorized := isStaticKey(staticKeys, token)

		// Validate Consul token delegation (with short TTL cache)
		if !authorized && tokenDelegation {
			var verifyErr error
			authorized, verifyErr = t.verifyDelegatedConsulToken(consulAddr, consulDC, token)
			if verifyErr != nil {
				// Consul is down: we cannot tell whether the token is valid. Saying "invalid
				// token" here would send operators hunting for a credentials problem.
				span.SetStatus(codes.Error, "consul unavailable for token verification")
				span.SetAttributes(
					attribute.String("auth.status", "failure"),
					attribute.String("auth.error", "consul_unavailable"),
				)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "10")
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{
					"error":   "consul_unavailable",
					"message": "Your token could not be checked because ATC cannot reach Consul. This is not a problem with your token.",
				})
				return
			}
		}

		if !authorized {
			span.SetStatus(codes.Error, "unauthorized: invalid token")
			span.SetAttributes(
				attribute.String("auth.status", "failure"),
				attribute.String("auth.error", "invalid_token"),
			)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized: invalid token"})
			return
		}

		span.SetAttributes(
			attribute.String("auth.status", "success"),
		)

		// Propagate token in context
		ctx = context.WithValue(r.Context(), tokenContextKey, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// verifyDelegatedConsulToken reports whether Consul accepts token. A non-nil error means
// Consul could not be asked (unreachable, no leader, 5xx), as opposed to a rejected token.
func (t *Atc) verifyDelegatedConsulToken(consulAddr, consulDC, token string) (bool, error) {
	cacheKey := consulAddr + "|" + consulDC + "|" + token
	now := time.Now()

	t.tokenCacheMu.RLock()
	if t.tokenCache != nil {
		if exp, ok := t.tokenCache[cacheKey]; ok && now.Before(exp) {
			t.tokenCacheMu.RUnlock()
			return true, nil
		}
	}
	t.tokenCacheMu.RUnlock()

	// Use an uncached client: caching per token here would let unauthenticated callers grow
	// the client cache without bound by sending random tokens.
	client, err := t.newRequestConsulClient(consulAddr, token, consulDC, "")
	if err != nil {
		return false, err
	}
	if _, err := client.Agent().Self(); err != nil {
		var se api.StatusError
		if errors.As(err, &se) && (se.Code == http.StatusForbidden || se.Code == http.StatusUnauthorized) {
			return false, nil // Consul answered: the token is not valid.
		}
		return false, err
	}

	t.tokenCacheMu.Lock()
	if t.tokenCache == nil {
		t.tokenCache = make(map[string]time.Time)
	}
	for k, exp := range t.tokenCache {
		if now.After(exp) {
			delete(t.tokenCache, k)
		}
	}
	t.tokenCache[cacheKey] = now.Add(delegatedTokenCacheTTL)
	t.tokenCacheMu.Unlock()
	return true, nil
}

// isStaticKey reports, in constant time per key, whether token is one of the static API keys.
func isStaticKey(staticKeys []string, token string) bool {
	found := false
	for _, sk := range staticKeys {
		if subtle.ConstantTimeCompare([]byte(sk), []byte(token)) == 1 {
			found = true
		}
	}
	return found
}

// bindMCPSession ties each MCP session to the token that created it. MCP tool calls run with
// the context of the session's initialize request, so without this a different (valid) token
// holder who learned a session ID would act with the original caller's Consul permissions.
// The go-sdk enforces the binding when requests carry auth.TokenInfo with a UserID; we derive
// that ID from a hash of the already-authenticated token.
func (t *Atc) bindMCPSession(next http.Handler) http.Handler {
	verifier := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		sum := sha256.Sum256([]byte(token))
		return &auth.TokenInfo{UserID: hex.EncodeToString(sum[:])}, nil
	}
	bound := auth.RequireBearerToken(verifier, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(next)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, _ := r.Context().Value(tokenContextKey).(string)
		if token == "" {
			// Auth disabled: nothing to bind to.
			next.ServeHTTP(w, r)
			return
		}
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+token)
		bound.ServeHTTP(w, r)
	})
}
