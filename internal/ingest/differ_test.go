package ingest

import (
	"testing"
	"time"

	transit "github.com/briankim06/urban-goggles/proto/transit"
)

func mkEvent(trip, stop string, delay int32) *transit.DelayEvent {
	return &transit.DelayEvent{TripId: trip, StopId: stop, DelaySeconds: delay}
}

func mkTimedEvent(trip, stop string, delay int32, predicted int64) *transit.DelayEvent {
	return &transit.DelayEvent{TripId: trip, StopId: stop, DelaySeconds: delay, PredictedArrival: predicted}
}

// diffCommit mimics the poller's happy path: every selected event publishes
// successfully and is committed.
func diffCommit(d *Differ, events []*transit.DelayEvent) []*transit.DelayEvent {
	out := d.Diff(events)
	for _, ev := range out {
		d.Commit(ev)
	}
	return out
}

func TestDiffer_FirstPollEmitsEverything(t *testing.T) {
	d := NewDiffer()
	in := []*transit.DelayEvent{
		mkEvent("t1", "s1", 0),
		mkEvent("t1", "s2", 60),
	}
	out := diffCommit(d, in)
	if len(out) != 2 {
		t.Fatalf("first poll: want 2 events, got %d", len(out))
	}
}

func TestDiffer_DropsSmallChanges(t *testing.T) {
	d := NewDiffer()
	diffCommit(d, []*transit.DelayEvent{mkEvent("t1", "s1", 60)})
	// Change of 10s (< 15s threshold) should be suppressed.
	out := diffCommit(d, []*transit.DelayEvent{mkEvent("t1", "s1", 70)})
	if len(out) != 0 {
		t.Fatalf("small change: want 0 events, got %d", len(out))
	}
}

func TestDiffer_EmitsLargeChanges(t *testing.T) {
	d := NewDiffer()
	diffCommit(d, []*transit.DelayEvent{mkEvent("t1", "s1", 60)})
	// Change of 30s (> 15s threshold) should pass through.
	out := diffCommit(d, []*transit.DelayEvent{mkEvent("t1", "s1", 90)})
	if len(out) != 1 {
		t.Fatalf("large change: want 1 event, got %d", len(out))
	}
	if out[0].GetDelaySeconds() != 90 {
		t.Errorf("stored delay not updated: got %d", out[0].GetDelaySeconds())
	}
}

func TestDiffer_PredictedMovementEmits(t *testing.T) {
	d := NewDiffer()
	// Time-only feed shape: delay stays 0, only the prediction moves.
	diffCommit(d, []*transit.DelayEvent{mkTimedEvent("t1", "s1", 0, 1700000600)})
	out := diffCommit(d, []*transit.DelayEvent{mkTimedEvent("t1", "s1", 0, 1700000900)})
	if len(out) != 1 {
		t.Fatalf("prediction moved 300s: want 1 event, got %d", len(out))
	}
}

func TestDiffer_PredictedSmallMovementSuppressed(t *testing.T) {
	d := NewDiffer()
	diffCommit(d, []*transit.DelayEvent{mkTimedEvent("t1", "s1", 0, 1700000600)})
	out := diffCommit(d, []*transit.DelayEvent{mkTimedEvent("t1", "s1", 0, 1700000610)})
	if len(out) != 0 {
		t.Fatalf("prediction moved 10s: want 0 events, got %d", len(out))
	}
}

func TestDiffer_DiffDoesNotCommit(t *testing.T) {
	d := NewDiffer()
	in := []*transit.DelayEvent{mkEvent("t1", "s1", 60)}
	if out := d.Diff(in); len(out) != 1 {
		t.Fatalf("first Diff: want 1, got %d", len(out))
	}
	// No Commit (publish failed) → same event must re-candidate.
	if out := d.Diff(in); len(out) != 1 {
		t.Fatalf("uncommitted event must re-emit, got %d", len(out))
	}
}

func TestDiffer_CommitSuppresses(t *testing.T) {
	d := NewDiffer()
	in := []*transit.DelayEvent{mkEvent("t1", "s1", 60)}
	diffCommit(d, in)
	if out := d.Diff(in); len(out) != 0 {
		t.Fatalf("committed unchanged event must be suppressed, got %d", len(out))
	}
}

func TestDiffer_TypeChangeEmits(t *testing.T) {
	d := NewDiffer()
	diffCommit(d, []*transit.DelayEvent{mkEvent("t1", "s1", 10)})
	skip := mkEvent("t1", "s1", 10)
	skip.Type = transit.DelayEvent_SKIP_STOP
	out := diffCommit(d, []*transit.DelayEvent{skip})
	if len(out) != 1 {
		t.Fatalf("type change with same delay: want 1 event, got %d", len(out))
	}
}

func TestDiffer_PruneEvictsStaleKeys(t *testing.T) {
	d := NewDiffer()
	diffCommit(d, []*transit.DelayEvent{mkEvent("t1", "s1", 60), mkEvent("t2", "s1", 60)})

	// t2 stays live: an intermediate Diff refreshes its lastSeen.
	future := time.Now().Add(20 * time.Minute)
	d.mu.Lock()
	st := d.prev[diffKey(mkEvent("t2", "s1", 60))]
	st.lastSeen = future
	d.prev[diffKey(mkEvent("t2", "s1", 60))] = st
	d.mu.Unlock()

	pruned := d.Prune(time.Now().Add(31 * time.Minute))
	if pruned != 1 {
		t.Fatalf("pruned = %d, want 1 (only the stale t1 key)", pruned)
	}
	// t1 re-emits as first-seen; t2 stays suppressed.
	out := d.Diff([]*transit.DelayEvent{mkEvent("t1", "s1", 60), mkEvent("t2", "s1", 60)})
	if len(out) != 1 || out[0].GetTripId() != "t1" {
		t.Fatalf("after prune: want only t1 re-emitted, got %v", out)
	}
}
