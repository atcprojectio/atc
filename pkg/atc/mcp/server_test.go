package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/common/version"
	"github.com/stretchr/testify/assert"
)

type mockService struct{}

func (m *mockService) GetEnabledModulesTable() string                              { return "" }
func (m *mockService) GetAtcEnabledServices(ctx context.Context) ([]string, error) { return nil, nil }
func (m *mockService) PurgeServiceResolver(ctx context.Context, name string) error { return nil }
func (m *mockService) IsLeader() bool                                              { return false }
func (m *mockService) GetFederationStatus(ctx context.Context) (map[string]string, error) {
	return nil, nil
}
func (m *mockService) ApplyFailoverOverride(ctx context.Context, service, targetDc, targetNs, duration string, force bool) error {
	return nil
}
func (m *mockService) TriggerManualRedirect(ctx context.Context, service, redirectDc, redirectNs, duration string, force bool) error {
	return nil
}
func (m *mockService) GetPredefinedStrategies(ctx context.Context) (string, error) { return "", nil }
func (m *mockService) ListActiveOverrides(ctx context.Context) ([]map[string]any, error) {
	return nil, nil
}
func (m *mockService) TriggerConfigReload() error { return nil }

type testService struct {
	isLeader        bool
	services        []string
	fedStatus       map[string]string
	strategies      string
	overrides       []map[string]any
	purgedSvc       string
	overrideSvc     string
	overrideDc      string
	overrideNs      string
	overrideDur     string
	redirectSvc     string
	redirectDc      string
	redirectNs      string
	redirectDur     string
	reloadTriggered bool
}

func (m *testService) GetEnabledModulesTable() string { return "" }
func (m *testService) GetAtcEnabledServices(ctx context.Context) ([]string, error) {
	return m.services, nil
}
func (m *testService) PurgeServiceResolver(ctx context.Context, name string) error {
	m.purgedSvc = name
	return nil
}
func (m *testService) IsLeader() bool {
	return m.isLeader
}
func (m *testService) GetFederationStatus(ctx context.Context) (map[string]string, error) {
	return m.fedStatus, nil
}
func (m *testService) ApplyFailoverOverride(ctx context.Context, service, targetDc, targetNs, duration string, force bool) error {
	m.overrideSvc = service
	m.overrideDc = targetDc
	m.overrideNs = targetNs
	m.overrideDur = duration
	return nil
}
func (m *testService) TriggerManualRedirect(ctx context.Context, service, redirectDc, redirectNs, duration string, force bool) error {
	m.redirectSvc = service
	m.redirectDc = redirectDc
	m.redirectNs = redirectNs
	m.redirectDur = duration
	return nil
}
func (m *testService) GetPredefinedStrategies(ctx context.Context) (string, error) {
	return m.strategies, nil
}
func (m *testService) ListActiveOverrides(ctx context.Context) ([]map[string]any, error) {
	return m.overrides, nil
}
func (m *testService) TriggerConfigReload() error {
	m.reloadTriggered = true
	return nil
}

func TestMcpServerVersion(t *testing.T) {
	mockSvc := &mockService{}
	handler := NewHandler(mockSvc)

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

	assert.Equal(t, "atc", resp.Result.ServerInfo.Name)
	assert.Equal(t, version.Version, resp.Result.ServerInfo.Version)
}

func callToolSync(t *testing.T, handler http.Handler, sessionId string, name string, args any) string {
	reqMap := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": args,
		},
	}
	body, _ := json.Marshal(reqMap)
	req := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", sessionId)
	rr := httptest.NewRecorder()

	handler.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("POST to /mcp for tool %s returned status %d. Body: %s", name, rr.Code, rr.Body.String())
	}

	lines := strings.Split(rr.Body.String(), "\n")
	var dataJSON string
	for _, line := range lines {
		if strings.HasPrefix(line, "data: ") {
			dataJSON = strings.TrimPrefix(line, "data: ")
			break
		}
	}
	assert.NotEmpty(t, dataJSON, "No response data for tool %s", name)

	var resp struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}

	err := json.Unmarshal([]byte(dataJSON), &resp)
	assert.NoError(t, err)
	if resp.Error != nil {
		t.Fatalf("Tool %s JSON-RPC error: %d - %s", name, resp.Error.Code, resp.Error.Message)
	}
	if len(resp.Result.Content) > 0 {
		return resp.Result.Content[0].Text
	}
	return ""
}

