package atc

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	atc_server "github.com/atcprojectio/atc/pkg/atc/server"
)

// TestRunWithServerModuleDoesNotPanic guards against duplicate route registration
// (e.g. /ready being registered in both initServer and Run), which makes
// net/http.ServeMux panic at startup.
func TestRunWithServerModuleDoesNotPanic(t *testing.T) {
	cfg := Config{
		Target:     []string{Forwarder}, // pulls in the server module as a dependency
		ConsulAddr: "127.0.0.1:1",
		Server: atc_server.Config{
			HTTPListenPort:    0,
			MetricsListenPort: 0,
			UiEnabled:         false,
			McpEnabled:        false,
		},
	}

	instance, err := New(cfg)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	require.NotNil(t, instance.Server, "server module must be enabled for this test")

	var runErr error
	assert.NotPanics(t, func() {
		runErr = instance.Run(ctx)
	})
	if runErr != nil {
		assert.NotContains(t, runErr.Error(), "invalid target")
	}
}
