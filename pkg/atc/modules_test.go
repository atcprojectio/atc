package atc

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/consul/api"
	"github.com/spf13/viper"
)

func TestResolveModules(t *testing.T) {
	tests := []struct {
		name     string
		targets  []string
		expected []string
	}{
		{
			name:     "Target Server",
			targets:  []string{Server},
			expected: []string{Server},
		},
		{
			name:     "Target Forwarder",
			targets:  []string{Forwarder},
			expected: []string{Forwarder, Server},
		},
		{
			name:     "Target Redirector",
			targets:  []string{Redirector},
			expected: []string{Redirector, Server},
		},
		{
			name:     "Target Consul",
			targets:  []string{Consul},
			expected: []string{Consul, Forwarder, Redirector, Server},
		},
		{
			name:     "Target All",
			targets:  []string{All},
			expected: []string{All, Consul, Forwarder, Redirector, Server},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved := resolveModules(tt.targets)
			if len(resolved) != len(tt.expected) {
				t.Errorf("expected %d resolved modules, got %d (resolved: %v)", len(tt.expected), len(resolved), resolved)
			}
			for _, exp := range tt.expected {
				if !resolved[exp] {
					t.Errorf("expected resolved modules to contain %s, but it was not present", exp)
				}
			}
		})
	}
}

func TestDependenciesForModule(t *testing.T) {
	atcInstance := &Atc{}
	deps := atcInstance.DependenciesForModule(Consul)
	expectedDeps := []string{Forwarder, Redirector, Server}
	slices.Sort(deps)
	slices.Sort(expectedDeps)

	if !slices.Equal(deps, expectedDeps) {
		t.Errorf("expected dependencies for Consul to be %v, got %v", expectedDeps, deps)
	}
}

func TestUserVisibleModuleNames(t *testing.T) {
	atcInstance := &Atc{}
	visible := atcInstance.UserVisibleModuleNames()

	if len(visible) != len(UserVisibleModules) {
		t.Errorf("expected %d user visible modules, got %d", len(UserVisibleModules), len(visible))
	}

	// Ensure Server is not in the user visible list
	if slices.Contains(visible, Server) {
		t.Errorf("user visible modules should not contain internal module Server: %v", visible)
	}
}