func TestMcpToolsCall(t *testing.T) {
	svc := &testService{
		isLeader:   true,
		services:   []string{"payment-service", "user-service"},
		fedStatus:  map[string]string{"dc1": "alive", "dc2": "failed"},
		strategies: "failover-strategies",
		overrides: []map[string]any{
			{"service": "payment-service", "type": "failover", "target_dc": "dc2", "expires_at": "never"},
		},
	}

	handler := NewHandler(svc)

	// 1. Send POST initialize request first to get a session ID
	reqInitBody := `{
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
	reqInit := httptest.NewRequest("POST", "/mcp", strings.NewReader(reqInitBody))
	reqInit.Header.Set("Content-Type", "application/json")
	reqInit.Header.Set("Accept", "application/json, text/event-stream")
	rrInit := httptest.NewRecorder()
	handler.ServeHTTP(rrInit, reqInit)
	assert.Equal(t, 200, rrInit.Code)

	sessionId := rrInit.Header().Get("Mcp-Session-Id")
	assert.NotEmpty(t, sessionId)

	// Send initialized notification to transition state
	reqInitDoneBody := `{
		"jsonrpc": "2.0",
		"method": "notifications/initialized"
	}`
	reqInitDone := httptest.NewRequest("POST", "/mcp", strings.NewReader(reqInitDoneBody))
	reqInitDone.Header.Set("Content-Type", "application/json")
	reqInitDone.Header.Set("Accept", "application/json, text/event-stream")
	reqInitDone.Header.Set("Mcp-Session-Id", sessionId)
	rrInitDone := httptest.NewRecorder()
	handler.ServeHTTP(rrInitDone, reqInitDone)
	assert.True(t, rrInitDone.Code == 200 || rrInitDone.Code == 202)

	// 2. Call tools synchronously

	// 1. check_readiness
	text := callToolSync(t, handler, sessionId, "check_readiness", map[string]any{})
	assert.Equal(t, "OK", text)

	// 2. check_leadership
	text = callToolSync(t, handler, sessionId, "check_leadership", map[string]any{})
	assert.Contains(t, text, "ACTIVE LEADER")

	svc.isLeader = false
	text = callToolSync(t, handler, sessionId, "check_leadership", map[string]any{})
	assert.Contains(t, text, "STANDBY")

	// 3. list_atc_enabled_services
	text = callToolSync(t, handler, sessionId, "list_atc_enabled_services", map[string]any{})
	assert.Contains(t, text, "payment-service")
	assert.Contains(t, text, "user-service")

	// 4. list_wan_federation_status
	text = callToolSync(t, handler, sessionId, "list_wan_federation_status", map[string]any{})
	assert.Contains(t, text, "dc1: ALIVE")
	assert.Contains(t, text, "dc2: FAILED")

	// 5. list_strategies
	text = callToolSync(t, handler, sessionId, "list_strategies", map[string]any{})
	assert.Equal(t, "failover-strategies", text)

	// 6. list_active_overrides
	text = callToolSync(t, handler, sessionId, "list_active_overrides", map[string]any{})
	assert.Contains(t, text, "payment-service: Type=failover, Target=dc2, Expires=never")

	// 7. purge_redirect_config
	text = callToolSync(t, handler, sessionId, "purge_redirect_config", map[string]any{"name": "user-service"})
	assert.Contains(t, text, "Successfully purged service-resolver config entry for user-service")
	assert.Equal(t, "user-service", svc.purgedSvc)

	// 8. apply_failover_override
	text = callToolSync(t, handler, sessionId, "apply_failover_override", map[string]any{
		"service":   "payment-service",
		"target_dc": "dc2",
		"namespace": "custom-ns",
		"duration":  "5m",
	})
	assert.Contains(t, text, "Successfully applied failover override for service payment-service to datacenter dc2")
	assert.Equal(t, "payment-service", svc.overrideSvc)
	assert.Equal(t, "dc2", svc.overrideDc)
	assert.Equal(t, "custom-ns", svc.overrideNs)
	assert.Equal(t, "5m", svc.overrideDur)

	// 9. trigger_manual_redirect
	text = callToolSync(t, handler, sessionId, "trigger_manual_redirect", map[string]any{
		"service":     "billing-service",
		"redirect_dc": "dc3",
		"duration":    "10m",
	})
	assert.Contains(t, text, "Successfully triggered manual redirect for service billing-service to datacenter dc3")
	assert.Equal(t, "billing-service", svc.redirectSvc)
	assert.Equal(t, "dc3", svc.redirectDc)
	assert.Equal(t, "10m", svc.redirectDur)

	// 10. reload_config
	text = callToolSync(t, handler, sessionId, "reload_config", map[string]any{})
	assert.Contains(t, text, "Configuration reloaded successfully")
	assert.True(t, svc.reloadTriggered)
}

type dryRunService struct{ mockService }

func (*dryRunService) IsDryRun() bool { return true }

func TestWriteToolsReportDryRun(t *testing.T) {
	assert.True(t, strings.HasPrefix(dryRunText(&dryRunService{}, "Successfully purged x"), "[DRY RUN] Nothing was written"))
	assert.Equal(t, "Successfully purged x", dryRunText(&mockService{}, "Successfully purged x"))
}

type auditEntry struct {
	action  string
	service string
	details map[string]any
}

type auditingService struct {
	mockService
	dryRun   bool
	nextErr  error
	recorded []auditEntry
}

func (a *auditingService) IsDryRun() bool { return a.dryRun }

func (a *auditingService) LogAudit(_ context.Context, action, service string, details map[string]any) {
	copied := make(map[string]any, len(details))
	for k, v := range details {
		copied[k] = v
	}
	a.recorded = append(a.recorded, auditEntry{action: action, service: service, details: copied})
}

func (a *auditingService) PurgeServiceResolver(_ context.Context, _ string) error {
	return a.nextErr
}

func (a *auditingService) ApplyFailoverOverride(_ context.Context, _, _, _, _ string, _ bool) error {
	return a.nextErr
}

func (a *auditingService) TriggerManualRedirect(_ context.Context, _, _, _, _ string, _ bool) error {
	return a.nextErr
}

func TestMcpWriteToolsAuditOutcomesAndForce(t *testing.T) {
	svc := &auditingService{}
	handler := NewHandler(svc)

	reqInit := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"1.0"}}}`))
	reqInit.Header.Set("Content-Type", "application/json")
	reqInit.Header.Set("Accept", "application/json, text/event-stream")
	rrInit := httptest.NewRecorder()
	handler.ServeHTTP(rrInit, reqInit)
	sessionId := rrInit.Header().Get("Mcp-Session-Id")
	assert.NotEmpty(t, sessionId)

	reqDone := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	reqDone.Header.Set("Content-Type", "application/json")
	reqDone.Header.Set("Accept", "application/json, text/event-stream")
	reqDone.Header.Set("Mcp-Session-Id", sessionId)
	handler.ServeHTTP(httptest.NewRecorder(), reqDone)

	// 1. Applied override with force=true records outcome=applied and force=true
	callToolSync(t, handler, sessionId, "apply_failover_override", map[string]any{
		"service":   "payments",
		"target_dc": "dc2",
		"force":     true,
	})
	assert.Len(t, svc.recorded, 1)
	assert.Equal(t, "create_override", svc.recorded[0].action)
	assert.Equal(t, "payments", svc.recorded[0].service)
	assert.Equal(t, "applied", svc.recorded[0].details["outcome"])
	assert.Equal(t, true, svc.recorded[0].details["force"])

	// 2. Dry-run redirect records outcome=dry_run
	svc.dryRun = true
	callToolSync(t, handler, sessionId, "trigger_manual_redirect", map[string]any{
		"service":     "payments",
		"redirect_dc": "dc3",
	})
	assert.Len(t, svc.recorded, 2)
	assert.Equal(t, "dry_run", svc.recorded[1].details["outcome"])
	assert.Equal(t, false, svc.recorded[1].details["force"])

	// 3. Dry-run purge records outcome=dry_run
	callToolSync(t, handler, sessionId, "purge_redirect_config", map[string]any{"name": "payments"})
	assert.Len(t, svc.recorded, 3)
	assert.Equal(t, "purge_resolver", svc.recorded[2].action)
	assert.Equal(t, "dry_run", svc.recorded[2].details["outcome"])

	// 4. Refused foreign resolver override records outcome=rejected
	svc.dryRun = false
	svc.nextErr = errors.New(`refusing to overwrite service-resolver for "payments" that ATC did not create`)
	callToolSync(t, handler, sessionId, "apply_failover_override", map[string]any{
		"service":   "payments",
		"target_dc": "dc2",
	})
	assert.Len(t, svc.recorded, 4)
	assert.Equal(t, "rejected", svc.recorded[3].details["outcome"])

	// 5. Upstream error records outcome=failed
	svc.nextErr = assert.AnError
	callToolSync(t, handler, sessionId, "apply_failover_override", map[string]any{
		"service":   "payments",
		"target_dc": "dc2",
	})
	assert.Len(t, svc.recorded, 5)
	assert.Equal(t, "failed", svc.recorded[4].details["outcome"])
}

