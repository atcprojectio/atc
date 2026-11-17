package atc

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
)

func newDelegationAtc(t *testing.T, accept func(token string) bool) (*Atc, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	consul := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if accept(r.Header.Get("X-Consul-Token")) {
			_, _ = w.Write([]byte(`{"Config":{}}`))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(consul.Close)
	return &Atc{
		Cfg:           Config{ConsulAddr: consul.Listener.Addr().String(), Auth: AuthConfig{Enabled: true, ConsulTokenDelegation: true}},
		consulClients: map[string]*api.Client{},
	}, &calls
}

func authCall(a *Atc, token string) int {
	h := a.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/api/services", nil)
	req.Header.Set("X-Consul-Token", token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code
}

// TestRandomTokensCannotFloodConsul: unauthenticated callers choose the token, so uncached
// checks are capped; beyond the burst they get 429 without a Consul request.
func TestRandomTokensCannotFloodConsul(t *testing.T) {
	a, calls := newDelegationAtc(t, func(string) bool { return false })
	limited := 0
	for i := 0; i < 200; i++ {
		if authCall(a, fmt.Sprintf("random-%d", i)) == http.StatusTooManyRequests {
			limited++
		}
	}
	assert.LessOrEqual(t, calls.Load(), int32(tokenVerificationsBurst+5), "Consul calls must be capped near the burst size")
	assert.Positive(t, limited)
}

func TestRejectedTokenIsCachedBriefly(t *testing.T) {
	a, calls := newDelegationAtc(t, func(string) bool { return false })
	for i := 0; i < 10; i++ {
		assert.Equal(t, http.StatusUnauthorized, authCall(a, "same-bad-token"))
	}
	assert.Equal(t, int32(1), calls.Load(), "a repeated bad token is answered from the cache")
}

func TestValidTokensKeepWorkingDuringAFlood(t *testing.T) {
	a, _ := newDelegationAtc(t, func(tok string) bool { return tok == "good" })
	assert.Equal(t, http.StatusOK, authCall(a, "good")) // cached as valid
	for i := 0; i < 200; i++ {
		authCall(a, fmt.Sprintf("random-%d", i))
	}
	assert.Equal(t, http.StatusOK, authCall(a, "good"), "a cached valid token is not affected by the limit")
}

func TestTokenCachesAreBounded(t *testing.T) {
	a := &Atc{}
	now := time.Now()
	for i := 0; i < maxCachedTokens+100; i++ {
		a.rememberToken(&a.rejectedTokens, fmt.Sprintf("k%d", i), now.Add(time.Minute), now)
	}
	assert.Len(t, a.rejectedTokens, maxCachedTokens)
}
