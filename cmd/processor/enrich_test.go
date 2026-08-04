package main

import (
	"testing"
	"time"

	"github.com/briankim06/urban-goggles/internal/graph"
	pb "github.com/briankim06/urban-goggles/proto/transit"
)

// enrichGraph schedules trip "t1" at stop "S1" (arrival 10:00:00, departure
// 10:00:30), trip "t2" at "S1" past midnight (25:00:00 = 01:00 next day), and
// trip "t3" at "S1" just before midnight (23:59:00).
func enrichGraph() *graph.TransitGraph {
	return &graph.TransitGraph{
		Stops: map[string]*graph.Stop{"S1": {ID: "S1", LocationType: 1}},
		StopTimesByTrip: map[string][]*graph.ScheduledStopTime{
			"t1": {{TripID: "t1", StopID: "S1", StopSequence: 1, ArrivalSecs: 36000, DepartureSecs: 36030}},
			"t2": {{TripID: "t2", StopID: "S1", StopSequence: 1, ArrivalSecs: 90000, DepartureSecs: 90000}},
			"t3": {{TripID: "t3", StopID: "S1", StopSequence: 1, ArrivalSecs: 86340, DepartureSecs: 86340}},
		},
	}
}

// localEpoch returns a Unix timestamp for the given local wall-clock time,
// keeping tests independent of the process timezone.
func localEpoch(hour, min, sec int) int64 {
	return time.Date(2026, 3, 10, hour, min, sec, 0, time.Local).Unix()
}

func TestEnrichEvent_ComputesDelay(t *testing.T) {
	g := enrichGraph()
	ev := &pb.DelayEvent{
		TripId: "t1", StopId: "S1",
		PredictedArrival: localEpoch(10, 2, 0), // scheduled 10:00:00 → 120s late
	}
	if !enrichEvent(g, ev) {
		t.Fatal("expected enrichment")
	}
	if ev.GetDelaySeconds() != 120 {
		t.Errorf("delay = %d, want 120", ev.GetDelaySeconds())
	}
	if ev.GetScheduledArrival() != ev.GetPredictedArrival()-120 {
		t.Errorf("scheduled = %d, want predicted-120", ev.GetScheduledArrival())
	}
}

func TestEnrichEvent_DepartureUsesDepartureSecs(t *testing.T) {
	g := enrichGraph()
	ev := &pb.DelayEvent{
		TripId: "t1", StopId: "S1",
		Type:             pb.DelayEvent_DEPARTURE_DELAY,
		PredictedArrival: localEpoch(10, 1, 30), // scheduled departure 10:00:30 → 60s late
	}
	if !enrichEvent(g, ev) {
		t.Fatal("expected enrichment")
	}
	if ev.GetDelaySeconds() != 60 {
		t.Errorf("delay = %d, want 60", ev.GetDelaySeconds())
	}
}

func TestEnrichEvent_PastMidnightWrap(t *testing.T) {
	g := enrichGraph()

	// Scheduled 25:00 (01:00 next service day), predicted 01:01 local:
	// raw = 3660 - 90000 = -86340 → +86400 candidate = 60.
	ev := &pb.DelayEvent{TripId: "t2", StopId: "S1", PredictedArrival: localEpoch(1, 1, 0)}
	if !enrichEvent(g, ev) {
		t.Fatal("expected enrichment (25:00 schedule)")
	}
	if ev.GetDelaySeconds() != 60 {
		t.Errorf("past-midnight delay = %d, want 60", ev.GetDelaySeconds())
	}

	// Scheduled 23:59:00, predicted 00:01: raw = 60 - 86340 = -86280 →
	// +86400 candidate = 120.
	ev = &pb.DelayEvent{TripId: "t3", StopId: "S1", PredictedArrival: localEpoch(0, 1, 0)}
	if !enrichEvent(g, ev) {
		t.Fatal("expected enrichment (23:59 schedule)")
	}
	if ev.GetDelaySeconds() != 120 {
		t.Errorf("midnight-wrap delay = %d, want 120", ev.GetDelaySeconds())
	}
}

func TestEnrichEvent_MissLeavesEventUntouched(t *testing.T) {
	g := enrichGraph()
	ev := &pb.DelayEvent{TripId: "unknown", StopId: "S1", PredictedArrival: localEpoch(10, 0, 0)}
	if enrichEvent(g, ev) {
		t.Fatal("unknown trip must not enrich")
	}
	if ev.GetDelaySeconds() != 0 || ev.GetScheduledArrival() != 0 {
		t.Errorf("event mutated on miss: %+v", ev)
	}
}

func TestEnrichEvent_SkipsEnrichedAndNonDelayEvents(t *testing.T) {
	g := enrichGraph()

	// Delay already known → untouched.
	ev := &pb.DelayEvent{TripId: "t1", StopId: "S1", DelaySeconds: 90, PredictedArrival: localEpoch(10, 2, 0)}
	if enrichEvent(g, ev) {
		t.Error("already-delayed event must not be re-enriched")
	}
	if ev.GetDelaySeconds() != 90 {
		t.Errorf("delay overwritten: %d", ev.GetDelaySeconds())
	}

	// Non-delay types → untouched.
	ev = &pb.DelayEvent{TripId: "t1", StopId: "S1", Type: pb.DelayEvent_TRIP_CANCELLED, PredictedArrival: localEpoch(10, 2, 0)}
	if enrichEvent(g, ev) {
		t.Error("TRIP_CANCELLED must not be enriched")
	}

	// No prediction → untouched.
	ev = &pb.DelayEvent{TripId: "t1", StopId: "S1"}
	if enrichEvent(g, ev) {
		t.Error("event without prediction must not be enriched")
	}
}
