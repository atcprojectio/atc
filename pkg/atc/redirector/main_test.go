package redirector

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

func TestRedirectorReconcile(t *testing.T) {
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
				// service-e and service-f are completely deleted/absent from catalog
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
					Redirect: &api.ServiceResolverRedirect{
						Service:    "service-a",
						Datacenter: "dc2",
					},
				},
				&api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: "service-b",
					Meta: map[string]string{"created-by": "atc"},
				},
				&api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: "service-d",
					Meta: map[string]string{"created-by": "atc"},
				},
				&api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: "service-e",
					Meta: map[string]string{"created-by": "atc"},
				},
				&api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: "service-f",
					Meta: map[string]string{"created-by": "atc"},
					Redirect: &api.ServiceResolverRedirect{
						Service:    "service-f",
						Datacenter: "dc2",
					},
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
	r, err := New(logger, server.Listener.Addr().String(), "", "", false, nil, "", "", false)
	if err != nil {
		t.Fatalf("Failed to create redirector: %v", err)
	}

	err = r.reconcile(context.Background(), client)
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// Assertions:
	// service-a: updated to remove Redirect block (since it is active locally).
	// service-b: no update or delete (active and already clean base).
	// service-c: created as base entry (active locally).
	// service-d: deleted (tag was removed).
	// service-e: updated to add Redirect (completely deleted from catalog).
	// service-f: no update or delete (deleted from catalog and already Redirect).

	if len(createdOrUpdated) != 3 {
		t.Errorf("Expected 3 config entries to be created or updated, got %d", len(createdOrUpdated))
	} else {
		// Sort by name
		slices.SortFunc(createdOrUpdated, func(a, b *api.ServiceResolverConfigEntry) int {
			return strings.Compare(a.Name, b.Name)
		})

		// service-a must be base entry (no redirect)
		if createdOrUpdated[0].Name != "service-a" {
			t.Errorf("Expected first updated entry to be 'service-a', got '%s'", createdOrUpdated[0].Name)
		} else if createdOrUpdated[0].Redirect != nil && createdOrUpdated[0].Redirect.Service != "" {
			t.Errorf("Expected service-a to have no Redirect block")
		}

		// service-c must be base entry (no redirect)
		if createdOrUpdated[1].Name != "service-c" {
			t.Errorf("Expected second updated entry to be 'service-c', got '%s'", createdOrUpdated[1].Name)
		} else if createdOrUpdated[1].Redirect != nil && createdOrUpdated[1].Redirect.Service != "" {
			t.Errorf("Expected service-c to have no Redirect block")
		}

		// service-e must have Redirect pointing to dc2
		if createdOrUpdated[2].Name != "service-e" {
			t.Errorf("Expected third updated entry to be 'service-e', got '%s'", createdOrUpdated[2].Name)
		}
		if createdOrUpdated[2].Redirect == nil {
			t.Errorf("Expected service-e to have Redirect block")
		} else {
			if createdOrUpdated[2].Redirect.Service != "service-e" {
				t.Errorf("Expected redirect service to be 'service-e', got '%s'", createdOrUpdated[2].Redirect.Service)
			}
			if createdOrUpdated[2].Redirect.Datacenter != "dc2" {
				t.Errorf("Expected redirect datacenter to be 'dc2', got '%s'", createdOrUpdated[2].Redirect.Datacenter)
			}
		}
	}

	if len(deleted) != 1 {
		t.Errorf("Expected 1 config entry to be deleted, got %d", len(deleted))
	} else if deleted[0] != "service-d" {
		t.Errorf("Expected deleted entry to be 'service-d', got '%s'", deleted[0])
	}
}

func TestRedirectorReconcile_ForwarderEnabled(t *testing.T) {
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
			entries := []api.ConfigEntry{
				&api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: "service-a",
					Meta: map[string]string{"created-by": "atc"},
					Redirect: &api.ServiceResolverRedirect{
						Service:    "service-a",
						Datacenter: "dc2",
					},
				},
				&api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: "service-b",
					Meta: map[string]string{"created-by": "atc"},
				},
				&api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: "service-d",
					Meta: map[string]string{"created-by": "atc"},
				},
				&api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: "service-e",
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
	r, err := New(logger, server.Listener.Addr().String(), "", "", true, nil, "", "", false) // forwarderEnabled = true
	if err != nil {
		t.Fatalf("Failed to create redirector: %v", err)
	}

	err = r.reconcile(context.Background(), client)
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	// With forwarderEnabled = true:
	// - active services (service-a, service-b, service-c) are NOT modified by redirector
	// - deleted service (service-e) is updated to redirect
	// - untagged active service (service-d) resolver is deleted
	if len(createdOrUpdated) != 1 {
		t.Errorf("Expected only 1 config entry (service-e) to be created or updated, got %d", len(createdOrUpdated))
	} else if createdOrUpdated[0].Name != "service-e" {
		t.Errorf("Expected updated entry to be 'service-e', got '%s'", createdOrUpdated[0].Name)
	}

	if len(deleted) != 1 {
		t.Errorf("Expected 1 config entry to be deleted, got %d", len(deleted))
	} else if deleted[0] != "service-d" {
		t.Errorf("Expected deleted entry to be 'service-d', got '%s'", deleted[0])
	}
}

