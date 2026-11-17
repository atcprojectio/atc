package atc

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
)

// TestDelegatedTokensDoNotGrowClientCache: with token delegation on, unauthenticated callers
// can send arbitrary tokens. Each one must not leave a cached Consul client behind.
func TestDelegatedTokensDoNotGrowClientCache(t *testing.T) {
	consul := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer consul.Close()

	a := &Atc{
		Cfg: Config{
			ConsulAddr: consul.Listener.Addr().String(),
			Auth:       AuthConfig{Enabled: true, ConsulTokenDelegation: true},
		},
		consulClients: make(map[string]*api.Client),
		tokenCache:    make(map[string]time.Time),
	}
	h := a.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	for i := 0; i < 50; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/services", nil)
		req.Header.Set("X-ATC-Token", fmt.Sprintf("random-token-%d", i))
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	}

	a.consulClientsMu.RLock()
	defer a.consulClientsMu.RUnlock()
	assert.Empty(t, a.consulClients)
}
