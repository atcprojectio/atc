package atc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTriggerConfigReloadUsesLoaderAndRejectsInvalidConfig(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	running := Config{DryRun: true, WriteRateLimit: "1s"}
	a := &Atc{Cfg: running, logger: logger, coreLogger: logger, consulClients: map[string]*api.Client{}}

	// Invalid config: rejected, logged, running config kept.
	a.SetConfigLoader(func() (Config, error) { return Config{WriteRateLimit: "10/s"}, nil })
	err := a.TriggerConfigReload()
	require.Error(t, err)
	assert.Contains(t, logs.String(), "Configuration reload failed")
	assert.Equal(t, "1s", a.Cfg.WriteRateLimit)

	// Loader errors (e.g. unreadable file) are logged too.
	logs.Reset()
	a.SetConfigLoader(func() (Config, error) { return Config{}, errors.New("cannot read file") })
	require.Error(t, a.TriggerConfigReload())
	assert.Contains(t, logs.String(), "cannot read file")

	// Valid config from the loader (which applies CLI flags in the server) is applied as-is.
	a.SetConfigLoader(func() (Config, error) {
		return Config{DryRun: true, WriteRateLimit: "2s", Auth: AuthConfig{AllowUnauthenticated: true}}, nil
	})
	require.NoError(t, a.TriggerConfigReload())
	assert.Equal(t, "2s", a.Cfg.WriteRateLimit)
	assert.True(t, a.Cfg.DryRun)
}

func TestForceUnlockIsAudited(t *testing.T) {
	consul := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/kv/"):
			_, _ = w.Write([]byte(`[{"Key":"atc/leader/lock/forwarder","Value":"bm9kZS1h","Session":"sess-1"}]`))
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/v1/session/destroy/"):
			_, _ = w.Write([]byte(`true`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer consul.Close()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	a := &Atc{
		Cfg:           Config{ConsulAddr: consul.Listener.Addr().String(), HA: HaConfig{Enabled: true}},
		logger:        logger,
		coreLogger:    logger,
		consulClients: map[string]*api.Client{},
	}
	require.NoError(t, a.ForceUnlock(context.Background(), "forwarder"))
	out := logs.String()
	assert.Contains(t, out, "action=force_unlock")
	assert.Contains(t, out, "session_id=sess-1")
	assert.Contains(t, out, "held_by=node-a")
}
