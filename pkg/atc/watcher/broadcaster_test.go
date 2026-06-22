package watcher

import (
	"sync"
	"testing"
	"time"
)

func TestBroadcaster(t *testing.T) {
	b := NewBroadcaster()

	// 1. Test Subscribe & Unsubscribe
	ch1 := b.Subscribe()
	ch2 := b.Subscribe()

	b.mu.RLock()
	subsCount := len(b.subscribers)
	b.mu.RUnlock()
	if subsCount != 2 {
		t.Errorf("expected 2 subscribers, got %d", subsCount)
	}

	b.Unsubscribe(ch1)
	b.mu.RLock()
	subsCount = len(b.subscribers)
	b.mu.RUnlock()
	if subsCount != 1 {
		t.Errorf("expected 1 subscriber after unsubscription, got %d", subsCount)
	}

	// Clean up second
	b.Unsubscribe(ch2)

	// 2. Test Broadcasting
	chA := b.Subscribe()
	chB := b.Subscribe()
	defer b.Unsubscribe(chA)
	defer b.Unsubscribe(chB)

	b.Broadcast("hello")

	select {
	case msg := <-chA:
		if msg != "hello" {
			t.Errorf("chA expected 'hello', got %q", msg)
		}
	default:
		t.Errorf("chA did not receive broadcast")
	}

	select {
	case msg := <-chB:
		if msg != "hello" {
			t.Errorf("chB expected 'hello', got %q", msg)
		}
	default:
		t.Errorf("chB did not receive broadcast")
	}

	// 3. Test Slow Subscriber Bypass (non-blocking)
	// Fill chA to its buffer limit (capacity is 10)
	for i := 0; i < 10; i++ {
		chA <- "fill"
	}

	// This broadcast should be skipped for chA but delivered to chB
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		b.Broadcast("skipped-for-A")
	}()

	// Wait with timeout to ensure it doesn't deadlock
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// success, didn't block
	case <-time.After(1 * time.Second):
		t.Fatal("Broadcast blocked on full subscriber channel")
	}

	// Verify chB received the broadcast
	select {
	case msg := <-chB:
		if msg != "skipped-for-A" {
			t.Errorf("chB expected 'skipped-for-A', got %q", msg)
		}
	default:
		t.Fatal("chB did not receive second broadcast")
	}
}
