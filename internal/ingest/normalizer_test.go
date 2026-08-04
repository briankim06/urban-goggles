package ingest

import (
	"testing"

	"google.golang.org/protobuf/proto"

	gtfsrt "github.com/briankim06/urban-goggles/proto/gtfsrt"
	transit "github.com/briankim06/urban-goggles/proto/transit"
)

func TestParseRouteFromTripID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"130200_A..N", "A"},
		{"055150_6..N03R", "6"},
		{"094550_GS.N01R", "GS"},
		{"000000_L..S", "L"},
		{"noseparator", "noseparator"},
		{"", ""},
	}
	for _, c := range cases {
		got := ParseRouteFromTripID(c.in)
		if got != c.want {
			t.Errorf("ParseRouteFromTripID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- gtfsrt fixtures (proto2: scalar fields are pointers) ---

func stuTime(stop string, arrivalTime int64) *gtfsrt.TripUpdate_StopTimeUpdate {
	return &gtfsrt.TripUpdate_StopTimeUpdate{
		StopId:  proto.String(stop),
		Arrival: &gtfsrt.TripUpdate_StopTimeEvent{Time: proto.Int64(arrivalTime)},
	}
}

func stuDelay(stop string, delay int32, predicted int64) *gtfsrt.TripUpdate_StopTimeUpdate {
	ev := &gtfsrt.TripUpdate_StopTimeEvent{Delay: proto.Int32(delay)}
	if predicted != 0 {
		ev.Time = proto.Int64(predicted)
	}
	return &gtfsrt.TripUpdate_StopTimeUpdate{
		StopId:  proto.String(stop),
		Arrival: ev,
	}
}

func tripUpdate(tripID string, rel *gtfsrt.TripDescriptor_ScheduleRelationship, stus ...*gtfsrt.TripUpdate_StopTimeUpdate) *gtfsrt.TripUpdate {
	trip := &gtfsrt.TripDescriptor{TripId: proto.String(tripID)}
	if rel != nil {
		trip.ScheduleRelationship = rel
	}
	return &gtfsrt.TripUpdate{Trip: trip, StopTimeUpdate: stus}
}

func TestNormalize_TimeOnlyArrival(t *testing.T) {
	tu := tripUpdate("130200_A..N", nil, stuTime("A01N", 1700000600))
	events := NormalizeTripUpdate("mta", 1700000000, tu)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.GetDelaySeconds() != 0 {
		t.Errorf("delay = %d, want 0", ev.GetDelaySeconds())
	}
	if ev.GetPredictedArrival() != 1700000600 {
		t.Errorf("predicted = %d, want 1700000600", ev.GetPredictedArrival())
	}
	if ev.GetScheduledArrival() != 0 {
		t.Errorf("scheduled = %d, want 0 (unknown here; processor enriches)", ev.GetScheduledArrival())
	}
	if ev.GetType() != transit.DelayEvent_ARRIVAL_DELAY {
		t.Errorf("type = %v, want ARRIVAL_DELAY", ev.GetType())
	}
	if ev.GetRouteId() != "A" {
		t.Errorf("route = %q, want A (parsed from trip ID)", ev.GetRouteId())
	}
}

func TestNormalize_DelayAndTime(t *testing.T) {
	tu := tripUpdate("t1", nil, stuDelay("S1", 120, 1700000600))
	events := NormalizeTripUpdate("mta", 1700000000, tu)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.GetDelaySeconds() != 120 {
		t.Errorf("delay = %d, want 120", ev.GetDelaySeconds())
	}
	if ev.GetScheduledArrival() != 1700000600-120 {
		t.Errorf("scheduled = %d, want %d", ev.GetScheduledArrival(), 1700000600-120)
	}
}

func TestNormalize_DelayOnlyNoTime(t *testing.T) {
	tu := tripUpdate("t1", nil, stuDelay("S1", 120, 0))
	events := NormalizeTripUpdate("mta", 1700000000, tu)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.GetDelaySeconds() != 120 {
		t.Errorf("delay = %d, want 120", ev.GetDelaySeconds())
	}
	// Regression: scheduled must not be fabricated as -delay.
	if ev.GetScheduledArrival() != 0 {
		t.Errorf("scheduled = %d, want 0", ev.GetScheduledArrival())
	}
	if ev.GetPredictedArrival() != 0 {
		t.Errorf("predicted = %d, want 0", ev.GetPredictedArrival())
	}
}

func TestNormalize_EmptyArrivalFallsBackToDeparture(t *testing.T) {
	stu := &gtfsrt.TripUpdate_StopTimeUpdate{
		StopId:    proto.String("S1"),
		Arrival:   &gtfsrt.TripUpdate_StopTimeEvent{}, // present but unusable
		Departure: &gtfsrt.TripUpdate_StopTimeEvent{Time: proto.Int64(1700000900)},
	}
	events := NormalizeTripUpdate("mta", 1700000000, tripUpdate("t1", nil, stu))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	ev := events[0]
	if ev.GetType() != transit.DelayEvent_DEPARTURE_DELAY {
		t.Errorf("type = %v, want DEPARTURE_DELAY", ev.GetType())
	}
	if ev.GetPredictedArrival() != 1700000900 {
		t.Errorf("predicted = %d, want departure time 1700000900", ev.GetPredictedArrival())
	}
}

func TestNormalize_SkippedStopEmitsWithoutTimes(t *testing.T) {
	stu := &gtfsrt.TripUpdate_StopTimeUpdate{
		StopId:               proto.String("S1"),
		ScheduleRelationship: gtfsrt.TripUpdate_StopTimeUpdate_SKIPPED.Enum(),
	}
	events := NormalizeTripUpdate("mta", 1700000000, tripUpdate("t1", nil, stu))
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1 (skipped stop must emit without time events)", len(events))
	}
	ev := events[0]
	if ev.GetType() != transit.DelayEvent_SKIP_STOP {
		t.Errorf("type = %v, want SKIP_STOP", ev.GetType())
	}
	if ev.GetStopId() != "S1" {
		t.Errorf("stop = %q, want S1", ev.GetStopId())
	}
}

func TestNormalize_CanceledTripEmitsSingleEvent(t *testing.T) {
	tu := tripUpdate("t1", gtfsrt.TripDescriptor_CANCELED.Enum(),
		stuTime("S1", 1700000600), stuTime("S2", 1700000700))
	events := NormalizeTripUpdate("mta", 1700000000, tu)
	if len(events) != 1 {
		t.Fatalf("got %d events, want exactly 1 trip-scoped cancellation", len(events))
	}
	ev := events[0]
	if ev.GetType() != transit.DelayEvent_TRIP_CANCELLED {
		t.Errorf("type = %v, want TRIP_CANCELLED", ev.GetType())
	}
	if ev.GetStopId() != "" {
		t.Errorf("stop = %q, want empty (trip-scoped)", ev.GetStopId())
	}
	if ev.GetTripId() != "t1" {
		t.Errorf("trip = %q, want t1", ev.GetTripId())
	}
}
