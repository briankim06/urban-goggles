package main

import (
	"context"
	"errors"
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
