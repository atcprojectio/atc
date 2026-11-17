package redirector

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSingleDatacenterWritesNoRedirect(t *testing.T) {
	var puts atomic.Int32
	consul := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/catalog/services":
			_, _ = w.Write([]byte(`{}`)) // "payments" disappeared
		case r.URL.Path == "/v1/catalog/datacenters":
			_, _ = w.Write([]byte(`["dc1"]`))
		case r.URL.Path == "/v1/config/service-resolver":
			_, _ = w.Write([]byte(`[{"Kind":"service-resolver","Name":"payments","Meta":{"created-by":"atc"}}]`))
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
	client, err := api.NewClient(&api.Config{Address: consul.Listener.Addr().String()})
	require.NoError(t, err)

	require.NoError(t, r.reconcile(context.Background(), client))
	assert.Zero(t, puts.Load(), "must not redirect to a datacenter that does not exist")
}