func TestPurgeServiceResolver(t *testing.T) {
	var deleted string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/config/service-resolver/test-service" {
			entry := &api.ServiceResolverConfigEntry{
				Kind: "service-resolver",
				Name: "test-service",
				Meta: map[string]string{"created-by": "atc"},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(entry)
			return
		}
		if r.Method == "DELETE" && r.URL.Path == "/v1/config/service-resolver/test-service" {
			deleted = "test-service"
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
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// Test direct method
	err := atc.PurgeServiceResolver(context.Background(), "test-service")
	if err != nil {
		t.Fatalf("PurgeServiceResolver failed: %v", err)
	}
	if deleted != "test-service" {
		t.Errorf("Expected config entry to be deleted")
	}

	// Test endpoint handler
	deleted = ""
	req, err := http.NewRequest("DELETE", "/api/services?name=test-service", nil)
	if err != nil {
		t.Fatalf("Failed to create request: %v", err)
	}
	rr := httptest.NewRecorder()
	atc.apiServicesDeleteHandler(rr, req)

	if rr.Code != http.StatusNoContent {
		t.Errorf("Expected status code 204, got %d", rr.Code)
	}
	if deleted != "test-service" {
		t.Errorf("Expected endpoint handler to delete config entry")
	}
}

func TestApiServicesHandler(t *testing.T) {
	// Setup a mock Consul server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/catalog/services" {
			services := map[string][]string{
				"consul":         {},
				"service-active": {"atc.enabled=true"},
				"service-new":    {"atc.enabled=true"},
				"service-other":  {"some-tag"},
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
					Name: "service-active",
					Meta: map[string]string{"created-by": "atc"},
					Failover: map[string]api.ServiceResolverFailover{
						"*": {
							Datacenters: []string{"dc2"},
						},
					},
				},
				&api.ServiceResolverConfigEntry{
					Kind: "service-resolver",
					Name: "service-deleted",
					Meta: map[string]string{"created-by": "atc"},
					Redirect: &api.ServiceResolverRedirect{
						Service:    "service-deleted",
						Datacenter: "dc2",
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

	// 1. Test when both Forwarder and Redirector are enabled
	atcInstance := &Atc{
		Cfg: Config{
			ConsulAddr: server.Listener.Addr().String(),
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		enabledModules: map[string]bool{
			Forwarder:  true,
			Redirector: true,
		},
	}

	req, err := http.NewRequest("GET", "/api/services", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	rr := httptest.NewRecorder()
	atcInstance.apiServicesHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status OK, got %d", rr.Code)
	}

	var results []struct {
		Name         string   `json:"name"`
		Tags         []string `json:"tags"`
		ResolverType string   `json:"resolver_type"`
		Status       string   `json:"status"`
	}
	err = json.Unmarshal(rr.Body.Bytes(), &results)
	if err != nil {
		t.Fatalf("failed to unmarshal results: %v", err)
	}

	// Expect service-active, service-new, and service-deleted (since it is deleted but has a redirect config entry from us)
	expectedServices := map[string]struct {
		resolverType string
		status       string
	}{
		"service-active":  {"failover", "active"},
		"service-new":     {"failover", "active"}, // fallback when both modules enabled and config entry does not exist yet
		"service-deleted": {"redirect", "deleted"},
	}

	if len(results) != len(expectedServices) {
		t.Errorf("expected %d services, got %d", len(expectedServices), len(results))
	}

	for _, res := range results {
		exp, ok := expectedServices[res.Name]
		if !ok {
			t.Errorf("unexpected service in response: %s", res.Name)
			continue
		}
		if res.ResolverType != exp.resolverType {
			t.Errorf("for service %s: expected resolver type %q, got %q", res.Name, exp.resolverType, res.ResolverType)
		}
		if res.Status != exp.status {
			t.Errorf("for service %s: expected status %q, got %q", res.Name, exp.status, res.Status)
		}
	}

	// 2. Test when only Redirector is enabled
	atcInstanceOnlyRedirector := &Atc{
		Cfg: Config{
			ConsulAddr: server.Listener.Addr().String(),
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		enabledModules: map[string]bool{
			Forwarder:  false,
			Redirector: true,
		},
	}

	req2, err := http.NewRequest("GET", "/api/services", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	rr2 := httptest.NewRecorder()
	atcInstanceOnlyRedirector.apiServicesHandler(rr2, req2)

	var results2 []struct {
		Name         string   `json:"name"`
		Tags         []string `json:"tags"`
		ResolverType string   `json:"resolver_type"`
		Status       string   `json:"status"`
	}
	err = json.Unmarshal(rr2.Body.Bytes(), &results2)
	if err != nil {
		t.Fatalf("failed to unmarshal results: %v", err)
	}

	for _, res := range results2 {
		if res.Name == "service-new" {
			if res.ResolverType != "none" {
				t.Errorf("expected resolver_type = 'none' for service-new when only Redirector is enabled, got %q", res.ResolverType)
			}
		}
	}
}

func TestGetFederationStatusMock(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/agent/members" && r.URL.Query().Get("wan") == "1" {
			members := []map[string]interface{}{
				{
					"Name":   "consul-dc1",
					"Status": 1,
					"Tags":   map[string]string{"dc": "dc1"},
				},
				{
					"Name":   "consul-dc2",
					"Status": 4,
					"Tags":   map[string]string{"dc": "dc2"},
				},
				{
					"Name":   "consul-dc3",
					"Status": 1,
					"Tags":   map[string]string{"dc": "dc3"},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(members)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	atcInstance := &Atc{
		Cfg: Config{
			ConsulAddr: server.Listener.Addr().String(),
		},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	statusMap, err := atcInstance.GetFederationStatus(context.Background())
	if err != nil {
		t.Fatalf("GetFederationStatus failed: %v", err)
	}

	if statusMap["dc1"] != "alive" {
		t.Errorf("expected dc1 to be alive, got %q", statusMap["dc1"])
	}
	if statusMap["dc2"] != "failed" {
		t.Errorf("expected dc2 to be failed, got %q", statusMap["dc2"])
	}
	if statusMap["dc3"] != "alive" {
		t.Errorf("expected dc3 to be alive, got %q", statusMap["dc3"])
	}
}

func TestApplyFailoverOverride(t *testing.T) {
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

	err := atc.ApplyFailoverOverride(context.Background(), "payment-service", "dc2", "", "")
	if err != nil {
		t.Fatalf("ApplyFailoverOverride failed: %v", err)
	}

	if writtenEntry == nil {
		t.Fatalf("expected entry to be written")
	}
	if writtenEntry.Name != "payment-service" {
		t.Errorf("expected service name 'payment-service', got %q", writtenEntry.Name)
	}
	if writtenEntry.Meta["created-by"] != "atc-override" {
		t.Errorf("expected created-by to be 'atc-override', got %q", writtenEntry.Meta["created-by"])
	}
	if writtenEntry.Failover == nil {
		t.Fatalf("expected Failover configuration")
	}
	fo, ok := writtenEntry.Failover["*"]
	if !ok || len(fo.Targets) != 1 || fo.Targets[0].Datacenter != "dc2" {
		t.Errorf("expected target datacenter to be 'dc2'")
	}
}

func TestTriggerManualRedirect(t *testing.T) {
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

	err := atc.TriggerManualRedirect(context.Background(), "payment-service", "dc2", "", "")
	if err != nil {
		t.Fatalf("TriggerManualRedirect failed: %v", err)
	}

	if writtenEntry == nil {
		t.Fatalf("expected entry to be written")
	}
	if writtenEntry.Name != "payment-service" {
		t.Errorf("expected service name 'payment-service', got %q", writtenEntry.Name)
	}
	if writtenEntry.Meta["created-by"] != "atc-override" {
		t.Errorf("expected created-by to be 'atc-override', got %q", writtenEntry.Meta["created-by"])
	}
	if writtenEntry.Redirect == nil || writtenEntry.Redirect.Datacenter != "dc2" {
		t.Errorf("expected redirect datacenter to be 'dc2'")
	}
}

func TestApiOverridesHandler(t *testing.T) {
	var method string
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		path = r.URL.Path
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	atc := &Atc{
		Cfg: Config{
			ConsulAddr: server.Listener.Addr().String(),
		},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		coreLogger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// 1. Failover Override Request
	payload := `{"service":"payment-service","type":"failover","target_dc":"dc2"}`
	req := httptest.NewRequest("POST", "/api/overrides", strings.NewReader(payload))
	rr := httptest.NewRecorder()
	atc.apiOverridesHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("expected code 200, got %d", rr.Code)
	}
	if method != "PUT" || path != "/v1/config" {
		t.Errorf("expected PUT /v1/config request to Consul, got %s %s", method, path)
	}

	// 2. Redirect Override Request
	payload2 := `{"service":"payment-service","type":"redirect","target_dc":"dc3"}`
	req2 := httptest.NewRequest("POST", "/api/overrides", strings.NewReader(payload2))
	rr2 := httptest.NewRecorder()
	atc.apiOverridesHandler(rr2, req2)

	if rr2.Code != http.StatusOK {
		t.Errorf("expected code 200, got %d", rr2.Code)
	}
	if method != "PUT" || path != "/v1/config" {
		t.Errorf("expected PUT /v1/config request to Consul, got %s %s", method, path)
	}
}

func TestApiModulesHandler(t *testing.T) {
	atcInstance := &Atc{
		enabledModules: map[string]bool{
			Forwarder:  true,
			Redirector: true,
		},
	}

	req, err := http.NewRequest("GET", "/api/modules", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	rr := httptest.NewRecorder()
	atcInstance.apiModulesHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected status OK, got %d", rr.Code)
	}

	var results []string
	err = json.Unmarshal(rr.Body.Bytes(), &results)
	if err != nil {
		t.Fatalf("failed to unmarshal results: %v", err)
	}

	if len(results) != 2 || results[0] != "forwarder" || results[1] != "redirector" {
		t.Errorf("unexpected results: %v", results)
	}
}

func TestApiReloadHandler(t *testing.T) {
	viper.Set("config", "")

	atcInstance := &Atc{
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		coreLogger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	// 1. GET request (should be method not allowed)
	reqGet, err := http.NewRequest("GET", "/api/reload", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	rrGet := httptest.NewRecorder()
	atcInstance.apiReloadHandler(rrGet, reqGet)
	if rrGet.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected status MethodNotAllowed, got %d", rrGet.Code)
	}

	// 2. POST request (should trigger reload)
	reqPost, err := http.NewRequest("POST", "/api/reload", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	rrPost := httptest.NewRecorder()
	atcInstance.apiReloadHandler(rrPost, reqPost)

	if rrPost.Code != http.StatusOK {
		t.Errorf("expected status OK, got %d. Body: %s", rrPost.Code, rrPost.Body.String())
	}
}

