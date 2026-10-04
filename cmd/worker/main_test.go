package main

import (
	"context"
	"testing"
	"time"
)

func TestWorkerErrorBackoffGrowsCapsAndResets(t *testing.T) {
	backoff := newWorkerErrorBackoff(time.Second, 30*time.Second)
	want := []time.Duration{
		time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		16 * time.Second,
		30 * time.Second,
		30 * time.Second,
	}

	for i, expected := range want {
		if got := backoff.Next(); got != expected {
			t.Fatalf("attempt %d: Next() = %s, want %s", i+1, got, expected)
		}
	}

	backoff.Reset()
	if got := backoff.Next(); got != time.Second {
		t.Fatalf("Next() after Reset() = %s, want %s", got, time.Second)
	}
}

func TestWaitForWorkerLoopStopsWhenContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan bool, 1)
	go func() {
		done <- waitForWorkerLoop(ctx, time.Hour)
	}()

	select {
	case got := <-done:
		if got {
			t.Fatal("waitForWorkerLoop() = true, want false for cancelled context")
		}
	case <-time.After(time.Second):
		t.Fatal("waitForWorkerLoop() did not stop after context cancellation")
	}
}
