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
)

func TestLeaderElectionMock(t *testing.T) {
	var mu sync.Mutex
	sessionCreated := false
	lockAcquired := false
	lockSession := ""

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		// 1. Session create
		if r.Method == "PUT" && r.URL.Path == "/v1/session/create" {
			sessionCreated = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ID":"mock-session-id"}`))
			return
		}

		// 2. KV acquire
		if r.Method == "PUT" && r.URL.Path == "/v1/kv/atc/leader/lock" {
			if r.URL.Query().Get("acquire") == "mock-session-id" {
				lockAcquired = true
				lockSession = "mock-session-id"
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("true"))
				return
			}
		}

		// 3. KV read (blocking query or initial check)
		if r.Method == "GET" && r.URL.Path == "/v1/kv/atc/leader/lock" {
			if lockSession == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{
					"Key": "atc/leader/lock",
					"Value": "dGVzdC1ub2Rl",
					"Session": "mock-session-id",
					"CreateIndex": 100,
					"ModifyIndex": 100
				}
			]`))
			return
		}

		// 4. Session renew
		if r.Method == "PUT" && r.URL.Path == "/v1/session/renew/mock-session-id" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ID":"mock-session-id","TTL":"15s"}`))
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	cfg := Config{
		Name:       "test-node",
		ConsulAddr: server.Listener.Addr().String(),
		HA: HaConfig{
			Enabled:    true,
			LockKey:    "atc/leader/lock",
			SessionTTL: "15s",
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	atcInstance := &Atc{
		Cfg:        cfg,
		logger:     logger,
		coreLogger: logger,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Spin up leader election in background
	go func() {
		_ = atcInstance.runLeaderElection(ctx)
	}()

	// Wait up to 3s for leadership to be acquired
	start := time.Now()
	for time.Since(start) < 3*time.Second {
		if atcInstance.IsLeader() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	sc := sessionCreated
	la := lockAcquired
	mu.Unlock()

	if !sc {
		t.Errorf("Expected Consul session to be created")
	}
	if !la {
		t.Errorf("Expected leader lock key to be acquired")
	}
	if !atcInstance.IsLeader() {
		t.Errorf("Expected atcInstance.IsLeader() to be true")
	}
}

func TestTargetScopedLeaderElectionMock(t *testing.T) {
	var mu sync.Mutex
	sessionCreatedCount := 0
	locksAcquired := make(map[string]bool)
	lockSessions := make(map[string]string)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		// 1. Session create
		if r.Method == "PUT" && r.URL.Path == "/v1/session/create" {
			sessionCreatedCount++
			sessionID := fmt.Sprintf("mock-session-%d", sessionCreatedCount)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"ID":"%s"}`, sessionID)
			return
		}

		// 2. KV acquire
		if r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/v1/kv/atc/leader/lock/") {
			key := strings.TrimPrefix(r.URL.Path, "/v1/kv/")
			sessionID := r.URL.Query().Get("acquire")
			if sessionID != "" {
				locksAcquired[key] = true
				lockSessions[key] = sessionID
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("true"))
				return
			}
		}

		// 3. KV read (blocking query or initial check)
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/kv/atc/leader/lock/") {
			key := strings.TrimPrefix(r.URL.Path, "/v1/kv/")
			sessionID := lockSessions[key]
			if sessionID == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var cleanKey string
			switch {
			case strings.HasSuffix(key, "forwarder"):
				cleanKey = "atc/leader/lock/forwarder"
			case strings.HasSuffix(key, "redirector"):
				cleanKey = "atc/leader/lock/redirector"
			default:
				cleanKey = "atc/leader/lock"
			}
			cleanSessionID := lockSessions[cleanKey]
			if cleanSessionID == "" {
				cleanSessionID = "mock-session-1"
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `[
				{
					"Key": "%s",
					"Value": "dGVzdC1ub2Rl",
					"Session": "%s",
					"CreateIndex": 100,
					"ModifyIndex": 100
				}
			]`, cleanKey, cleanSessionID)
			return
		}

		// 4. Session renew
		if r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/v1/session/renew/") {
			sessionID := strings.TrimPrefix(r.URL.Path, "/v1/session/renew/")
			var cleanSessionID string
			if sessionID == "mock-session-2" {
				cleanSessionID = "mock-session-2"
			} else {
				cleanSessionID = "mock-session-1"
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"ID":"%s","TTL":"15s"}`, cleanSessionID)
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	cfg := Config{
		Name:       "test-node",
		ConsulAddr: server.Listener.Addr().String(),
		Target:     []string{"forwarder", "redirector"},
		HA: HaConfig{
			Enabled:    true,
			LockKey:    "atc/leader/lock",
			SessionTTL: "15s",
		},
	}

	atcInstance, err := New(cfg)
	if err != nil {
		t.Fatalf("Failed to instantiate Atc: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Spin up leader election in background
	go func() {
		_ = atcInstance.runLeaderElection(ctx)
	}()

	// Wait up to 3s for leadership to be acquired
	start := time.Now()
	for time.Since(start) < 3*time.Second {
		if atcInstance.IsLeader() {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	mu.Lock()
	scCount := sessionCreatedCount
	forwarderAcquired := locksAcquired["atc/leader/lock/forwarder"]
	redirectorAcquired := locksAcquired["atc/leader/lock/redirector"]
	mu.Unlock()

	if scCount < 2 {
		t.Errorf("Expected at least 2 Consul sessions to be created, got %d", scCount)
	}
	if !forwarderAcquired {
		t.Errorf("Expected forwarder leader lock key to be acquired")
	}
	if !redirectorAcquired {
		t.Errorf("Expected redirector leader lock key to be acquired")
	}
	if !atcInstance.IsLeader() {
		t.Errorf("Expected atcInstance.IsLeader() to be true")
	}
}

func TestLeaderElection_TeardownOnLockLoss(t *testing.T) {
	var mu sync.Mutex
	sessionCreated := false
	lockAcquired := false
	lockSession := ""
	triggerLockLoss := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		if r.Method == "PUT" && r.URL.Path == "/v1/session/create" {
			sessionCreated = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ID":"mock-session-id"}`))
			return
		}

		if r.Method == "PUT" && r.URL.Path == "/v1/kv/atc/leader/lock" {
			if r.URL.Query().Get("acquire") == "mock-session-id" {
				lockAcquired = true
				lockSession = "mock-session-id"
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("true"))
				return
			}
		}

		if r.Method == "GET" && r.URL.Path == "/v1/kv/atc/leader/lock" {
			indexStr := r.URL.Query().Get("index")
			if indexStr != "" && indexStr != "0" {
				mu.Unlock()
				select {
				case <-triggerLockLoss:
					mu.Lock()
					lockSession = ""
				case <-time.After(1 * time.Second):
					mu.Lock()
				}
			}

			if lockSession == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Consul-Index", "101")
			_, _ = w.Write([]byte(`[
				{
					"Key": "atc/leader/lock",
					"Value": "dGVzdC1ub2Rl",
					"Session": "mock-session-id",
					"CreateIndex": 100,
					"ModifyIndex": 101
				}
			]`))
			return
		}

		if r.Method == "PUT" && r.URL.Path == "/v1/session/renew/mock-session-id" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ID":"mock-session-id","TTL":"15s"}`))
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	cfg := Config{
		Name:       "test-node",
		ConsulAddr: server.Listener.Addr().String(),
		HA: HaConfig{
			Enabled:    true,
			LockKey:    "atc/leader/lock",
			SessionTTL: "15s",
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	atcInstance := &Atc{
		Cfg:        cfg,
		logger:     logger,
		coreLogger: logger,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	electionErrChan := make(chan error, 1)
	go func() {
		electionErrChan <- atcInstance.runLeaderElection(ctx)
	}()

	start := time.Now()
	for time.Since(start) < 2*time.Second {
		if atcInstance.IsLeader() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	sc := sessionCreated
	la := lockAcquired
	mu.Unlock()

	if !sc || !la || !atcInstance.IsLeader() {
		t.Fatalf("Failed to acquire initial leadership")
	}

	// Trigger lock loss
	close(triggerLockLoss)

	start = time.Now()
	for time.Since(start) < 2*time.Second {
		if !atcInstance.IsLeader() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if atcInstance.IsLeader() {
		t.Errorf("Expected leader state to terminate after lock loss")
	}

	cancel()
	<-electionErrChan
}

func TestLeaderElectionGracefulShutdown(t *testing.T) {
	var mu sync.Mutex
	sessionCreated := false
	lockAcquired := false
	lockReleased := false
	lockSession := ""

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()

		// 1. Session create
		if r.Method == "PUT" && r.URL.Path == "/v1/session/create" {
			sessionCreated = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ID":"mock-session-id"}`))
			return
		}

		// 2. KV acquire / release
		if r.Method == "PUT" && r.URL.Path == "/v1/kv/atc/leader/lock" {
			if r.URL.Query().Get("acquire") == "mock-session-id" {
				lockAcquired = true
				lockSession = "mock-session-id"
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("true"))
				return
			}
			if r.URL.Query().Get("release") == "mock-session-id" {
				lockReleased = true
				lockSession = ""
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("true"))
				return
			}
		}

		// 3. KV read
		if r.Method == "GET" && r.URL.Path == "/v1/kv/atc/leader/lock" {
			if lockSession == "" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[
				{
					"Key": "atc/leader/lock",
					"Value": "dGVzdC1ub2Rl",
					"Session": "mock-session-id",
					"CreateIndex": 100,
					"ModifyIndex": 100
				}
			]`))
			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	cfg := Config{
		Name:       "test-node",
		ConsulAddr: server.Listener.Addr().String(),
		HA: HaConfig{
			Enabled:    true,
			LockKey:    "atc/leader/lock",
			SessionTTL: "15s",
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	atcInstance := &Atc{
		Cfg:        cfg,
		logger:     logger,
		coreLogger: logger,
	}

	ctx, cancel := context.WithCancel(context.Background())

	electionErrChan := make(chan error, 1)
	go func() {
		electionErrChan <- atcInstance.runLeaderElection(ctx)
	}()

	// Wait up to 2s for leadership to be acquired
	start := time.Now()
	for time.Since(start) < 2*time.Second {
		if atcInstance.IsLeader() {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	sc := sessionCreated
	la := lockAcquired
	mu.Unlock()

	if !sc || !la || !atcInstance.IsLeader() {
		t.Fatalf("Failed to acquire initial leadership")
	}

	// Trigger graceful shutdown
	cancel()
	<-electionErrChan

	mu.Lock()
	lr := lockReleased
	mu.Unlock()

	if !lr {
		t.Errorf("Expected lock to be released on graceful shutdown")
	}
}

