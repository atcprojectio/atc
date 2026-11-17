package telemetry

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestOTLPEnabledRequiresAnEndpoint(t *testing.T) {
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	assert.False(t, OTLPEnabled("TRACES"), "no endpoint: no exporter, no error spam")

	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://otel:4318/v1/traces")
	assert.True(t, OTLPEnabled("TRACES"))
	assert.False(t, OTLPEnabled("METRICS"), "signal-specific endpoint enables only that signal")

	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://otel:4318")
	assert.True(t, OTLPEnabled("METRICS"))
	assert.True(t, OTLPEnabled("LOGS"))

	t.Setenv("OTEL_SDK_DISABLED", "true")
	assert.False(t, OTLPEnabled("TRACES"))
}
