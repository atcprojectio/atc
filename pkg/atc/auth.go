package atc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

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

		// 3. Extract from query parameter
		if token == "" {
			token = r.URL.Query().Get("token")
		}

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

		// Validate static keys in constant time
		authorized := false
		for _, sk := range staticKeys {
			if subtle.ConstantTimeCompare([]byte(sk), []byte(token)) == 1 {
				authorized = true
				break
			}
		}

		// Validate Consul token delegation (with short TTL cache)
		if !authorized && tokenDelegation {
			authorized = t.verifyDelegatedConsulToken(consulAddr, consulDC, token)
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

func (t *Atc) verifyDelegatedConsulToken(consulAddr, consulDC, token string) bool {
	cacheKey := consulAddr + "|" + consulDC + "|" + token
	now := time.Now()

	t.tokenCacheMu.RLock()
	if t.tokenCache != nil {
		if exp, ok := t.tokenCache[cacheKey]; ok && now.Before(exp) {
			t.tokenCacheMu.RUnlock()
			return true
		}
	}
	t.tokenCacheMu.RUnlock()

	client, err := t.getOrCreateConsulClient(consulAddr, token, consulDC, "")
	if err != nil {
		return false
	}
	if _, err := client.Agent().Self(); err != nil {
		return false
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
	return true
}
