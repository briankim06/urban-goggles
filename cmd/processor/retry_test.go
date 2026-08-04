package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestWithRetry_SucceedsAfterFailures(t *testing.T) {
	calls := 0
	err := withRetry(context.Background(), 3, time.Millisecond, func() error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success on attempt 3, got %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestWithRetry_ExhaustsAttempts(t *testing.T) {
	want := errors.New("still down")
	calls := 0
	err := withRetry(context.Background(), 3, time.Millisecond, func() error {
		calls++
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("expected last error, got %v", err)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3", calls)
	}
}

func TestWithRetry_CancelledContextAborts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	err := withRetry(ctx, 5, time.Minute, func() error {
		calls++
		return errors.New("failing")
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no waiting on dead context)", calls)
	}
}

func TestSumLags(t *testing.T) {
	var m sync.Map
	if got := sumLags(&m); got != 0 {
		t.Errorf("empty map sum = %d, want 0", got)
	}
	m.Store("delay-events/0", int64(100))
	m.Store("delay-events/1", int64(0))
	m.Store("delay-events/2", int64(50000))
	if got := sumLags(&m); got != 50100 {
		t.Errorf("sum = %d, want 50100 (one caught-up partition must not mask others)", got)
	}
	m.Delete("delay-events/2")
	if got := sumLags(&m); got != 100 {
		t.Errorf("after delete sum = %d, want 100", got)
	}
}
