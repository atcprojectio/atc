package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServer_UIEnabled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := Config{
		HTTPListenPort:    8099,
		MetricsListenPort: 8100,
		UiEnabled:         true,
	}

	serv, err := New(cfg, logger)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	serv.Mux.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200 OK, got %d", resp.StatusCode)
	}
}

func TestServer_UIDisabled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := Config{
		HTTPListenPort:    8101,
		MetricsListenPort: 8102,
		UiEnabled:         false,
	}

	serv, err := New(cfg, logger)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()

	serv.Mux.ServeHTTP(w, req)

	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("Expected status 404 Not Found, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "Web UI is disabled") {
		t.Errorf("Expected body containing 'Web UI is disabled', got '%s'", string(body))
	}
}

func TestServer_McpEnabledConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := Config{
		HTTPListenPort:    8103,
		MetricsListenPort: 8104,
		McpEnabled:        true,
	}

	serv, err := New(cfg, logger)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	if serv.cfg.McpEnabled != true {
		t.Errorf("Expected McpEnabled to be true")
	}
}

func TestServer_McpDisabledConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := Config{
		HTTPListenPort:    8105,
		MetricsListenPort: 8106,
		McpEnabled:        false,
	}

	serv, err := New(cfg, logger)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	if serv.cfg.McpEnabled != false {
		t.Errorf("Expected McpEnabled to be false")
	}
}