func TestRedirectorReconcile_WithStrategy(t *testing.T) {
	var mu sync.Mutex
	createdOrUpdated := make([]*api.ServiceResolverConfigEntry, 0)

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
			// service-e is deleted/absent from catalog
			services := map[string][]string{
				"consul": {},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(services)
			return
		}

		if r.Method == "GET" && r.URL.Path == "/v1/config/service-resolver" {
			// existing service-resolver with redirect-strategy in metadata
			entries := []api.ConfigEntry{
				&api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: "service-e",
					Meta: map[string]string{
						"created-by":        "atc",
						"redirect-strategy": "custom-redirect",
					},
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

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client, err := api.NewClient(&api.Config{Address: server.Listener.Addr().String()})
	if err != nil {
		t.Fatalf("Failed to create consul client: %v", err)
	}

	strategies := map[string]RedirectStrategy{
		"custom-redirect": {
			Service:       "redirected-svc",
			Datacenter:    "dc3",
			Namespace:     "custom-ns",
			ServiceSubset: "custom-sub",
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := New(logger, server.Listener.Addr().String(), "", "", false, strategies, "", "", false)
	if err != nil {
		t.Fatalf("Failed to create redirector: %v", err)
	}

	err = r.reconcile(context.Background(), client)
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(createdOrUpdated) != 1 {
		t.Fatalf("Expected 1 config entry to be updated, got %d", len(createdOrUpdated))
	}

	entry := createdOrUpdated[0]
	if entry.Name != "service-e" {
		t.Errorf("Expected entry name 'service-e', got '%s'", entry.Name)
	}

	if entry.Meta["redirect-strategy"] != "custom-redirect" {
		t.Errorf("Expected redirect-strategy metadata 'custom-redirect', got '%s'", entry.Meta["redirect-strategy"])
	}

	if entry.Meta["failover-strategy"] != "" {
		t.Errorf("Expected failover-strategy metadata to be empty, got '%s'", entry.Meta["failover-strategy"])
	}

	if entry.Redirect == nil {
		t.Fatalf("Expected Redirect block to be present")
	}

	if entry.Redirect.Service != "redirected-svc" || entry.Redirect.Datacenter != "dc3" ||
		entry.Redirect.Namespace != "custom-ns" || entry.Redirect.ServiceSubset != "custom-sub" {
		t.Errorf("Expected redirect to redirected-svc in dc3 (ns: custom-ns, subset: custom-sub), got service: %s, dc: %s, ns: %s, subset: %s",
			entry.Redirect.Service, entry.Redirect.Datacenter, entry.Redirect.Namespace, entry.Redirect.ServiceSubset)
	}
}

func TestRedirectorUpdateConfigRace(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	r, err := New(logger, "", "", "", false, nil, "5s", "0s", false)
	if err != nil {
		t.Fatalf("Failed to create redirector: %v", err)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					r.mu.Lock()
					_ = r.redirectStrategies
					_ = r.dampeningPeriod
					_ = r.minDampeningPeriod
					r.mu.Unlock()
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			r.UpdateConfig(
				map[string]RedirectStrategy{
					"test": {Datacenter: "dc2"},
				},
				"10s",
				"1s",
			)
			time.Sleep(1 * time.Microsecond)
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(done)
	wg.Wait()
}

func TestRedirectorReconcile_BypassOverrides(t *testing.T) {
	var mu sync.Mutex
	createdOrUpdated := false
	deleted := false

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
				"consul": {},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(services)
			return
		}

		if r.Method == "GET" && r.URL.Path == "/v1/config/service-resolver" {
			entries := []api.ConfigEntry{
				&api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: "service-a",
					Meta: map[string]string{"created-by": "atc-override"},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(entries)
			return
		}

		if r.Method == "PUT" && r.URL.Path == "/v1/config" {
			createdOrUpdated = true
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/v1/config/service-resolver/") {
			deleted = true
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
	r, err := New(logger, server.Listener.Addr().String(), "", "", false, nil, "", "", false)
	if err != nil {
		t.Fatalf("Failed to create redirector: %v", err)
	}

	err = r.reconcile(context.Background(), client)
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if createdOrUpdated {
		t.Error("Expected active override config entry to be bypassed (not created or updated)")
	}
	if deleted {
		t.Error("Expected active override config entry to be bypassed (not deleted)")
	}
}

func TestRedirector_GetDampeningDuration(t *testing.T) {
	tests := []struct {
		name          string
		tags          []string
		globalDefault string
		minLimit      string
		expected      time.Duration
	}{
		{
			name:          "Use Global Default when no tag",
			tags:          []string{"atc.enabled=true"},
			globalDefault: "5s",
			minLimit:      "1s",
			expected:      5 * time.Second,
		},
		{
			name:          "Use Tag override when present",
			tags:          []string{"atc.enabled=true", "atc.dampening=10s"},
			globalDefault: "5s",
			minLimit:      "1s",
			expected:      10 * time.Second,
		},
		{
			name:          "Clamp tag override to minimum limit",
			tags:          []string{"atc.enabled=true", "atc.dampening=500ms"},
			globalDefault: "5s",
			minLimit:      "1s",
			expected:      1 * time.Second,
		},
		{
			name:          "Zero duration dampening tag allowed when min limit is 0s",
			tags:          []string{"atc.enabled=true", "atc.dampening=0s"},
			globalDefault: "5s",
			minLimit:      "0s",
			expected:      0 * time.Second,
		},
		{
			name:          "Zero duration dampening tag clamped to min limit of 1s",
			tags:          []string{"atc.enabled=true", "atc.dampening=0s"},
			globalDefault: "5s",
			minLimit:      "1s",
			expected:      1 * time.Second,
		},
		{
			name:          "Fallback to global default on malformed tag",
			tags:          []string{"atc.enabled=true", "atc.dampening=invalid"},
			globalDefault: "5s",
			minLimit:      "1s",
			expected:      5 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := getDampeningDuration(tt.tags, tt.globalDefault, tt.minLimit)
			if res != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, res)
			}
		})
	}
}
