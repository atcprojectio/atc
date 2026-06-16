package telemetry

import (
	"context"
	"testing"
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

	err = shutdown(ctx)
	if err != nil {
		t.Logf("shutdown returned error (expected if no local collector is running): %v", err)
	}
}
