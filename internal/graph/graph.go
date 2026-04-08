package graph

import (
	"fmt"
	"log/slog"
	"sort"
	"time"
)

// TransitGraph holds the parsed GTFS static schedule for a single date.
type TransitGraph struct {
	Stops  map[string]*Stop  // stop_id → Stop (includes parents + children)
	Routes map[string]*Route // route_id → Route

	// TripRoute maps trip_id → route_id.
	TripRoute map[string]string
	// TripDirection maps trip_id → direction_id.
	TripDirection map[string]int

	// StopTimesByStop maps parent-station-ID → sorted slice of stop times
	// (sorted by DepartureSecs).
	StopTimesByStop map[string][]*ScheduledStopTime
	// StopTimesByTrip maps trip_id → stop times in stop_sequence order.
	StopTimesByTrip map[string][]*ScheduledStopTime

	// TransfersByStop maps parent-station-ID → outbound transfers.
	TransfersByStop map[string][]*Transfer

	// activeTrips is the set of trip IDs active on the loaded date.
	activeTrips map[string]bool
}

// BuildGraph loads all CSV files from dataDir, filters by services active on
// the given date, and builds every index the engine needs.
func BuildGraph(dataDir string, date time.Time) (*TransitGraph, error) {
	logger := slog.Default()

	stops, err := parseStops(dataDir)
	if err != nil {
		return nil, fmt.Errorf("parse stops: %w", err)
	}
	routes, err := parseRoutes(dataDir)
	if err != nil {
		return nil, fmt.Errorf("parse routes: %w", err)
	}
	trips, err := parseTrips(dataDir)
	if err != nil {
		return nil, fmt.Errorf("parse trips: %w", err)
	}
	transfers, err := parseTransfers(dataDir)
	if err != nil {
		return nil, fmt.Errorf("parse transfers: %w", err)
	}
	active, err := activeServices(dataDir, date)
	if err != nil {
		return nil, fmt.Errorf("active services: %w", err)
	}
	logger.Info("active services", "date", date.Format("2006-01-02"), "services", mapKeys(active))

	allStopTimes, err := parseStopTimes(dataDir)
	if err != nil {
		return nil, fmt.Errorf("parse stop_times: %w", err)
	}

	// Filter trips to active service dates.
	activeTrips := make(map[string]bool, len(trips))
	tripRoute := make(map[string]string, len(trips))
	tripDir := make(map[string]int, len(trips))
	for _, t := range trips {
		tripRoute[t.ID] = t.RouteID
		tripDir[t.ID] = t.DirectionID
		if active[t.ServiceID] {
			activeTrips[t.ID] = true
		}
	}

	// Build per-stop and per-trip indices (only for active trips).
	byStop := make(map[string][]*ScheduledStopTime)
	byTrip := make(map[string][]*ScheduledStopTime)
	for _, st := range allStopTimes {
		if !activeTrips[st.TripID] {
			continue
		}
		parentID := parentStation(st.StopID, stops)
		byStop[parentID] = append(byStop[parentID], st)
		byTrip[st.TripID] = append(byTrip[st.TripID], st)
	}
	// Sort stop-level lists by departure time, trip-level by stop sequence.
	for _, v := range byStop {
		sort.Slice(v, func(i, j int) bool { return v[i].DepartureSecs < v[j].DepartureSecs })
	}
	for _, v := range byTrip {
		sort.Slice(v, func(i, j int) bool { return v[i].StopSequence < v[j].StopSequence })
	}

	// Build transfer index keyed by parent station.
	xferByStop := make(map[string][]*Transfer)
	for _, tf := range transfers {
		fromParent := parentStation(tf.FromStopID, stops)
		toParent := parentStation(tf.ToStopID, stops)
		tf := &Transfer{
			FromStopID:      fromParent,
			ToStopID:        toParent,
			TransferType:    tf.TransferType,
			MinTransferTime: tf.MinTransferTime,
		}
		xferByStop[fromParent] = append(xferByStop[fromParent], tf)
	}

	g := &TransitGraph{
		Stops:           stops,
		Routes:          routes,
		TripRoute:       tripRoute,
		TripDirection:   tripDir,
		StopTimesByStop: byStop,
		StopTimesByTrip: byTrip,
		TransfersByStop: xferByStop,
		activeTrips:     activeTrips,
	}

	logger.Info("graph built",
		"stops", len(stops),
		"routes", len(routes),
		"active_trips", len(activeTrips),
		"transfer_pairs", countTransferPairs(xferByStop),
		"stop_time_rows", len(allStopTimes),
	)
	return g, nil
}

// ParentStation returns the parent station ID for any stop. If the stop is
// already a parent (location_type=1) or has no parent, the stop's own ID is
// returned.
func parentStation(stopID string, stops map[string]*Stop) string {
	s, ok := stops[stopID]
	if !ok {
		return stopID
	}
	if s.ParentStation != "" {
		return s.ParentStation
	}
	return stopID
}

// ParentStationID is the exported version of parentStation for use by other
// packages once the graph is built.
func (g *TransitGraph) ParentStationID(stopID string) string {
	return parentStation(stopID, g.Stops)
}

// GetTransfersFrom returns all outbound transfers from a station. The
// supplied stopID may be a child platform — it is normalized to the parent.
func (g *TransitGraph) GetTransfersFrom(stopID string) []*Transfer {
	return g.TransfersByStop[parentStation(stopID, g.Stops)]
}

// GetNextDepartures returns up to n scheduled departures on routeID from
// stopID after afterSecs (seconds since midnight). Results are sorted by
// departure time.
func (g *TransitGraph) GetNextDepartures(stopID, routeID string, afterSecs, n int) []*ScheduledStopTime {
	parentID := parentStation(stopID, g.Stops)
	all := g.StopTimesByStop[parentID]
	// Binary search to the first departure >= afterSecs.
	start := sort.Search(len(all), func(i int) bool {
		return all[i].DepartureSecs >= afterSecs
	})

	var out []*ScheduledStopTime
	for i := start; i < len(all) && len(out) < n; i++ {
		st := all[i]
		if g.TripRoute[st.TripID] == routeID {
			out = append(out, st)
		}
	}
	return out
}

// GetDownstreamStops returns all stops after stopID on the same trip's route
// and direction. It finds the trip from the stop times and walks forward.
func (g *TransitGraph) GetDownstreamStops(routeID, stopID string, directionID int) []*ScheduledStopTime {
	parentID := parentStation(stopID, g.Stops)
	// Find a representative trip matching route + direction that visits this
	// station, then return everything after the matching stop.
	for _, st := range g.StopTimesByStop[parentID] {
		if g.TripRoute[st.TripID] != routeID {
			continue
		}
		if g.TripDirection[st.TripID] != directionID {
			continue
		}
		tripStops := g.StopTimesByTrip[st.TripID]
		for j, ts := range tripStops {
			tsParent := parentStation(ts.StopID, g.Stops)
			if tsParent == parentID && j+1 < len(tripStops) {
				return tripStops[j+1:]
			}
		}
	}
	return nil
}

// Stats returns human-readable statistics about the loaded graph.
func (g *TransitGraph) Stats() (totalStops, parentStops, totalRoutes, transferPairs int) {
	for _, s := range g.Stops {
		totalStops++
		if s.LocationType == 1 {
			parentStops++
		}
	}
	totalRoutes = len(g.Routes)
	transferPairs = countTransferPairs(g.TransfersByStop)
	return
}

func countTransferPairs(m map[string][]*Transfer) int {
	n := 0
	for _, v := range m {
		n += len(v)
	}
	return n
}

func mapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
