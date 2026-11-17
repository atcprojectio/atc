package forwarder

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

	"github.com/atcprojectio/atc/pkg/atc/watcher"
)

// fakeConsul serves just enough of the Consul API for reconcile and the watcher.
type fakeConsul struct {
	srv           *httptest.Server
	resolverLists atomic.Int32
	resolverPuts  atomic.Int32
	failPuts      atomic.Bool
	tags          atomic.Value // []string for the "payments" service
	resolvers     atomic.Value // JSON list returned for service-resolver entries
	datacenters   atomic.Value // JSON list, nearest first
	lastPut       atomic.Value // body of the last config write
	current       atomic.Value // JSON of the single "payments" resolver, "" = not found
}

func newFakeConsul(t *testing.T) *fakeConsul {
	t.Helper()
	fc := &fakeConsul{}
	fc.tags.Store([]string{"atc.enabled=true"})
	fc.resolvers.Store(`[]`)
	fc.datacenters.Store(`["dc1","dc2"]`)
	fc.lastPut.Store("")
	fc.current.Store("")
	fc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Blocking watch queries carry an index: hang like a real Consul until the client gives up.
		if r.URL.Query().Get("index") != "" {
			<-r.Context().Done()
			return
		}
		w.Header().Set("X-Consul-Index", "1")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/catalog/services":
			_ = json.NewEncoder(w).Encode(map[string][]string{"payments": fc.tags.Load().([]string)})
		case r.URL.Path == "/v1/health/state/any":
			_, _ = w.Write([]byte(`[]`))
		case r.URL.Path == "/v1/catalog/datacenters":
			_, _ = w.Write([]byte(fc.datacenters.Load().(string)))
		case r.URL.Path == "/v1/config/service-resolver/payments" && r.Method == http.MethodGet:
			if cur := fc.current.Load().(string); cur != "" {
				_, _ = w.Write([]byte(cur))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/v1/config/service-resolver" && r.Method == http.MethodGet:
			fc.resolverLists.Add(1)
			_, _ = w.Write([]byte(fc.resolvers.Load().(string)))
		case r.URL.Path == "/v1/config" && r.Method == http.MethodPut:
			fc.resolverPuts.Add(1)
			body, _ := io.ReadAll(r.Body)
			fc.lastPut.Store(string(body))
			if fc.failPuts.Load() {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("rpc error"))
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

func newTestForwarder(t *testing.T, addr string) *Forwarder {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f, err := New(logger, addr, "", "dc1", "", "", nil, "", "", false)
	require.NoError(t, err)
	f.SetWatcher(watcher.New(logger, addr, "", "dc1", ""))
	return f
}

func TestReconcileReportsFailedWrites(t *testing.T) {
	fc := newFakeConsul(t)
	fc.failPuts.Store(true)
	f := newTestForwarder(t, fc.addr())

	client, err := api.NewClient(&api.Config{Address: fc.addr()})
	require.NoError(t, err)
	err = f.reconcile(context.Background(), client)
	require.Error(t, err, "a failed resolver write must fail the reconcile so it is retried")
	assert.Equal(t, int32(1), fc.resolverPuts.Load())
}

// runUntilQuiet starts f.Run with a failing Consul, waits until the startup reconciles
// (initial run plus the watcher's first, non-blocking fetches) have settled, then lets
// Consul recover and reports how many writes happen afterwards without any catalog event.
func runUntilQuiet(t *testing.T, resync time.Duration) int32 {
	t.Helper()
	fc := newFakeConsul(t)
	fc.failPuts.Store(true)
	f := newTestForwarder(t, fc.addr())
	f.SetResyncInterval(resync)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()

	require.Eventually(t, func() bool { return fc.resolverPuts.Load() >= 1 }, 2*time.Second, 10*time.Millisecond)
	time.Sleep(300 * time.Millisecond) // startup events settle; watch queries now block
	settled := fc.resolverPuts.Load()
	fc.failPuts.Store(false) // Consul recovers; no catalog event follows

	time.Sleep(600 * time.Millisecond)
	after := fc.resolverPuts.Load() - settled

	cancel()
	require.NoError(t, <-done)
	return after
}

func TestPeriodicResyncRetriesWithoutCatalogEvents(t *testing.T) {
	assert.Positive(t, runUntilQuiet(t, 100*time.Millisecond),
		"the periodic resync must retry the failed write without any catalog event")
}

func TestWithoutResyncFailedWriteIsNotRetried(t *testing.T) {
	// Control: shows the retry above comes from the resync, not from catalog events.
	assert.Zero(t, runUntilQuiet(t, 0))
}

func TestSetConnectionSwitchesConsulClient(t *testing.T) {
	oldConsul := newFakeConsul(t)
	newConsul := newFakeConsul(t)
	f := newTestForwarder(t, oldConsul.addr())

	require.NoError(t, f.reconcileWithCurrentClient(context.Background()))
	require.Equal(t, int32(1), oldConsul.resolverLists.Load())

	f.SetConnection(newConsul.addr(), "", "dc1", "")
	require.NoError(t, f.reconcileWithCurrentClient(context.Background()))
	assert.Equal(t, int32(1), oldConsul.resolverLists.Load(), "old Consul must no longer be used")
	assert.Equal(t, int32(1), newConsul.resolverLists.Load())
}

func TestFailoverEntryRecordsRemovalIntent(t *testing.T) {
	intent := RemovalIntentMeta([]string{"atc.enabled=true", "atc.redirect=geo-redirect", "atc.dampening=30s"})
	assert.Equal(t, map[string]string{MetaRedirectOnRemoval: "geo-redirect", MetaDampeningOnRemoval: "30s"}, intent)
	assert.Empty(t, RemovalIntentMeta([]string{"atc.enabled=true"}))

	// An existing entry without the intent must be rewritten once to record it.
	assert.False(t, removalIntentMatches(map[string]string{"created-by": "atc"}, intent))
	assert.True(t, removalIntentMatches(withRemovalIntent(map[string]string{"created-by": "atc"}, intent), intent))
}

func TestForeignResolverIsLeftAloneUnlessAdopted(t *testing.T) {
	fc := newFakeConsul(t)
	// Hand-written resolver with a subset ATC would drop if it replaced the entry.
	fc.resolvers.Store(`[{"Kind":"service-resolver","Name":"payments","DefaultSubset":"v2","Subsets":{"v2":{"Filter":"Service.Meta.version == 2"}}}]`)
	f := newTestForwarder(t, fc.addr())
	client, err := api.NewClient(&api.Config{Address: fc.addr()})
	require.NoError(t, err)

	require.NoError(t, f.reconcile(context.Background(), client))
	assert.Zero(t, fc.resolverPuts.Load(), "a resolver ATC did not create must not be replaced")
	assert.Len(t, f.foreign.services, 1)

	fc.tags.Store([]string{"atc.enabled=true", AdoptTag})
	require.NoError(t, f.reconcile(context.Background(), client))
	assert.Equal(t, int32(1), fc.resolverPuts.Load(), "atc.adopt=true lets ATC take it over")
	assert.Empty(t, f.foreign.services)
}

type countingHandler struct {
	slog.Handler
	warns *atomic.Int32
}

func (h countingHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level == slog.LevelWarn {
		h.warns.Add(1)
	}
	return nil
}

func TestForeignTrackerWarnsOncePerService(t *testing.T) {
	var warns atomic.Int32
	logger := slog.New(countingHandler{Handler: slog.NewTextHandler(io.Discard, nil), warns: &warns})
	ft := &ForeignTracker{module: "forwarder", logger: logger, services: map[string]struct{}{}}

	ft.Update(context.Background(), map[string]struct{}{"a": {}})
	ft.Update(context.Background(), map[string]struct{}{"a": {}})
	ft.Update(context.Background(), map[string]struct{}{"a": {}, "b": {}})
	assert.Equal(t, int32(2), warns.Load())
}
