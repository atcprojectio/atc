package redirector

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/atcprojectio/atc/pkg/atc/forwarder"
)

// TestNewLeaderRedirectsUsingPersistedIntent simulates a leader change during an incident:
// the service disappears and the new leader has never seen its atc.redirect tag. The redirect
// must still follow the configured strategy, read back from the resolver's metadata.
func TestNewLeaderRedirectsUsingPersistedIntent(t *testing.T) {
	var mu sync.Mutex
	var written *api.ServiceResolverConfigEntry

	consul := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/catalog/services":
			_, _ = w.Write([]byte(`{"consul":[]}`)) // "payments" is gone
		case r.URL.Path == "/v1/catalog/datacenters":
			_, _ = w.Write([]byte(`["dc1","dc2","dc3"]`))
		case r.URL.Path == "/v1/config/service-resolver" && r.Method == http.MethodGet:
			// Failover entry written earlier by the forwarder, carrying the redirect intent.
			_, _ = w.Write([]byte(`[{"Kind":"service-resolver","Name":"payments","Meta":{"created-by":"atc","failover-strategy":"standard-failover","` +
				forwarder.MetaRedirectOnRemoval + `":"geo-redirect"},"Failover":{"*":{"Targets":[{"Service":"payments","Datacenter":"dc2"}]}}}]`))
		case r.URL.Path == "/v1/config" && r.Method == http.MethodPut:
			var e api.ServiceResolverConfigEntry
			require.NoError(t, json.NewDecoder(r.Body).Decode(&e))
			mu.Lock()
			written = &e
			mu.Unlock()
			_, _ = w.Write([]byte(`true`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer consul.Close()

	strategies := map[string]RedirectStrategy{
		"geo-redirect": {Datacenter: "dc3", Service: "geo-fallback"},
	}
	r, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), consul.Listener.Addr().String(), "", "dc1", "", "", false, strategies, "", "", false, false)
	require.NoError(t, err)

	client, err := api.NewClient(&api.Config{Address: consul.Listener.Addr().String()})
	require.NoError(t, err)
	require.NoError(t, r.reconcile(context.Background(), client))

	mu.Lock()
	defer mu.Unlock()
	require.NotNil(t, written, "expected a redirect entry to be written")
	require.NotNil(t, written.Redirect)
	assert.Equal(t, "geo-fallback", written.Redirect.Service)
	assert.Equal(t, "dc3", written.Redirect.Datacenter, "must follow the configured strategy, not the nearest DC")
	assert.Equal(t, "geo-redirect", written.Meta["redirect-strategy"])
}
