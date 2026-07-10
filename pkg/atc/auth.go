package atc

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hashicorp/consul/api"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func (t *Atc) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.cfgMu.RLock()
		authEnabled := t.Cfg.Auth.Enabled
		staticKeys := t.Cfg.Auth.StaticKeys
		tokenDelegation := t.Cfg.Auth.ConsulTokenDelegation
		consulAddr := t.Cfg.ConsulAddr
		consulDC := t.Cfg.ConsulDC
		t.cfgMu.RUnlock()

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

		// Validate static keys
		authorized := false
		for _, sk := range staticKeys {
			if sk == token {
				authorized = true
				break
			}
		}

		// Validate Consul token delegation
		if !authorized && tokenDelegation {
			config := api.DefaultConfig()
			config.Address = consulAddr
			config.Token = token
			if consulDC != "" {
				config.Datacenter = consulDC
			}
			client, err := api.NewClient(config)
			if err == nil {
				_, err = client.Agent().Self()
				if err == nil {
					authorized = true
				}
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
		ctx := context.WithValue(r.Context(), tokenContextKey, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
