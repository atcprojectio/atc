package forwarder

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPickDatacenter(t *testing.T) {
	assert.Equal(t, "dc3", PickDatacenter([]string{"dc2", "dc3"}, "dc3"), "keeps the previous choice while available")
	assert.Equal(t, "dc2", PickDatacenter([]string{"dc2", "dc3"}, "dc9"), "previous DC gone: nearest")
	assert.Equal(t, "dc2", PickDatacenter([]string{"dc2", "dc3"}, ""))
	assert.Equal(t, "", PickDatacenter(nil, "dc2"), "no other datacenter")
}

func TestLocalDatacenterDoesNotGuess(t *testing.T) {
	consul := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer consul.Close()
	client, err := api.NewClient(&api.Config{Address: consul.Listener.Addr().String()})
	require.NoError(t, err)

	_, err = LocalDatacenter(client, "")
	assert.Error(t, err, "must not fall back to a made-up dc1")
	dc, err := LocalDatacenter(client, "eu-west")
	require.NoError(t, err)
	assert.Equal(t, "eu-west", dc)
}

func TestSingleDatacenterWritesNoFailover(t *testing.T) {
	fc := newFakeConsul(t)
	fc.datacenters.Store(`["dc1"]`)
	f := newTestForwarder(t, fc.addr())
	client, err := api.NewClient(&api.Config{Address: fc.addr()})
	require.NoError(t, err)

	require.NoError(t, f.reconcile(context.Background(), client))
	assert.Zero(t, fc.resolverPuts.Load(), "must not write a failover to a datacenter that does not exist")
}

func TestAutoChosenDatacenterIsStable(t *testing.T) {
	fc := newFakeConsul(t)
	// Previously chose dc3; round-trip times now rank dc2 first.
	fc.datacenters.Store(`["dc1","dc2","dc3"]`)
	fc.resolvers.Store(`[{"Kind":"service-resolver","Name":"payments","ConnectTimeout":"15s","Meta":{"created-by":"atc","failover-strategy":""},"Failover":{"*":{"Targets":[{"Service":"payments","Datacenter":"dc3"}]}}}]`)
	f := newTestForwarder(t, fc.addr())
	client, err := api.NewClient(&api.Config{Address: fc.addr()})
	require.NoError(t, err)

	require.NoError(t, f.reconcile(context.Background(), client))
	assert.Zero(t, fc.resolverPuts.Load(), "the resolver must not flip to dc2 just because it is now nearer")

	fc.datacenters.Store(`["dc1","dc2"]`) // dc3 is gone
	require.NoError(t, f.reconcile(context.Background(), client))
	require.Equal(t, int32(1), fc.resolverPuts.Load())
	assert.Contains(t, fc.lastPut.Load().(string), `"Datacenter":"dc2"`)
}

func TestUnknownStrategyTagIsReported(t *testing.T) {
	fc := newFakeConsul(t)
	fc.tags.Store([]string{"atc.enabled=true", "atc.failover=standard-failvoer"})
	var warns atomic.Int32
	logger := slog.New(countingHandler{Handler: slog.NewTextHandler(io.Discard, nil), warns: &warns})
	f, err := New(logger, fc.addr(), "", "dc1", "", "", map[string]FailoverStrategy{"standard-failover": {}}, "", "", false)
	require.NoError(t, err)
	client, err := api.NewClient(&api.Config{Address: fc.addr()})
	require.NoError(t, err)

	require.NoError(t, f.reconcile(context.Background(), client))
	require.NoError(t, f.reconcile(context.Background(), client))
	assert.Equal(t, int32(1), warns.Load(), "a typo in the strategy tag is warned about, once")
}

// TestReconcileConcurrentWithSetConnection must pass under -race.
func TestReconcileConcurrentWithSetConnection(t *testing.T) {
	fc := newFakeConsul(t)
	f := newTestForwarder(t, fc.addr())
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _ = f.reconcileWithCurrentClient(context.Background()) }()
		go func(i int) {
			defer wg.Done()
			dc := "dc1"
			if i%2 == 0 {
				dc = "dc-other"
			}
			f.SetConnection(fc.addr(), "", dc, "")
		}(i)
	}
	wg.Wait()
}
