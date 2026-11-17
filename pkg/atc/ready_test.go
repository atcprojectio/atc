package atc

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	atc_server "github.com/atcprojectio/atc/pkg/atc/server"
)

func newReadinessAtc(consulAddr string) *Atc {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Atc{
		Cfg:           Config{ConsulAddr: consulAddr},
		logger:        logger,
		coreLogger:    logger,
		consulClients: make(map[string]*api.Client),
	}
}

func TestReadinessDoesNotDependOnConsul(t *testing.T) {
	probe := func(h http.HandlerFunc) int {
		rr := httptest.NewRecorder()
		h(rr, httptest.NewRequest(http.MethodGet, "/ready", nil))
		return rr.Code
	}

	// A Consul outage hits every replica at once; failing readiness would remove them all from
	// the Service and replace the dashboard's explanation with a bare load-balancer error.
	a := newReadinessAtc("127.0.0.1:1")
	assert.Equal(t, http.StatusOK, probe(a.readyHandler))
	assert.Equal(t, http.StatusOK, probe(a.healthHandler))

	a.shutdownRequested.Store(true)
	assert.Equal(t, http.StatusServiceUnavailable, probe(a.readyHandler))
	assert.Equal(t, http.StatusServiceUnavailable, probe(a.healthHandler))
}

func TestConsulStatusEndpoint(t *testing.T) {
	origTTL := consulStatusCacheTTL
	consulStatusCacheTTL = 0
	defer func() { consulStatusCacheTTL = origTTL }()

	var up atomic.Bool
	var leader atomic.Value
	up.Store(true)
	leader.Store(`"10.0.0.1:8300"`)
	consul := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("rpc error: connection refused to 10.9.8.7:8300"))
			return
		}
		if r.URL.Path == "/v1/status/leader" {
			_, _ = w.Write([]byte(leader.Load().(string)))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer consul.Close()

	a := newReadinessAtc(consul.Listener.Addr().String())
	get := func() (ConsulStatus, string) {
		rr := httptest.NewRecorder()
		a.apiStatusHandler(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
		require.Equal(t, http.StatusOK, rr.Code, "status endpoint itself must answer 200 so the UI can render it")
		var body struct {
			Consul ConsulStatus `json:"consul"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		return body.Consul, rr.Body.String()
	}

	st, _ := get()
	assert.True(t, st.Reachable)
	require.NotNil(t, st.LastContact)
	firstContact := *st.LastContact

	up.Store(false)
	st, raw := get()
	assert.False(t, st.Reachable)
	assert.Equal(t, consulProblemUnreachable, st.Problem)
	assert.Equal(t, "ATC cannot reach Consul.", st.Message)
	require.NotNil(t, st.LastContact, "last successful contact must survive the outage")
	assert.Equal(t, firstContact, *st.LastContact)
	assert.NotContains(t, raw, "10.9.8.7", "unauthenticated endpoint must not leak internal addresses")

	up.Store(true)
	leader.Store(`""`)
	st, _ = get()
	assert.False(t, st.Reachable)
	assert.Equal(t, consulProblemNoLeader, st.Problem)
}

func TestConsulStatusEndpointNeedsNoToken(t *testing.T) {
	a := newReadinessAtc("127.0.0.1:1")
	a.Cfg.Auth = AuthConfig{Enabled: true, StaticKeys: []string{"k"}}
	srv, err := atc_server.New(atc_server.Config{}, a.logger)
	require.NoError(t, err)
	a.Server = srv
	srv.Mux.HandleFunc("GET /api/status", a.apiStatusHandler)
	srv.Mux.Handle("GET /api/services", a.authMiddleware(http.HandlerFunc(a.apiServicesHandler)))

	rr := httptest.NewRecorder()
	srv.Mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	assert.Equal(t, http.StatusOK, rr.Code)

	rr = httptest.NewRecorder()
	srv.Mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/services", nil))
	assert.Equal(t, http.StatusUnauthorized, rr.Code, "other API routes stay protected")
}

func TestDelegatedAuthReportsConsulOutage(t *testing.T) {
	var status atomic.Int32
	consul := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer consul.Close()

	a := newReadinessAtc(consul.Listener.Addr().String())
	a.Cfg.Auth = AuthConfig{Enabled: true, ConsulTokenDelegation: true}
	a.tokenCache = make(map[string]time.Time)
	h := a.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/services", nil)
		req.Header.Set("X-Consul-Token", "some-token")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	status.Store(http.StatusForbidden)
	assert.Equal(t, http.StatusUnauthorized, call().Code, "Consul rejecting the token is an auth failure")

	status.Store(http.StatusInternalServerError)
	rr := call()
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code, "Consul failing is not the caller's fault")
	assert.Contains(t, rr.Body.String(), "consul_unavailable")

	consul.Close()
	assert.Equal(t, http.StatusServiceUnavailable, call().Code, "unreachable Consul is reported as unavailable")
}

func TestActiveOverridesCountIsCached(t *testing.T) {
	var lists atomic.Int32
	consul := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/config/service-resolver" {
			lists.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"Kind":"service-resolver","Name":"payments","Meta":{"created-by":"atc-override"}}]`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer consul.Close()

	a := newReadinessAtc(consul.Listener.Addr().String())
	for i := 0; i < 5; i++ {
		n, ok := a.activeOverridesCount(context.Background())
		require.True(t, ok)
		assert.Equal(t, int64(1), n)
	}
	assert.Equal(t, int32(1), lists.Load(), "scrapes within the TTL must not query Consul again")
}
