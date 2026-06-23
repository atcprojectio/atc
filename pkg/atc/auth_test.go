package atc

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAuthMiddleware(t *testing.T) {
	// Mock Consul server for token delegation validation
	var mockConsulResponseStatus int
	var mockConsulRequestsCount int
	var lastReceivedToken string

	mockConsul := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mockConsulRequestsCount++
		lastReceivedToken = r.Header.Get("X-Consul-Token")
		if lastReceivedToken == "" {
			lastReceivedToken = r.URL.Query().Get("token")
		}
		if authHeader := r.Header.Get("Authorization"); authHeader != "" {
			lastReceivedToken = strings.TrimPrefix(authHeader, "Bearer ")
		}

		w.WriteHeader(mockConsulResponseStatus)
		if mockConsulResponseStatus == http.StatusOK {
			_, _ = w.Write([]byte(`{"Config":{}}`))
		}
	}))
	defer mockConsul.Close()

	atcInstance := &Atc{
		Cfg: Config{
			ConsulAddr: mockConsul.URL,
			Auth: AuthConfig{
				Enabled:               true,
				StaticKeys:            []string{"valid-static-token"},
				ConsulTokenDelegation: true,
			},
		},
	}

	testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ready" && atcInstance.Cfg.Auth.Enabled {
			ctxToken := r.Context().Value(tokenContextKey)
			assert.NotNil(t, ctxToken)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	authHandler := atcInstance.authMiddleware(testHandler)

	t.Run("Disabled auth allows everything", func(t *testing.T) {
		atcInstance.Cfg.Auth.Enabled = false
		req := httptest.NewRequest("GET", "/api/services", nil)
		rr := httptest.NewRecorder()
		authHandler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
		atcInstance.Cfg.Auth.Enabled = true // restore
	})

	t.Run("Ready path is bypassed", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/ready", nil)
		rr := httptest.NewRecorder()
		authHandler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("Missing token returns 401", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/services", nil)
		rr := httptest.NewRecorder()
		authHandler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusUnauthorized, rr.Code)
		var body map[string]string
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		assert.Equal(t, "unauthorized: missing token", body["error"])
	})

	t.Run("Valid static token in Authorization Header is accepted", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/services", nil)
		req.Header.Set("Authorization", "Bearer valid-static-token")
		rr := httptest.NewRecorder()
		authHandler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("Valid static token in X-ATC-Token Header is accepted", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/services", nil)
		req.Header.Set("X-ATC-Token", "valid-static-token")
		rr := httptest.NewRecorder()
		authHandler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("Valid static token in query parameter is accepted", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/services?token=valid-static-token", nil)
		rr := httptest.NewRecorder()
		authHandler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	t.Run("Valid Consul delegated token is accepted", func(t *testing.T) {
		mockConsulResponseStatus = http.StatusOK
		mockConsulRequestsCount = 0

		req := httptest.NewRequest("GET", "/api/services", nil)
		req.Header.Set("Authorization", "Bearer valid-consul-token")
		rr := httptest.NewRecorder()
		authHandler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusOK, rr.Code)
		assert.Equal(t, 1, mockConsulRequestsCount)
		assert.Equal(t, "valid-consul-token", lastReceivedToken)
	})

	t.Run("Invalid Consul delegated token returns 401", func(t *testing.T) {
		mockConsulResponseStatus = http.StatusForbidden
		mockConsulRequestsCount = 0

		req := httptest.NewRequest("GET", "/api/services", nil)
		req.Header.Set("Authorization", "Bearer invalid-consul-token")
		rr := httptest.NewRecorder()
		authHandler.ServeHTTP(rr, req)

		assert.Equal(t, http.StatusUnauthorized, rr.Code)
		var body map[string]string
		_ = json.Unmarshal(rr.Body.Bytes(), &body)
		assert.Equal(t, "unauthorized: invalid token", body["error"])
	})
}
