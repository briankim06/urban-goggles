package ingest

import (
	"sync"

	transit "github.com/briankim06/urban-goggles/proto/transit"
)

// DefaultDelayChangeThresholdSeconds is the minimum absolute change in
// delay_seconds required for the differ to re-emit an event for a
// (trip_id, stop_id) pair. Anything under this is treated as noise.
const DefaultDelayChangeThresholdSeconds int32 = 15

// Differ keeps the most recently observed delay for each (trip_id, stop_id)
// pair and returns only events whose delay has shifted by more than the
// configured threshold since the last observation.
//
// On the very first call for a given key, every event is emitted regardless
// of its current value so downstream consumers see an initial snapshot.
//
// Differ is safe for concurrent use because a single Poller may be invoked
// from a ticker goroutine while draining a shutdown signal on another.
type Differ struct {
	thresholdSeconds int32

	mu   sync.Mutex
	prev map[string]int32 // key: tripID|stopID -> last delay_seconds
}

// NewDiffer constructs a Differ using the default 15-second threshold.
func NewDiffer() *Differ {
	return &Differ{
		thresholdSeconds: DefaultDelayChangeThresholdSeconds,
		prev:             make(map[string]int32),
	}
}

// Diff takes the events normalized from the most recent feed poll and returns
// only those whose delay has changed meaningfully since the last observation.
// The internal state is updated in place.
func (d *Differ) Diff(events []*transit.DelayEvent) []*transit.DelayEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]*transit.DelayEvent, 0, len(events))

	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ev := range events {
		key := ev.GetTripId() + "|" + ev.GetStopId()
		last, seen := d.prev[key]
		if !seen {
			d.prev[key] = ev.GetDelaySeconds()
			out = append(out, ev)
			continue
		}
		if abs32(ev.GetDelaySeconds()-last) > d.thresholdSeconds {
			d.prev[key] = ev.GetDelaySeconds()
			out = append(out, ev)
		}
	}
	return out
}

func abs32(x int32) int32 {
	if x < 0 {
		return -x
	}
	return x
}
