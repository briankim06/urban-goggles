package graph

import (
	"fmt"
	"sort"
)

const (
	// maxHeadwayGapSecs excludes service-break gaps (e.g. overnight) from the
	// headway calculation.
	maxHeadwayGapSecs = 3 * 3600
	// maxHeadwayHour covers past-midnight departures (DepartureSecs > 86400
	// bucket into hours 24-27).
	maxHeadwayHour = 27
)

func headwayKey(routeID string, directionID int, stationID string, hour int) string {
	return fmt.Sprintf("%s:%d:%s:%d", routeID, directionID, stationID, hour)
}

// BuildHeadwayIndex computes the median scheduled headway for every
// (route, direction, parent station, hour-of-day) bucket from the stop-time
// indices. BuildGraph calls this automatically; hand-built graphs (tests)
// must call it explicitly after populating StopTimesByStop, TripRoute, and
// TripDirection.
func (g *TransitGraph) BuildHeadwayIndex() {
	g.headways = make(map[string]float64)

	// Group departures per (route, direction) within each station.
	for stationID, stopTimes := range g.StopTimesByStop {
		deps := make(map[string][]int) // "route:dir" → departure secs (sorted, input already sorted)
		for _, st := range stopTimes {
			routeID, ok := g.TripRoute[st.TripID]
			if !ok {
				continue
			}
			rd := fmt.Sprintf("%s:%d", routeID, g.TripDirection[st.TripID])
			deps[rd] = append(deps[rd], st.DepartureSecs)
		}

		for rd, times := range deps {
			// Bucket consecutive-departure gaps by the hour of the earlier
			// departure.
			gaps := make(map[int][]int)
			for i := 1; i < len(times); i++ {
				gap := times[i] - times[i-1]
				if gap <= 0 || gap > maxHeadwayGapSecs {
					continue
				}
				hour := times[i-1] / 3600
				if hour > maxHeadwayHour {
					continue
				}
				gaps[hour] = append(gaps[hour], gap)
			}
			for hour, hourGaps := range gaps {
				sort.Ints(hourGaps)
				g.headways[rd+":"+fmt.Sprintf("%s:%d", stationID, hour)] = median(hourGaps)
			}
		}
	}
}

// GetScheduledHeadway returns the median scheduled headway in seconds for the
// route/direction at the given station during the hour containing
// secsSinceMidnight. ok is false when the index is absent or the bucket has
// no computable headway (fewer than two departures in that hour).
func (g *TransitGraph) GetScheduledHeadway(routeID string, directionID int, stopID string, secsSinceMidnight int) (float64, bool) {
	if g == nil || g.headways == nil {
		return 0, false
	}
	stationID := parentStation(stopID, g.Stops)
	hour := secsSinceMidnight / 3600
	if hour < 0 || hour > maxHeadwayHour {
		return 0, false
	}
	h, ok := g.headways[headwayKey(routeID, directionID, stationID, hour)]
	return h, ok
}

func median(sorted []int) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return float64(sorted[n/2])
	}
	return float64(sorted[n/2-1]+sorted[n/2]) / 2
}
