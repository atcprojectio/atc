package atc

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOverrideRefusesForeignResolverUnlessForced(t *testing.T) {
	var existing atomic.Value
	var puts atomic.Int32
	consul := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			body := existing.Load().(string)
			if body == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		case http.MethodPut:
			puts.Add(1)
			_, _ = w.Write([]byte(`true`))
		}
	}))
	defer consul.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &Atc{Cfg: Config{ConsulAddr: consul.Listener.Addr().String()}, logger: logger, coreLogger: logger, consulClients: map[string]*api.Client{}}
	post := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		a.apiOverridesHandler(rr, httptest.NewRequest(http.MethodPost, "/api/overrides", strings.NewReader(body)))
		return rr
	}

	existing.Store(`{"Kind":"service-resolver","Name":"payments","DefaultSubset":"v2"}`)
	rr := post(`{"service":"payments","type":"failover","target_dc":"dc2"}`)
	assert.Equal(t, http.StatusConflict, rr.Code)
	assert.Contains(t, rr.Body.String(), "did not create")
	assert.Zero(t, puts.Load())

	rr = post(`{"service":"payments","type":"redirect","target_dc":"dc2","force":true}`)
	assert.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, int32(1), puts.Load())

	existing.Store(`{"Kind":"service-resolver","Name":"payments","Meta":{"created-by":"atc"}}`)
	require.NoError(t, a.ApplyFailoverOverride(context.Background(), "payments", "dc2", "", "", false), "ATC-owned resolvers can be overridden")

	existing.Store("")
	require.NoError(t, a.TriggerManualRedirect(context.Background(), "payments", "dc2", "", "", false), "no resolver yet")
}

func TestManagedByOf(t *testing.T) {
	foreign := &api.ServiceResolverConfigEntry{Name: "x"}
	assert.Equal(t, "foreign", managedByOf(foreign, []string{"atc.enabled=true"}))
	assert.Equal(t, "adopting", managedByOf(foreign, []string{"atc.enabled=true", "atc.adopt=true"}))
	assert.Equal(t, "atc", managedByOf(&api.ServiceResolverConfigEntry{Meta: map[string]string{"created-by": "atc"}}, nil))
	assert.Equal(t, "override", managedByOf(&api.ServiceResolverConfigEntry{Meta: map[string]string{"created-by": "atc-override"}}, nil))
}
