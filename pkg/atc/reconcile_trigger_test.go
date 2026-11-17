package atc

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hashicorp/consul/api"
	"github.com/stretchr/testify/require"

	"github.com/atcprojectio/atc/pkg/atc/watcher"
)

func expectEvent(t *testing.T, ch chan string, want string) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case ev := <-ch:
			if ev == want {
				return
			}
		case <-deadline:
			t.Fatalf("expected %q event", want)
		}
	}
}

func TestReloadAndPurgeRequestReconcile(t *testing.T) {
	consul := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/config/service-resolver/payments":
			_, _ = w.Write([]byte(`{"Kind":"service-resolver","Name":"payments","Meta":{"created-by":"atc-override"}}`))
		case r.Method == http.MethodDelete:
			_, _ = w.Write([]byte(`true`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer consul.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &Atc{
		Cfg:           Config{ConsulAddr: consul.Listener.Addr().String()},
		logger:        logger,
		coreLogger:    logger,
		consulClients: make(map[string]*api.Client),
		Watcher:       watcher.New(logger, consul.Listener.Addr().String(), "", "", ""),
	}
	events := a.Watcher.Events.Subscribe()
	defer a.Watcher.Events.Unsubscribe(events)

	a.ReloadConfig(Config{ConsulAddr: consul.Listener.Addr().String()})
	expectEvent(t, events, "config_reload")

	require.NoError(t, a.PurgeServiceResolver(context.Background(), "payments"))
	expectEvent(t, events, "resolver_purged")
}
