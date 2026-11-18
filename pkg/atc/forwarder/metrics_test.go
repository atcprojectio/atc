package forwarder

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/atcprojectio/atc/pkg/atc/telemetry"
	"github.com/atcprojectio/atc/pkg/atc/telemetry/telemetrytest"
)

// TestDurationHistogramsUseSecondBuckets records millisecond-scale durations and scrapes them
// through the real Prometheus exporter. With the SDK's default (millisecond) boundaries every
// sample landed in the 0-5 bucket and p95 came out as ~4.75s for a 3ms reconcile.
func TestDurationHistogramsUseSecondBuckets(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "true") // no OTLP exporters; Prometheus is always on
	ctx := context.Background()
	shutdown, err := telemetry.Init(ctx, "atc-test")
	require.NoError(t, err)
	t.Cleanup(func() { _ = shutdown(ctx) })

	f, err := New(slog.New(slog.NewTextHandler(io.Discard, nil)), "127.0.0.1:1", "", "dc1", "", "", nil, "", "", false)
	require.NoError(t, err)
	f.reconcileDuration.Record(ctx, 0.003)
	f.consulDurationHistogram.Record(ctx, 0.0004)

	telemetrytest.AssertSecondBuckets(t, "atc_forwarder_reconcile_duration_seconds", "0.0025", "0.005")
	telemetrytest.AssertSecondBuckets(t, "atc_consul_request_duration_seconds", "0", "0.001")
}
