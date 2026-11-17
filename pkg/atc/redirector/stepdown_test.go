package redirector

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/consul/api"
)

// TestScheduledWriteDroppedAfterStepDown covers the window where a dampening timer has
// fired but the write has not happened yet when leadership is lost: the write must be dropped.
func TestScheduledWriteDroppedAfterStepDown(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			writes.Add(1)
		}
		_, _ = w.Write([]byte("true"))
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	f, err := New(logger, server.Listener.Addr().String(), "", "", "", "", false, nil, "", "", false, false)
	if err != nil {
		t.Fatal(err)
	}

	entry := &api.ServiceResolverConfigEntry{Kind: api.ServiceResolver, Name: "svc"}

	// Active term: the scheduled write goes through.
	runCtx, cancel := context.WithCancel(context.Background())
	f.schedMu.Lock()
	f.runCtx = runCtx
	f.pendingWrites["svc"] = entry
	f.schedMu.Unlock()
	f.executeScheduledWrite("svc")
	if got := writes.Load(); got != 1 {
		t.Fatalf("expected 1 write while leader, got %d", got)
	}

	// Step down, then a timer that had already fired runs its callback.
	cancel()
	f.stopScheduledWrites(context.Background())
	f.schedMu.Lock()
	f.pendingWrites["svc"] = entry
	f.schedMu.Unlock()
	f.executeScheduledWrite("svc")
	if got := writes.Load(); got != 1 {
		t.Fatalf("expected scheduled write to be dropped after step-down, got %d writes", got)
	}
}
