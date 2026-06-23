package atc

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
)

func TestApplyOverride_Expiration(t *testing.T) {
	var writtenEntry *api.ServiceResolverConfigEntry

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && r.URL.Path == "/v1/config" {
			var entry api.ServiceResolverConfigEntry
			if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writtenEntry = &entry
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	atc := &Atc{
		Cfg: Config{
			ConsulAddr: server.Listener.Addr().String(),
		},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		coreLogger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// 1. Test ApplyFailoverOverride with duration "5m"
	err := atc.ApplyFailoverOverride(context.Background(), "payment-service", "dc2", "custom-ns", "5m")
	assert.NoError(t, err)
	assert.NotNil(t, writtenEntry)
	assert.Equal(t, "payment-service", writtenEntry.Name)
	expiresAtStr := writtenEntry.Meta["atc-override-expires-at"]
	assert.NotEmpty(t, expiresAtStr)

	expiresAt, err := time.Parse(time.RFC3339, expiresAtStr)
	assert.NoError(t, err)
	assert.True(t, expiresAt.After(time.Now()))
	assert.True(t, expiresAt.Before(time.Now().Add(6*time.Minute)))

	// 2. Test TriggerManualRedirect with duration "10m"
	writtenEntry = nil
	err = atc.TriggerManualRedirect(context.Background(), "billing-service", "dc3", "", "10m")
	assert.NoError(t, err)
	assert.NotNil(t, writtenEntry)
	assert.Equal(t, "billing-service", writtenEntry.Name)
	expiresAtStr = writtenEntry.Meta["atc-override-expires-at"]
	assert.NotEmpty(t, expiresAtStr)

	expiresAt, err = time.Parse(time.RFC3339, expiresAtStr)
	assert.NoError(t, err)
	assert.True(t, expiresAt.After(time.Now().Add(9*time.Minute)))
	assert.True(t, expiresAt.Before(time.Now().Add(11*time.Minute)))
}

func TestListActiveOverrides(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/config/service-resolver" {
			entries := []api.ConfigEntry{
				&api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: "svc-failover",
					Meta: map[string]string{
						"created-by":              "atc-override",
						"atc-override-expires-at": "2026-06-20T12:00:00Z",
					},
					Failover: map[string]api.ServiceResolverFailover{
						"*": {
							Targets: []api.ServiceResolverFailoverTarget{
								{Datacenter: "dc2"},
							},
						},
					},
				},
				&api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: "svc-redirect",
					Meta: map[string]string{
						"created-by": "atc-override",
					},
					Redirect: &api.ServiceResolverRedirect{
						Service:    "svc-redirect",
						Datacenter: "dc3",
					},
				},
				&api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: "svc-ignored",
					Meta: map[string]string{
						"created-by": "atc",
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(entries)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	atc := &Atc{
		Cfg: Config{
			ConsulAddr: server.Listener.Addr().String(),
		},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		coreLogger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	overrides, err := atc.ListActiveOverrides(context.Background())
	assert.NoError(t, err)
	assert.Len(t, overrides, 2)

	var fo, rd map[string]any
	for _, o := range overrides {
		switch o["service"] {
		case "svc-failover":
			fo = o
		case "svc-redirect":
			rd = o
		}
	}

	assert.NotNil(t, fo)
	assert.Equal(t, "failover", fo["type"])
	assert.Equal(t, "dc2", fo["target_dc"])
	assert.Equal(t, "2026-06-20T12:00:00Z", fo["expires_at"])

	assert.NotNil(t, rd)
	assert.Equal(t, "redirect", rd["type"])
	assert.Equal(t, "dc3", rd["target_dc"])
	assert.Equal(t, "never", rd["expires_at"])
}
