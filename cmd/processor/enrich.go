package main

import (
	"time"

	"github.com/briankim06/urban-goggles/internal/graph"
	"github.com/briankim06/urban-goggles/internal/metrics"
	pb "github.com/briankim06/urban-goggles/proto/transit"
)

// enrichEvent fills DelaySeconds and ScheduledArrival on a time-only event
// (delay 0, predicted set — the typical MTA feed shape) using the static
// schedule. A lookup miss leaves the event untouched, exactly as it flows
// today; the hit/miss counter makes trip-ID match health observable. Returns
// true if the event was enriched.
func enrichEvent(g *graph.TransitGraph, ev *pb.DelayEvent) bool {
	if ev.GetDelaySeconds() != 0 || ev.GetPredictedArrival() == 0 || ev.GetScheduledArrival() != 0 {
		return false
	}
	if t := ev.GetType(); t != pb.DelayEvent_ARRIVAL_DELAY && t != pb.DelayEvent_DEPARTURE_DELAY {
		return false
	}

	st, ok := g.GetScheduledStopTime(ev.GetTripId(), ev.GetStopId())
	if !ok {
		metrics.ProcessorEnrichment.WithLabelValues("miss").Inc()
		return false
	}

	schedSecs := st.ArrivalSecs
	if ev.GetType() == pb.DelayEvent_DEPARTURE_DELAY {
		schedSecs = st.DepartureSecs
	}
	if schedSecs == 0 {
		if schedSecs = st.ArrivalSecs; schedSecs == 0 {
			schedSecs = st.DepartureSecs
		}
	}
	if schedSecs == 0 {
		metrics.ProcessorEnrichment.WithLabelValues("miss").Inc()
		return false
	}

	// Predicted epoch → seconds since local midnight (process-local TZ, the
	// repo-wide convention). The schedule uses GTFS service-day seconds that
	// can exceed 86400 (past-midnight trips), and a prediction can fall on
	// the other side of midnight from its scheduled time — so pick the delay
	// candidate among {raw, raw±86400} with the smallest magnitude.
	pt := time.Unix(ev.GetPredictedArrival(), 0)
	predTod := int64(pt.Hour()*3600 + pt.Minute()*60 + pt.Second())
	raw := predTod - int64(schedSecs)
	best := raw
	for _, cand := range []int64{raw - 86400, raw + 86400} {
		if abs64(cand) < abs64(best) {
			best = cand
		}
	}

	ev.DelaySeconds = int32(best)
	ev.ScheduledArrival = ev.GetPredictedArrival() - best
	metrics.ProcessorEnrichment.WithLabelValues("hit").Inc()
	return true
}

func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
