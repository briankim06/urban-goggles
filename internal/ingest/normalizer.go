package ingest

import (
	"strings"

	gtfsrt "github.com/briankim06/urban-goggles/proto/gtfsrt"
	transit "github.com/briankim06/urban-goggles/proto/transit"
)

// ParseRouteFromTripID extracts the route identifier from an MTA NYCT trip_id.
//
// MTA encodes the route after the first underscore and before a trailing ".."
// or directional suffix. Examples:
//
//	"130200_A..N"     -> "A"
//	"055150_6..N03R"  -> "6"
//	"094550_GS.N01R"  -> "GS"
//
// If the format is unrecognized, the original trip_id is returned unchanged so
// the caller can fall back to any route hint it has.
func ParseRouteFromTripID(tripID string) string {
	if tripID == "" {
		return ""
	}
	idx := strings.Index(tripID, "_")
	if idx < 0 || idx == len(tripID)-1 {
		return tripID
	}
	tail := tripID[idx+1:]
	// Strip everything from the first '.' onward (directional/run suffix).
	if dot := strings.Index(tail, "."); dot >= 0 {
		tail = tail[:dot]
	}
	return tail
}

// NormalizeTripUpdate converts a single GTFS-RT TripUpdate into a slice of
// internal DelayEvent records, one per StopTimeUpdate that carries a delay or
// predicted time. agencyID and observedAt are stamped onto every event.
func NormalizeTripUpdate(
	agencyID string,
	observedAt int64,
	tu *gtfsrt.TripUpdate,
) []*transit.DelayEvent {
	if tu == nil || tu.GetTrip() == nil {
		return nil
	}
	trip := tu.GetTrip()
	tripID := trip.GetTripId()
	routeID := trip.GetRouteId()
	if routeID == "" {
		routeID = ParseRouteFromTripID(tripID)
	}

	events := make([]*transit.DelayEvent, 0, len(tu.GetStopTimeUpdate()))
	for _, stu := range tu.GetStopTimeUpdate() {
		delay, predicted, scheduled, ok := extractDelay(stu)
		if !ok {
			continue
		}
		evType := transit.DelayEvent_ARRIVAL_DELAY
		if stu.GetArrival() == nil && stu.GetDeparture() != nil {
			evType = transit.DelayEvent_DEPARTURE_DELAY
		}
		if stu.GetScheduleRelationship() == gtfsrt.TripUpdate_StopTimeUpdate_SKIPPED {
			evType = transit.DelayEvent_SKIP_STOP
		}
		events = append(events, &transit.DelayEvent{
			AgencyId:         agencyID,
			TripId:           tripID,
			RouteId:          routeID,
			StopId:           stu.GetStopId(),
			DelaySeconds:     delay,
			ObservedAt:       observedAt,
			ScheduledArrival: scheduled,
			PredictedArrival: predicted,
			Type:             evType,
		})
	}
	return events
}

// extractDelay pulls (delay_seconds, predicted_unix, scheduled_unix, ok) out of
// a StopTimeUpdate. Prefers the arrival event, falling back to departure.
// MTA feeds typically set `time` but not `delay`, so we compute delay as
// (predicted - scheduled) when possible. If neither field is usable, ok=false.
func extractDelay(stu *gtfsrt.TripUpdate_StopTimeUpdate) (delay int32, predicted, scheduled int64, ok bool) {
	ev := stu.GetArrival()
	if ev == nil {
		ev = stu.GetDeparture()
	}
	if ev == nil {
		return 0, 0, 0, false
	}
	predicted = ev.GetTime()
	if ev.Delay != nil {
		delay = ev.GetDelay()
		scheduled = predicted - int64(delay)
		return delay, predicted, scheduled, true
	}
	// No explicit delay — we don't know the static schedule here, so report
	// a predicted time with zero delay. Step 4 (graph) will enable real
	// scheduled-vs-predicted subtraction; for now this still lets the diff
	// layer detect changes over time.
	if predicted == 0 {
		return 0, 0, 0, false
	}
	return 0, predicted, 0, true
}
