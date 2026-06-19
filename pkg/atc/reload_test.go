package atc

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/atcprojectio/atc/pkg/atc/forwarder"
	"github.com/atcprojectio/atc/pkg/atc/redirector"
)

func TestAtcConfigReloadingRace(t *testing.T) {
	cfg := Config{
		DampeningPeriod:    "5s",
		MinDampeningPeriod: "0s",
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	atcInstance := &Atc{
		Cfg:        cfg,
		logger:     logger,
		coreLogger: logger,
	}

	// Initialize Forwarder and Redirector
	fwd, err := forwarder.New(logger, "", "", "", nil, "5s", "0s")
	if err != nil {
		t.Fatalf("failed to create forwarder: %v", err)
	}
	redir, err := redirector.New(logger, "", "", "", false, nil, "5s", "0s")
	if err != nil {
		t.Fatalf("failed to create redirector: %v", err)
	}

	atcInstance.Forwarder = fwd
	atcInstance.Redirector = redir

	done := make(chan struct{})
	var wg sync.WaitGroup

	// Racy Readers: call IsLeader and simulate consul client creation
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					_ = atcInstance.IsLeader()
					_, _ = atcInstance.getConsulClient(context.Background())
				}
			}
		}()
	}

	// Racy Writer: trigger ReloadConfig
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			newCfg := Config{
				DampeningPeriod:    "10s",
				MinDampeningPeriod: "1s",
			}
			atcInstance.ReloadConfig(newCfg)
			time.Sleep(1 * time.Microsecond)
		}
	}()

	time.Sleep(50 * time.Millisecond)
	close(done)
	wg.Wait()

	// Verify final reloaded config values
	atcInstance.cfgMu.RLock()
	dp := atcInstance.Cfg.DampeningPeriod
	minDp := atcInstance.Cfg.MinDampeningPeriod
	atcInstance.cfgMu.RUnlock()

	if dp != "10s" {
		t.Errorf("expected DampeningPeriod to be reloaded to 10s, got %s", dp)
	}
	if minDp != "1s" {
		t.Errorf("expected MinDampeningPeriod to be reloaded to 1s, got %s", minDp)
	}
}
