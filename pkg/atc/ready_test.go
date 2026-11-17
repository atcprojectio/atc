package atc

import (
	"context"
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

func TestReadiness(t *testing.T) {
	origTTL := readinessCacheTTL
	readinessCacheTTL = time.Hour
	defer func() { readinessCacheTTL = origTTL }()

	var leader atomic.Value
	leader.Store(`"10.0.0.1:8300"`)
	var probes atomic.Int32
	consul := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/status/leader" {
			probes.Add(1)
			_, _ = w.Write([]byte(leader.Load().(string)))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer consul.Close()

	probe := func(a *Atc, h http.HandlerFunc) int {
		rr := httptest.NewRecorder()
		h(rr, httptest.NewRequest(http.MethodGet, "/ready", nil))
		return rr.Code
	}

	t.Run("ready when Consul has a leader, and cached", func(t *testing.T) {
		a := newReadinessAtc(consul.Listener.Addr().String())
		assert.Equal(t, http.StatusOK, probe(a, a.readyHandler))
		assert.Equal(t, http.StatusOK, probe(a, a.readyHandler))
		assert.Equal(t, int32(1), probes.Load(), "second probe should be served from cache")
	})

	t.Run("not ready when Consul has no leader", func(t *testing.T) {
		leader.Store(`""`)
		defer leader.Store(`"10.0.0.1:8300"`)
		a := newReadinessAtc(consul.Listener.Addr().String())
		err := a.CheckReadiness(context.Background())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no leader")
	})

	t.Run("not ready when Consul is unreachable, but still live", func(t *testing.T) {
		a := newReadinessAtc("127.0.0.1:1")
		assert.Equal(t, http.StatusServiceUnavailable, probe(a, a.readyHandler))
		assert.Equal(t, http.StatusOK, probe(a, a.healthHandler), "liveness must not depend on Consul")
	})

	t.Run("not ready and not live while shutting down", func(t *testing.T) {
		a := newReadinessAtc(consul.Listener.Addr().String())
		a.shutdownRequested.Store(true)
		assert.Equal(t, http.StatusServiceUnavailable, probe(a, a.readyHandler))
		assert.Equal(t, http.StatusServiceUnavailable, probe(a, a.healthHandler))
	})
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
