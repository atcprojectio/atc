package redirector

import (
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

// TestQueuedRedirectDoesNotResurrectPurgedResolver: a service is retired (deregistered), the
// redirector queues a dampened redirect write, and the operator purges the resolver before it
// fires. The queued write must not put the entry back.
func TestQueuedRedirectDoesNotResurrectPurgedResolver(t *testing.T) {
	var current atomic.Value
	current.Store(`{"Kind":"service-resolver","Name":"payments","Meta":{"created-by":"atc"}}`)
	var puts atomic.Int32
	consul := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/config/service-resolver/payments":
			if cur := current.Load().(string); cur != "" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(cur))
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPut:
			puts.Add(1)
			_, _ = w.Write([]byte(`true`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer consul.Close()

	r, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), consul.Listener.Addr().String(), "", "dc1", "", "", false, nil, "", "", false, false)
	require.NoError(t, err)

	redirect := &api.ServiceResolverConfigEntry{
		Kind: api.ServiceResolver, Name: "payments",
		Meta:     map[string]string{"created-by": "atc"},
		Redirect: &api.ServiceResolverRedirect{Service: "payments", Datacenter: "dc2"},
	}
	r.scheduleWrite("payments", redirect, 30*time.Millisecond, "created-by=atc")
	current.Store("") // purged, e.g. through DELETE /api/services on another node

	time.Sleep(200 * time.Millisecond)
	assert.Zero(t, puts.Load(), "the queued redirect must not recreate a purged resolver")
}
