package forwarder

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
					Kind:           "service-resolver",
					Name:           "service-a",
					Meta:           map[string]string{"created-by": "atc"},
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
	f, err := New(logger, server.Listener.Addr().String(), "", "", "", "", nil, "", "", false)
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

func TestForwarderReconcile_WithStrategy(t *testing.T) {
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
			services := map[string][]string{
				"service-a": {"atc.enabled=true", "atc.failover=custom-strategy"},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(services)
			return
		}

		if r.Method == "GET" && r.URL.Path == "/v1/config/service-resolver" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]api.ConfigEntry{})
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

	strategies := map[string]FailoverStrategy{
		"custom-strategy": {
			ConnectTimeout: "12s",
			Targets: []FailoverTarget{
				{
					Service:    "fallback-svc",
					Datacenter: "dc3",
				},
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f, err := New(logger, server.Listener.Addr().String(), "", "", "", "", strategies, "", "", false)
	if err != nil {
		t.Fatalf("Failed to create forwarder: %v", err)
	}

	err = f.reconcile(context.Background(), client)
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(createdOrUpdated) != 1 {
		t.Fatalf("Expected 1 config entry to be updated, got %d", len(createdOrUpdated))
	}

	entry := createdOrUpdated[0]
	if entry.Name != "service-a" {
		t.Errorf("Expected entry name 'service-a', got '%s'", entry.Name)
	}

	if entry.ConnectTimeout != 12*time.Second {
		t.Errorf("Expected ConnectTimeout 12s, got %v", entry.ConnectTimeout)
	}

	if entry.Meta["failover-strategy"] != "custom-strategy" {
		t.Errorf("Expected failover-strategy metadata 'custom-strategy', got '%s'", entry.Meta["failover-strategy"])
	}

	if entry.Meta["redirect-strategy"] != "" {
		t.Errorf("Expected redirect-strategy metadata to be empty, got '%s'", entry.Meta["redirect-strategy"])
	}

	fo, ok := entry.Failover["*"]
	if !ok {
		t.Fatalf("Expected Failover map for '*' to be present")
	}

	if len(fo.Targets) != 1 {
		t.Fatalf("Expected 1 target, got %d", len(fo.Targets))
	}

	if fo.Targets[0].Service != "fallback-svc" || fo.Targets[0].Datacenter != "dc3" {
		t.Errorf("Expected target fallback-svc in dc3, got %s in %s", fo.Targets[0].Service, fo.Targets[0].Datacenter)
	}
}

func TestForwarderUpdateConfigRace(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f, err := New(logger, "", "", "", "", "", nil, "5s", "0s", false)
	if err != nil {
		t.Fatalf("Failed to create forwarder: %v", err)
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
					f.mu.Lock()
					_ = f.failoverStrategies
					_ = f.dampeningPeriod
					_ = f.minDampeningPeriod
					f.mu.Unlock()
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			f.UpdateConfig(
				map[string]FailoverStrategy{
					"test": {ConnectTimeout: "10s"},
				},
				"10s",
				"1s",
				"",
				"",
				false,
			)
			time.Sleep(1 * time.Microsecond)
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(done)
	wg.Wait()
}

func TestForwarderReconcile_WithDefaultStrategy(t *testing.T) {
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
			services := map[string][]string{
				"service-a": {"atc.enabled=true"},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(services)
			return
		}

		if r.Method == "GET" && r.URL.Path == "/v1/config/service-resolver" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]api.ConfigEntry{})
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

	strategies := map[string]FailoverStrategy{
		"default": {
			ConnectTimeout: "15s",
			Targets: []FailoverTarget{
				{
					Service:    "default-fallback-svc",
					Datacenter: "dc2",
				},
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f, err := New(logger, server.Listener.Addr().String(), "", "", "", "", strategies, "", "", false)
	if err != nil {
		t.Fatalf("Failed to create forwarder: %v", err)
	}

	err = f.reconcile(context.Background(), client)
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(createdOrUpdated) != 1 {
		t.Fatalf("Expected 1 config entry to be updated, got %d", len(createdOrUpdated))
	}

	entry := createdOrUpdated[0]
	if entry.Meta["failover-strategy"] != "default" {
		t.Errorf("Expected failover-strategy metadata 'default', got '%s'", entry.Meta["failover-strategy"])
	}
}

func TestForwarderReconcile_BypassOverrides(t *testing.T) {
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
				"service-a": {"atc.enabled=true"},
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
	f, err := New(logger, server.Listener.Addr().String(), "", "", "", "", nil, "", "", false)
	if err != nil {
		t.Fatalf("Failed to create forwarder: %v", err)
	}

	err = f.reconcile(context.Background(), client)
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

func TestGetDampeningDuration(t *testing.T) {
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

func TestForwarder_UpdateConfig_Propagation(t *testing.T) {
	var mu sync.Mutex
	var lastPutEntry *api.ServiceResolverConfigEntry

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if r.Method == "GET" && r.URL.Path == "/v1/agent/self" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"Config":{"Datacenter":"dc1"}}`))
			return
		}
		if r.Method == "GET" && r.URL.Path == "/v1/catalog/datacenters" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`["dc1","dc2","dc3"]`))
			return
		}
		if r.Method == "GET" && r.URL.Path == "/v1/catalog/services" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"service-a":["atc.enabled=true","atc.failover=test-strategy"]}`))
			return
		}
		if r.Method == "GET" && r.URL.Path == "/v1/config/service-resolver" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
			return
		}
		if r.Method == "PUT" && r.URL.Path == "/v1/config" {
			var entry api.ServiceResolverConfigEntry
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &entry)
			lastPutEntry = &entry
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
	f, err := New(logger, server.Listener.Addr().String(), "", "", "", "", nil, "", "", false)
	if err != nil {
		t.Fatalf("Failed to create forwarder: %v", err)
	}

	_ = f.reconcile(context.Background(), client)

	mu.Lock()
	entryBefore := lastPutEntry
	lastPutEntry = nil
	mu.Unlock()

	if entryBefore == nil {
		t.Fatal("Expected config entry to be written on first reconcile (with default target fallback)")
	}
	foBefore, ok := entryBefore.Failover["*"]
	if !ok || len(foBefore.Targets) != 1 || foBefore.Targets[0].Service != "service-a" || foBefore.Targets[0].Datacenter != "dc2" {
		t.Errorf("Expected fallback target to be default service-a in dc2, got: %v", foBefore)
	}

	f.UpdateConfig(
		map[string]FailoverStrategy{
			"test-strategy": {
				ConnectTimeout: "10s",
				Targets: []FailoverTarget{
					{Service: "fallback-a", Datacenter: "dc2"},
				},
			},
		},
		"0s",
		"0s",
		"",
		"",
		false,
	)

	// Since f.reconcile checks if existing config matches, it will detect that the failover targets
	// have changed from default to fallback-a and will issue an update.
	err = f.reconcile(context.Background(), client)
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	mu.Lock()
	entryAfter := lastPutEntry
	mu.Unlock()

	if entryAfter == nil {
		t.Fatal("Expected config entry to be written after reloading strategies")
	}
	if entryAfter.Meta["failover-strategy"] != "test-strategy" {
		t.Errorf("expected failover-strategy to be 'test-strategy', got %q", entryAfter.Meta["failover-strategy"])
	}
	foAfter, ok := entryAfter.Failover["*"]
	if !ok || len(foAfter.Targets) != 1 || foAfter.Targets[0].Service != "fallback-a" || foAfter.Targets[0].Datacenter != "dc2" {
		t.Errorf("expected failover target to be fallback-a in dc2, got: %v", foAfter)
	}
}

func TestForwarderDebouncerCoalesce(t *testing.T) {
	var servicesQueryCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			atomic.AddInt32(&servicesQueryCount, 1)
			indexStr := r.URL.Query().Get("index")
			index, _ := strconv.Atoi(indexStr)
			w.Header().Set("X-Consul-Index", "1")
			if index > 0 {
				time.Sleep(200 * time.Millisecond)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string][]string{})
			return
		}

		if r.URL.Path == "/v1/health/state/any" {
			indexStr := r.URL.Query().Get("index")
			index, _ := strconv.Atoi(indexStr)
			w.Header().Set("X-Consul-Index", "1")
			if index > 0 {
				time.Sleep(200 * time.Millisecond)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}

		if r.Method == "GET" && r.URL.Path == "/v1/config/service-resolver" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]api.ConfigEntry{})
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f, err := New(logger, server.Listener.Addr().String(), "", "", "", "50ms", nil, "", "", false)
	if err != nil {
		t.Fatalf("Failed to create forwarder: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = f.Run(ctx)
	}()

	// Wait for initial reconcile to finish (which increments count to 1)
	time.Sleep(20 * time.Millisecond)

	// Broadcast 5 events in rapid succession (every 2ms)
	for i := 0; i < 5; i++ {
		f.watcher.Events.Broadcast("test_update")
		time.Sleep(2 * time.Millisecond)
	}

	// Wait for the debouncer window to elapse and run the reconcile
	time.Sleep(120 * time.Millisecond)

	count := atomic.LoadInt32(&servicesQueryCount)
	if count > 4 {
		t.Errorf("Expected at most 4 service catalog queries (including watcher blocking queries), got %d (debouncing failed)", count)
	}
}

func TestForwarderReconcile_DryRun(t *testing.T) {
	var mu sync.Mutex
	var writeAttempted bool

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
				"service-a": {"atc.enabled=true"},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(services)
			return
		}

		if r.Method == "GET" && r.URL.Path == "/v1/config/service-resolver" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]api.ConfigEntry{})
			return
		}

		if r.Method == "PUT" || r.Method == "DELETE" {
			writeAttempted = true
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
	f, err := New(logger, server.Listener.Addr().String(), "", "", "", "", nil, "", "", true)
	if err != nil {
		t.Fatalf("Failed to create forwarder: %v", err)
	}

	err = f.reconcile(context.Background(), client)
	if err != nil {
		t.Fatalf("reconcile failed: %v", err)
	}

	mu.Lock()
	attempted := writeAttempted
	mu.Unlock()

	if attempted {
		t.Errorf("Expected dryRun = true to bypass all writes to Consul, but write/delete was attempted")
	}
}

