package ingest

import (
	"testing"

	transit "github.com/briankim06/urban-goggles/proto/transit"
)

func mkEvent(trip, stop string, delay int32) *transit.DelayEvent {
	return &transit.DelayEvent{TripId: trip, StopId: stop, DelaySeconds: delay}
}

func TestDiffer_FirstPollEmitsEverything(t *testing.T) {
	d := NewDiffer()
	in := []*transit.DelayEvent{
		mkEvent("t1", "s1", 0),
		mkEvent("t1", "s2", 60),
	}
	out := d.Diff(in)
	if len(out) != 2 {
		t.Fatalf("first poll: want 2 events, got %d", len(out))
	}
}

func TestDiffer_DropsSmallChanges(t *testing.T) {
	d := NewDiffer()
	d.Diff([]*transit.DelayEvent{mkEvent("t1", "s1", 60)})
	// Change of 10s (< 15s threshold) should be suppressed.
	out := d.Diff([]*transit.DelayEvent{mkEvent("t1", "s1", 70)})
	if len(out) != 0 {
		t.Fatalf("small change: want 0 events, got %d", len(out))
	}
}

func TestDiffer_EmitsLargeChanges(t *testing.T) {
	d := NewDiffer()
	d.Diff([]*transit.DelayEvent{mkEvent("t1", "s1", 60)})
	// Change of 30s (> 15s threshold) should pass through.
	out := d.Diff([]*transit.DelayEvent{mkEvent("t1", "s1", 90)})
	if len(out) != 1 {
		t.Fatalf("large change: want 1 event, got %d", len(out))
	}
	if out[0].GetDelaySeconds() != 90 {
		t.Errorf("stored delay not updated: got %d", out[0].GetDelaySeconds())
	}
}
