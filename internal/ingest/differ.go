package ingest

import (
	"sync"
	"time"

	transit "github.com/briankim06/urban-goggles/proto/transit"
)

const (
	// DefaultDelayChangeThresholdSeconds is the minimum absolute change in
	// delay_seconds required for the differ to re-emit an event for a
	// (trip_id, stop_id) pair. Anything under this is treated as noise.
	DefaultDelayChangeThresholdSeconds int32 = 15

	// DefaultPredictedChangeThresholdSeconds is the same threshold applied to
	// movement of the predicted arrival time. MTA feeds typically carry only
	// predicted times (delay stays 0), so prediction movement is the primary
	// change signal for them.
	DefaultPredictedChangeThresholdSeconds int64 = 15

	// DefaultStalenessWindow is how long a key may go unseen in the feed
	// before it is pruned. Trip IDs are unique per service day, so without
	// pruning the state grows unbounded.
	DefaultStalenessWindow = 30 * time.Minute
)

// diffState is the last committed observation for one (trip, stop) key.
type diffState struct {
	delay     int32
	predicted int64
	evType    transit.DelayEvent_EventType
	lastSeen  time.Time
}

// Differ keeps the most recently published observation for each
// (trip_id, stop_id) pair and selects only events that changed meaningfully
// since then. Selection (Diff) and state commit (Commit) are split so the
// poller can commit only after a successful publish — a failed publish leaves
// the key unchanged and the event re-candidates on the next poll.
//
// Differ is safe for concurrent use.
type Differ struct {
	thresholdSeconds   int32
	predictedThreshold int64
	staleness          time.Duration

	mu   sync.Mutex
	prev map[string]diffState
}

// NewDiffer constructs a Differ using the default thresholds.
func NewDiffer() *Differ {
	return &Differ{
		thresholdSeconds:   DefaultDelayChangeThresholdSeconds,
		predictedThreshold: DefaultPredictedChangeThresholdSeconds,
		staleness:          DefaultStalenessWindow,
		prev:               make(map[string]diffState),
	}
}

func diffKey(ev *transit.DelayEvent) string {
	return ev.GetTripId() + "|" + ev.GetStopId()
}

// Diff returns the events that changed meaningfully since their last
// committed observation: unseen keys, delay movement past the threshold,
// predicted-time movement past the threshold, or a change of event type. It
// does NOT commit new values — call Commit per event after it has been
// durably published. Present keys have their last-seen time refreshed so
// unchanged-but-live keys survive pruning.
func (d *Differ) Diff(events []*transit.DelayEvent) []*transit.DelayEvent {
	if len(events) == 0 {
		return nil
	}
	out := make([]*transit.DelayEvent, 0, len(events))
	now := time.Now()

	d.mu.Lock()
	defer d.mu.Unlock()
	for _, ev := range events {
		key := diffKey(ev)
		last, seen := d.prev[key]
		if seen {
			last.lastSeen = now
			d.prev[key] = last
		}
		switch {
		case !seen:
			out = append(out, ev)
		case ev.GetType() != last.evType:
			out = append(out, ev)
		case abs32(ev.GetDelaySeconds()-last.delay) > d.thresholdSeconds:
			out = append(out, ev)
		case abs64(ev.GetPredictedArrival()-last.predicted) > d.predictedThreshold:
			out = append(out, ev)
		}
	}
	return out
}

// Commit records an event's values as the last published observation for its
// key. Call only after the event has been successfully published.
func (d *Differ) Commit(ev *transit.DelayEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.prev[diffKey(ev)] = diffState{
		delay:     ev.GetDelaySeconds(),
		predicted: ev.GetPredictedArrival(),
		evType:    ev.GetType(),
		lastSeen:  time.Now(),
	}
}

// Prune evicts keys not seen in any feed snapshot for the staleness window
// (completed or long-gone trips) and returns how many were removed.
func (d *Differ) Prune(now time.Time) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for key, st := range d.prev {
		if now.Sub(st.lastSeen) > d.staleness {
			delete(d.prev, key)
			n++
		}
	}
	return n
}

func abs32(x int32) int32 {
	if x < 0 {
		return -x
	}
	return x
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
