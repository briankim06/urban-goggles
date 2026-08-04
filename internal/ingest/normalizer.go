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

	// A cancelled trip usually carries no StopTimeUpdates. Emit a single
	// trip-scoped event (empty StopId) so downstream can retract the trip's
	// delay state.
	if trip.GetScheduleRelationship() == gtfsrt.TripDescriptor_CANCELED {
		return []*transit.DelayEvent{{
			AgencyId:   agencyID,
			TripId:     tripID,
			RouteId:    routeID,
			ObservedAt: observedAt,
			Type:       transit.DelayEvent_TRIP_CANCELLED,
		}}
	}

	events := make([]*transit.DelayEvent, 0, len(tu.GetStopTimeUpdate()))
	for _, stu := range tu.GetStopTimeUpdate() {
		// A skipped stop usually carries no arrival/departure events, so it
		// must be detected before requiring a usable time or delay.
		if stu.GetScheduleRelationship() == gtfsrt.TripUpdate_StopTimeUpdate_SKIPPED {
			_, predicted, scheduled, _, _ := extractDelay(stu)
			events = append(events, &transit.DelayEvent{
				AgencyId:         agencyID,
				TripId:           tripID,
				RouteId:          routeID,
				StopId:           stu.GetStopId(),
				ObservedAt:       observedAt,
				ScheduledArrival: scheduled,
				PredictedArrival: predicted,
				Type:             transit.DelayEvent_SKIP_STOP,
			})
			continue
		}

		delay, predicted, scheduled, fromDeparture, ok := extractDelay(stu)
		if !ok {
			continue
		}
		evType := transit.DelayEvent_ARRIVAL_DELAY
		if fromDeparture {
			evType = transit.DelayEvent_DEPARTURE_DELAY
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

// extractDelay pulls (delay_seconds, predicted_unix, scheduled_unix) out of a
// StopTimeUpdate. Prefers a usable arrival event — one carrying a delay or a
// time — falling back to the departure when the arrival is absent or empty;
// fromDeparture reports which was used. MTA feeds typically set `time` but
// not `delay`; such events carry delay 0 and the processor derives the real
// delay from the static schedule. If neither event is usable, ok=false.
func extractDelay(stu *gtfsrt.TripUpdate_StopTimeUpdate) (delay int32, predicted, scheduled int64, fromDeparture, ok bool) {
	usable := func(ev *gtfsrt.TripUpdate_StopTimeEvent) bool {
		return ev != nil && (ev.Delay != nil || ev.GetTime() != 0)
	}

	ev := stu.GetArrival()
	if !usable(ev) {
		ev = stu.GetDeparture()
		fromDeparture = true
	}
	if !usable(ev) {
		return 0, 0, 0, false, false
	}

	predicted = ev.GetTime()
	if ev.Delay != nil {
		delay = ev.GetDelay()
		if predicted > 0 {
			scheduled = predicted - int64(delay)
		}
		return delay, predicted, scheduled, fromDeparture, true
	}
	return 0, predicted, 0, fromDeparture, true
}
