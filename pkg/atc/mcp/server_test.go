package mcp

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/common/version"
)

type mockService struct{}

func (m *mockService) GetEnabledModulesTable() string                                              { return "" }
func (m *mockService) GetAtcEnabledServices(ctx context.Context) ([]string, error)                { return nil, nil }
func (m *mockService) PurgeServiceResolver(ctx context.Context, name string) error                { return nil }
func (m *mockService) IsLeader() bool                                                              { return false }
func (m *mockService) GetFederationStatus(ctx context.Context) (map[string]string, error)          { return nil, nil }
func (m *mockService) ApplyFailoverOverride(ctx context.Context, service, targetDc, targetNs, duration string) error { return nil }
func (m *mockService) TriggerManualRedirect(ctx context.Context, service, redirectDc, redirectNs, duration string) error { return nil }
func (m *mockService) GetPredefinedStrategies(ctx context.Context) (string, error)                 { return "", nil }
func (m *mockService) ListActiveOverrides(ctx context.Context) ([]map[string]any, error)           { return nil, nil }
func (m *mockService) TriggerConfigReload() error                                                  { return nil }

func TestMcpServerVersion(t *testing.T) {
	mockSvc := &mockService{}
	handler := NewHandler(mockSvc)

	// Construct JSON-RPC initialize request
	reqBody := `{
		"jsonrpc": "2.0",
		"id": 1,
		"method": "initialize",
		"params": {
			"protocolVersion": "2024-11-05",
			"capabilities": {},
			"clientInfo": {
				"name": "test-client",
				"version": "1.0.0"
			}
		}
	}`

	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)

	if rr.Code != 200 {
		t.Fatalf("expected status 200, got %d. Body: %s", rr.Code, rr.Body.String())
	}

	var resp struct {
		Result struct {
			ServerInfo struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"serverInfo"`
		} `json:"result"`
	}

	// Parse SSE stream data block
	lines := strings.Split(rr.Body.String(), "\n")
	var dataJSON string
	for _, line := range lines {
		if strings.HasPrefix(line, "data: ") {
			dataJSON = strings.TrimPrefix(line, "data: ")
			break
		}
	}
	if dataJSON == "" {
		t.Fatalf("expected data line in SSE response, got body: %s", rr.Body.String())
	}

	if err := json.Unmarshal([]byte(dataJSON), &resp); err != nil {
		t.Fatalf("failed to unmarshal JSON-RPC response: %v", err)
	}

	if resp.Result.ServerInfo.Name != "atc" {
		t.Errorf("expected server name 'atc', got %q", resp.Result.ServerInfo.Name)
	}

	if resp.Result.ServerInfo.Version != version.Version {
		t.Errorf("expected MCP server version %q to match version.Version %q", resp.Result.ServerInfo.Version, version.Version)
	}
}
