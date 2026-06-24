package atc

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	atc_server "github.com/atcprojectio/atc/pkg/atc/server"
)

func TestMcpEnabled(t *testing.T) {
	cfg := Config{
		Target: []string{Server},
		Server: atc_server.Config{
			McpEnabled: true,
		},
	}

	atcInstance, err := New(cfg)
	if err != nil {
		t.Fatalf("failed to create atc: %v", err)
	}

	req := httptest.NewRequest("GET", "/mcp", nil)
	w := httptest.NewRecorder()

	atcInstance.Server.Mux.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		t.Errorf("Expected status other than 404, got %d", resp.StatusCode)
	}
}

func TestMcpDisabled(t *testing.T) {
	cfg := Config{
		Target: []string{Server},
		Server: atc_server.Config{
			McpEnabled: false,
		},
	}

	atcInstance, err := New(cfg)
	if err != nil {
		t.Fatalf("failed to create atc: %v", err)
	}

	req := httptest.NewRequest("GET", "/mcp", nil)
	w := httptest.NewRecorder()

	atcInstance.Server.Mux.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected status 404 Not Found, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "MCP server is disabled") {
		t.Errorf("Expected body containing 'MCP server is disabled', got '%s'", string(body))
	}
}
