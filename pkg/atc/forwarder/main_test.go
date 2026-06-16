package forwarder

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
)

func TestForwarderReconcile(t *testing.T) {
	var mu sync.Mutex
	createdOrUpdated := make([]*api.ServiceResolverConfigEntry, 0)
	deleted := make([]string, 0)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if r.Method == "GET" && r.URL.Path == "/v1/agent/self" {
			info := map[string]interface{}{
				"Config": map[string]interface{}{
					"Datacenter": "dc1",
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(info)
			return
		}

		if r.Method == "GET" && r.URL.Path == "/v1/catalog/datacenters" {
			dcs := []string{"dc1", "dc2"}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(dcs)
			return
		}

		if r.Method == "GET" && r.URL.Path == "/v1/catalog/services" {
			services := map[string][]string{
				"consul":    {},
				"service-a": {"atc.enabled=true"},
				"service-b": {"atc.enabled=true"},
				"service-c": {"atc.enabled=true"},
				"service-d": {"some-other-tag"},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(services)
			return
		}

		if r.Method == "GET" && r.URL.Path == "/v1/config/service-resolver" {
			// List API
			entries := []api.ConfigEntry{
				&api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: "service-a",
					Meta: map[string]string{"created-by": "atc"},
					ConnectTimeout: 15 * time.Second,
					Failover: map[string]api.ServiceResolverFailover{
						"*": {
							Targets: []api.ServiceResolverFailoverTarget{
								{
									Service:    "service-a",
									Datacenter: "dc2",
								},
							},
						},
					},
				},
				&api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: "service-b",
					Meta: map[string]string{"created-by": "atc"},
					// Failover block is missing
				},
				&api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: "service-d",
					Meta: map[string]string{"created-by": "atc"},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(entries)
			return
		}

		if r.Method == "PUT" && r.URL.Path == "/v1/config" {
			var entry api.ServiceResolverConfigEntry
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &entry)
			createdOrUpdated = append(createdOrUpdated, &entry)
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/v1/config/service-resolver/") {
			name := strings.TrimPrefix(r.URL.Path, "/v1/config/service-resolver/")
			deleted = append(deleted, name)
			w.WriteHeader(http.StatusOK)
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.Listener.Addr().String()})
	if err != nil {
		t.Fatalf("Failed to create consul client: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f, err := New(logger, server.Listener.Addr().String(), "", "")
	if err != nil {
		t.Fatalf("Failed to create forwarder: %v", err)
	}

	err = f.reconcile(context.Background(), client)
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Assertions:
	// service-a: should not be created or updated (already has correct configuration).
	// service-b: updated to add Failover.
	// service-c: created with Failover.
	// service-d: deleted because tag was removed.

	if len(createdOrUpdated) != 2 {
		t.Errorf("Expected 2 config entries to be created or updated, got %d", len(createdOrUpdated))
	} else {
		// Sort by name
		slices.SortFunc(createdOrUpdated, func(a, b *api.ServiceResolverConfigEntry) int {
			return strings.Compare(a.Name, b.Name)
		})

		if createdOrUpdated[0].Name != "service-b" {
			t.Errorf("Expected first updated entry to be 'service-b', got '%s'", createdOrUpdated[0].Name)
		}
		if createdOrUpdated[1].Name != "service-c" {
			t.Errorf("Expected second updated entry to be 'service-c', got '%s'", createdOrUpdated[1].Name)
		}

		for _, entry := range createdOrUpdated {
			fo, ok := entry.Failover["*"]
			if !ok {
				t.Errorf("Expected entry %s to have Failover config for '*'", entry.Name)
			} else if len(fo.Targets) != 1 || fo.Targets[0].Datacenter != "dc2" || fo.Targets[0].Service != entry.Name {
				t.Errorf("Expected entry %s to have Targets point to dc2 and name, got %v", entry.Name, fo.Targets)
			}
			if entry.ConnectTimeout != 15*time.Second {
				t.Errorf("Expected ConnectTimeout to be 15s, got %v", entry.ConnectTimeout)
			}
		}
	}

	if len(deleted) != 1 {
		t.Errorf("Expected 1 config entry to be deleted, got %d", len(deleted))
	} else if deleted[0] != "service-d" {
		t.Errorf("Expected deleted entry to be 'service-d', got '%s'", deleted[0])
	}
}
