package forwarder

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReloadedResyncIntervalAppliesWhileRunning: resync starts disabled; a reload enables it
// while Run is active. The failed write must then be retried without a restart.
func TestReloadedResyncIntervalAppliesWhileRunning(t *testing.T) {
	fc := newFakeConsul(t)
	fc.failPuts.Store(true)
	f := newTestForwarder(t, fc.addr())
	f.SetResyncInterval(0)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	defer func() { cancel(); require.NoError(t, <-done) }()

	require.Eventually(t, func() bool { return fc.resolverPuts.Load() >= 1 }, 2*time.Second, 10*time.Millisecond)
	time.Sleep(300 * time.Millisecond)
	settled := fc.resolverPuts.Load()
	fc.failPuts.Store(false)

	f.SetResyncInterval(100 * time.Millisecond) // hot reload
	assert.Eventually(t, func() bool { return fc.resolverPuts.Load() > settled }, 2*time.Second, 20*time.Millisecond,
		"the new resync interval must take effect without restarting Run")
}

// TestSetResyncIntervalConcurrentWithRun must pass under -race.
func TestSetResyncIntervalConcurrentWithRun(t *testing.T) {
	fc := newFakeConsul(t)
	f := newTestForwarder(t, fc.addr())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f.SetResyncInterval(time.Duration(i%3) * 50 * time.Millisecond)
		}(i)
	}
	wg.Wait()
	cancel()
	require.NoError(t, <-done)
}
