package telemetry

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/common/version"
)

func TestInit(t *testing.T) {
	ctx := context.Background()
	shutdown, err := Init(ctx, "test-atc-service")
	if err != nil {
		t.Fatalf("failed to initialize telemetry: %v", err)
	}

	if GlobalPrometheusHandler == nil {
		t.Error("expected GlobalPrometheusHandler to be initialized, got nil")
	}

	// Verify atc_build_info is exported and matches version.Version
	req := httptest.NewRequest("GET", "/metrics", nil)
	rr := httptest.NewRecorder()
	GlobalPrometheusHandler.ServeHTTP(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, "atc_build_info") {
		t.Error("expected atc_build_info metric in Prometheus output, got none")
	}

	// Only test matching if version is not empty (it is populated at build time by ldflags)
	if version.Version != "" {
		expectedVersionLabel := `version="` + version.Version + `"`
		if !strings.Contains(body, expectedVersionLabel) {
			t.Errorf("expected atc_build_info metric to contain %q, output:\n%s", expectedVersionLabel, body)
		}
	}

	err = shutdown(ctx)
	if err != nil {
		t.Logf("shutdown returned error (expected if no local collector is running): %v", err)
	}
}
