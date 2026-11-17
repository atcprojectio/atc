package watcher

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
)

func newBlockingConsul(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Query().Get("index") != "" {
			<-r.Context().Done()
			return
		}
		w.Header().Set("X-Consul-Index", "1")
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/catalog/services" {
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestSetConnectionRestartsRunningWatch(t *testing.T) {
	var oldHits, newHits atomic.Int32
	oldConsul := newBlockingConsul(t, &oldHits)
	newConsul := newBlockingConsul(t, &newHits)

	w := New(slog.New(slog.NewTextHandler(io.Discard, nil)), oldConsul.Listener.Addr().String(), "", "", "")
	events := w.Events.Subscribe()
	defer w.Events.Unsubscribe(events)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	require.Eventually(t, func() bool { return oldHits.Load() >= 2 }, 2*time.Second, 10*time.Millisecond)

	w.SetConnection(newConsul.Listener.Addr().String(), "", "", "")

	require.Eventually(t, func() bool { return newHits.Load() >= 2 }, 2*time.Second, 10*time.Millisecond,
		"after a connection change the running watch must query the new Consul")
	gotChanged := false
	deadline := time.After(2 * time.Second)
	for !gotChanged {
		select {
		case ev := <-events:
			gotChanged = ev == "connection_changed"
		case <-deadline:
			t.Fatal("expected a connection_changed event")
		}
	}

	settledOld := oldHits.Load()
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, settledOld, oldHits.Load(), "the old Consul must no longer be polled")

	cancel()
	require.NoError(t, <-done)
}

func TestSetConnectionWithoutChangeDoesNotRestart(t *testing.T) {
	w := New(slog.New(slog.NewTextHandler(io.Discard, nil)), "a:1", "t", "dc1", "ns")
	w.SetConnection("a:1", "t", "dc1", "ns")
	assert.Empty(t, w.restartCh)
	w.SetNamespace("other")
	assert.Len(t, w.restartCh, 1)
}

// TestWatcherDoesNotWatchHealthChecks: reconciliation only reads the service catalog, so the
// watcher must not poll every health check in the datacenter (each flap caused a reconcile).
func TestWatcherDoesNotWatchHealthChecks(t *testing.T) {
	var healthQueries, catalogQueries atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/health/state/any":
			healthQueries.Add(1)
		case "/v1/catalog/services":
			catalogQueries.Add(1)
		}
		if r.URL.Query().Get("index") != "" {
			<-r.Context().Done()
			return
		}
		w.Header().Set("X-Consul-Index", "1")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	w := New(slog.New(slog.NewTextHandler(io.Discard, nil)), srv.Listener.Addr().String(), "", "", "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	require.Eventually(t, func() bool { return catalogQueries.Load() >= 2 }, 2*time.Second, 10*time.Millisecond)
	cancel()
	require.NoError(t, <-done)
	assert.Zero(t, healthQueries.Load())
}
