package watcher

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func TestConsulWatcher(t *testing.T) {
	var servicesQueryCount int32
	var checksQueryCount int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/v1/catalog/services" {
			count := atomic.AddInt32(&servicesQueryCount, 1)
			indexStr := r.URL.Query().Get("index")
			index, _ := strconv.Atoi(indexStr)

			w.Header().Set("X-Consul-Index", strconv.Itoa(int(count)))

			if index > 0 {
				// Block for a short bit to simulate Consul blocking query
				time.Sleep(10 * time.Millisecond)
			}
			services := map[string][]string{
				"test-service": {"atc.enabled=true"},
			}
			_ = json.NewEncoder(w).Encode(services)
			return
		}

		if r.URL.Path == "/v1/health/state/any" {
			count := atomic.AddInt32(&checksQueryCount, 1)
			indexStr := r.URL.Query().Get("index")
			index, _ := strconv.Atoi(indexStr)

			w.Header().Set("X-Consul-Index", strconv.Itoa(int(count)))

			if index > 0 {
				// Block for a short bit
				time.Sleep(10 * time.Millisecond)
			}
			checks := []map[string]any{
				{
					"Node":        "node1",
					"CheckID":     "service:test-service",
					"Name":        "Service 'test-service' check",
					"Status":      "passing",
					"ServiceID":   "test-service",
					"ServiceName": "test-service",
				},
			}
			_ = json.NewEncoder(w).Encode(checks)
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := New(logger, server.Listener.Addr().String(), "", "", "")

	ch := w.Events.Subscribe()
	defer w.Events.Unsubscribe(ch)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errChan := make(chan error, 1)
	go func() {
		errChan <- w.Run(ctx)
	}()

	// Wait for events to be broadcasted
	servicesUpdated := false
	checksUpdated := false

	timeout := time.After(2 * time.Second)
	for !servicesUpdated || !checksUpdated {
		select {
		case msg := <-ch:
			if msg == "services_update" {
				servicesUpdated = true
			}
			if msg == "checks_update" {
				checksUpdated = true
			}
		case <-timeout:
			t.Fatal("timed out waiting for watcher updates")
		}
	}

	cancel()
	select {
	case err := <-errChan:
		if err != nil {
			t.Errorf("expected clean shutdown, got error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Error("watcher did not shutdown cleanly after context cancellation")
	}
}
