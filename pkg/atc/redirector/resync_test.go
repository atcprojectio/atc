package redirector

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/atcprojectio/atc/pkg/atc/watcher"
)

type fakeConsul struct {
	srv           *httptest.Server
	resolverLists atomic.Int32
	puts          atomic.Int32
	failPuts      atomic.Bool
}

func newFakeConsul(t *testing.T) *fakeConsul {
	t.Helper()
	fc := &fakeConsul{}
	fc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("index") != "" {
			<-r.Context().Done() // blocking watch query
			return
		}
		w.Header().Set("X-Consul-Index", "1")
		w.Header().Set("X-Consul-LastContact", "0")
		w.Header().Set("X-Consul-KnownLeader", "true")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/catalog/services":
			_, _ = w.Write([]byte(`{"payments":["atc.enabled=true"]}`))
		case r.URL.Path == "/v1/catalog/datacenters":
			_, _ = w.Write([]byte(`["dc1","dc2"]`))
		case r.URL.Path == "/v1/config/service-resolver" && r.Method == http.MethodGet:
			fc.resolverLists.Add(1)
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPut:
			fc.puts.Add(1)
			if fc.failPuts.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(`true`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(fc.srv.Close)
	return fc
}

func (fc *fakeConsul) addr() string { return fc.srv.Listener.Addr().String() }

// newStandaloneRedirector writes base resolvers itself (no forwarder running).
func newStandaloneRedirector(t *testing.T, addr string) *Redirector {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := New(logger, addr, "", "dc1", "", "", false, nil, "", "", false, false)
	require.NoError(t, err)
	r.SetWatcher(watcher.New(logger, addr, "", "dc1", ""))
	return r
}

func runUntilQuiet(t *testing.T, resync time.Duration) int32 {
	t.Helper()
	fc := newFakeConsul(t)
	fc.failPuts.Store(true)
	r := newStandaloneRedirector(t, fc.addr())
	r.SetResyncInterval(resync)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()

	require.Eventually(t, func() bool { return fc.puts.Load() >= 1 }, 2*time.Second, 10*time.Millisecond)
	time.Sleep(300 * time.Millisecond) // startup events settle
	settled := fc.puts.Load()
	fc.failPuts.Store(false)
	time.Sleep(600 * time.Millisecond)
	after := fc.puts.Load() - settled

	cancel()
	require.NoError(t, <-done)
	return after
}

func TestRedirectorResyncRetriesFailedWrites(t *testing.T) {
	assert.Positive(t, runUntilQuiet(t, 100*time.Millisecond), "the periodic resync must retry the failed write")
}

func TestRedirectorWithoutResyncDoesNotRetry(t *testing.T) {
	assert.Zero(t, runUntilQuiet(t, 0), "control: the retry comes from the resync")
}

func TestRedirectorSetConnectionSwitchesConsul(t *testing.T) {
	oldConsul, newConsul := newFakeConsul(t), newFakeConsul(t)
	r := newStandaloneRedirector(t, oldConsul.addr())

	require.NoError(t, r.reconcileWithCurrentClient(context.Background()))
	r.SetConnection(newConsul.addr(), "", "dc1", "")
	require.NoError(t, r.reconcileWithCurrentClient(context.Background()))

	assert.Equal(t, int32(1), oldConsul.resolverLists.Load(), "old Consul must no longer be used")
	assert.Equal(t, int32(1), newConsul.resolverLists.Load())
}
