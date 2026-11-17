package atc

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/atcprojectio/atc/pkg/atc/forwarder"
	"github.com/atcprojectio/atc/pkg/atc/redirector"
)

func TestIsSweepOwner(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fwd, err := forwarder.New(logger, "", "", "", "", "", nil, "", "", false)
	require.NoError(t, err)
	red, err := redirector.New(logger, "", "", "", "", "", true, nil, "", "", false, false)
	require.NoError(t, err)

	t.Run("HA disabled: every node sweeps", func(t *testing.T) {
		assert.True(t, (&Atc{Forwarder: fwd}).isSweepOwner())
	})
	t.Run("forwarder leader sweeps, even without the redirector lock", func(t *testing.T) {
		a := &Atc{Cfg: Config{HA: HaConfig{Enabled: true}}, Forwarder: fwd, Redirector: red}
		a.forwarderLeader.Store(true)
		assert.True(t, a.isSweepOwner(), "split leadership must still have one sweeper")
	})
	t.Run("redirector leader alone does not sweep when a forwarder runs", func(t *testing.T) {
		a := &Atc{Cfg: Config{HA: HaConfig{Enabled: true}}, Forwarder: fwd, Redirector: red}
		a.redirectorLeader.Store(true)
		assert.False(t, a.isSweepOwner(), "exactly one node sweeps: the forwarder leader")
	})
	t.Run("redirector-only deployment: redirector leader sweeps", func(t *testing.T) {
		a := &Atc{Cfg: Config{HA: HaConfig{Enabled: true}}, Redirector: red}
		a.redirectorLeader.Store(true)
		assert.True(t, a.isSweepOwner())
	})
	t.Run("server-only deployment: core leader sweeps", func(t *testing.T) {
		a := &Atc{Cfg: Config{HA: HaConfig{Enabled: true}}}
		assert.False(t, a.isSweepOwner())
		a.isLeader.Store(true)
		assert.True(t, a.isSweepOwner())
	})
}

// overrideConsul serves three overrides (expired, future, permanent) and records deletes.
func overrideConsul(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	past := time.Now().Add(-time.Minute).Format(time.RFC3339)
	future := time.Now().Add(time.Hour).Format(time.RFC3339)
	entries := map[string]string{
		"expired":   fmt.Sprintf(`{"Kind":"service-resolver","Name":"expired","Meta":{"created-by":"atc-override","atc-override-expires-at":%q}}`, past),
		"future":    fmt.Sprintf(`{"Kind":"service-resolver","Name":"future","Meta":{"created-by":"atc-override","atc-override-expires-at":%q}}`, future),
		"permanent": `{"Kind":"service-resolver","Name":"permanent","Meta":{"created-by":"atc-override"}}`,
	}
	var mu sync.Mutex
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		name := strings.TrimPrefix(r.URL.Path, "/v1/config/service-resolver/")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/config/service-resolver":
			_, _ = w.Write([]byte("[" + entries["expired"] + "," + entries["future"] + "," + entries["permanent"] + "]"))
		case r.Method == http.MethodGet && entries[name] != "":
			_, _ = w.Write([]byte(entries[name]))
		case r.Method == http.MethodDelete:
			mu.Lock()
			deleted = append(deleted, name)
			mu.Unlock()
			_, _ = w.Write([]byte("true"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), deleted...) }
}

func newSweepAtc(addr string) *Atc {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Atc{Cfg: Config{ConsulAddr: addr}, logger: logger, coreLogger: logger, consulClients: map[string]*api.Client{}}
}

func TestSweepPurgesOnlyExpiredOverrides(t *testing.T) {
	srv, deleted := overrideConsul(t)
	newSweepAtc(srv.Listener.Addr().String()).sweepExpiredOverridesOnce(context.Background())
	assert.Equal(t, []string{"expired"}, deleted())
}

func TestSweepDoesNothingOnNonOwnerOrInDryRun(t *testing.T) {
	srv, deleted := overrideConsul(t)

	standby := newSweepAtc(srv.Listener.Addr().String())
	standby.Cfg.HA.Enabled = true // not the leader of anything
	standby.sweepExpiredOverridesOnce(context.Background())

	dry := newSweepAtc(srv.Listener.Addr().String())
	dry.Cfg.DryRun = true
	dry.sweepExpiredOverridesOnce(context.Background())

	assert.Empty(t, deleted())
}

func TestSweepLoopRunsOnInterval(t *testing.T) {
	orig := overrideSweepInterval
	overrideSweepInterval = 20 * time.Millisecond
	defer func() { overrideSweepInterval = orig }()

	srv, deleted := overrideConsul(t)
	a := newSweepAtc(srv.Listener.Addr().String())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.sweepExpiredOverrides(ctx)
	require.Eventually(t, func() bool { return len(deleted()) > 0 }, 2*time.Second, 10*time.Millisecond)
}
