package graph

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
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

	// RoutesAtStop maps parent-station-ID → set of route IDs serving it.
	RoutesAtStop map[string]map[string]bool

	// activeTrips is the set of trip IDs active on the loaded date.
	activeTrips map[string]bool

	// headways maps route:direction:parentStation:hour → median scheduled
	// headway in seconds. Built by BuildHeadwayIndex.
	headways map[string]float64

	// tripSuffix maps the RT-shaped tail of a static trip ID (the substring
	// after the first underscore, e.g. "130200_A..N" from
	// "AFA25GEN-A048-Weekday-00_130200_A..N") to the full static trip ID.
	// Ambiguous suffixes map to "" and are treated as misses.
	tripSuffix map[string]string

	// tz is the agency's timezone from agency.txt — all schedule times are
	// local to it. Nil falls back to the process-local timezone.
	tz *time.Location
}

// Location returns the agency timezone the schedule is expressed in,
// defaulting to the process-local timezone when agency.txt was absent (or
// for hand-built graphs).
func (g *TransitGraph) Location() *time.Location {
	if g == nil || g.tz == nil {
		return time.Local
	}
	return g.tz
}

// SecsSinceMidnight converts a Unix timestamp to seconds since midnight in
// the agency's timezone, falling back to the current time when ts is zero.
// This is the only correct frame for comparing against GTFS schedule times.
func (g *TransitGraph) SecsSinceMidnight(ts int64) int {
	t := time.Now().In(g.Location())
	if ts > 0 {
		t = time.Unix(ts, 0).In(g.Location())
	}
	return t.Hour()*3600 + t.Minute()*60 + t.Second()
}

// BuildGraph loads all CSV files from dataDir, filters by services active on
// the given date, and builds every index the engine needs.
func BuildGraph(dataDir string, date time.Time) (*TransitGraph, error) {
	logger := slog.Default()

	// The agency timezone governs which service day "date" falls on and all
	// schedule-time comparisons — a UTC host would otherwise flip service
	// days at ~8pm ET and shift every time-of-day computation.
	loc := time.Local
	if tzName, err := parseAgencyTZ(dataDir); err != nil {
		logger.Warn("agency.txt unreadable, using process-local timezone", "err", err)
	} else if tzName != "" {
		if l, err := time.LoadLocation(tzName); err != nil {
			logger.Warn("invalid agency_timezone, using process-local timezone", "tz", tzName, "err", err)
		} else {
			loc = l
		}
	}
	date = date.In(loc)

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
	routesAtStop := make(map[string]map[string]bool)
	for _, st := range allStopTimes {
		if !activeTrips[st.TripID] {
			continue
		}
		parentID := parentStation(st.StopID, stops)
		byStop[parentID] = append(byStop[parentID], st)
		byTrip[st.TripID] = append(byTrip[st.TripID], st)
		if routesAtStop[parentID] == nil {
			routesAtStop[parentID] = make(map[string]bool)
		}
		routesAtStop[parentID][tripRoute[st.TripID]] = true
	}
	// Index active trips by their RT-shaped ID suffix for realtime→static
	// trip resolution.
	tripSuffix := make(map[string]string, len(byTrip))
	for tripID := range byTrip {
		if idx := strings.Index(tripID, "_"); idx >= 0 && idx < len(tripID)-1 {
			suffix := tripID[idx+1:]
			if _, dup := tripSuffix[suffix]; dup {
				tripSuffix[suffix] = "" // ambiguous → miss
			} else {
				tripSuffix[suffix] = tripID
			}
		}
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
		if tf.TransferType == 3 {
			continue // GTFS type 3: transfers not possible — never a connection
		}
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
		RoutesAtStop:    routesAtStop,
		activeTrips:     activeTrips,
		tripSuffix:      tripSuffix,
		tz:              loc,
	}
	g.BuildHeadwayIndex()

	logger.Info("graph built",
		"stops", len(stops),
		"routes", len(routes),
		"active_trips", len(activeTrips),
		"transfer_pairs", countTransferPairs(xferByStop),
		"stop_time_rows", len(allStopTimes),
		"tz", loc.String(),
		"service_date", date.Format("2006-01-02"),
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

// GetRoutesAtStop returns all route IDs that serve the given station.
func (g *TransitGraph) GetRoutesAtStop(stopID string) map[string]bool {
	return g.RoutesAtStop[parentStation(stopID, g.Stops)]
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

// overnightWindowSecs bounds the early-morning window in which a wall-clock
// time-of-day may belong to the previous service day's schedule (GTFS 24:xx+
// times, aligned with maxHeadwayHour = 27).
const overnightWindowSecs = 4 * 3600

// NextDeparturesAfter is the wrap-aware form of GetNextDepartures. A
// time-of-day in the early-morning window may belong to the previous
// service day's schedule (GTFS 24:xx+ times), so both representations are
// tried and the one with the smaller wait to its first departure wins.
// effectiveTod is the representation used — callers computing margins must
// use it, not the raw tod.
func (g *TransitGraph) NextDeparturesAfter(stopID, routeID string, tod, n int) (deps []*ScheduledStopTime, effectiveTod int) {
	deps, effectiveTod = g.GetNextDepartures(stopID, routeID, tod, n), tod
	if tod < overnightWindowSecs {
		wrapped := tod + 86400
		late := g.GetNextDepartures(stopID, routeID, wrapped, n)
		if len(late) > 0 && (len(deps) == 0 || late[0].DepartureSecs-wrapped < deps[0].DepartureSecs-tod) {
			return late, wrapped
		}
	}
	return deps, effectiveTod
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

// ResolveTripID maps a realtime trip ID to a static trip ID present in
// StopTimesByTrip: exact match first, then the suffix index (MTA RT trip IDs
// are the tail of the static ID). Ambiguous suffixes are misses.
func (g *TransitGraph) ResolveTripID(rtTripID string) (string, bool) {
	if rtTripID == "" {
		return "", false
	}
	if _, ok := g.StopTimesByTrip[rtTripID]; ok {
		return rtTripID, true
	}
	if full, ok := g.tripSuffix[rtTripID]; ok && full != "" {
		return full, true
	}
	return "", false
}

// GetScheduledStopTime returns the static stop_times row for the trip at the
// given stop. The stop is matched by raw ID first, then by parent station;
// the trip is resolved via ResolveTripID.
func (g *TransitGraph) GetScheduledStopTime(tripID, stopID string) (*ScheduledStopTime, bool) {
	resolved, ok := g.ResolveTripID(tripID)
	if !ok {
		return nil, false
	}
	sts := g.StopTimesByTrip[resolved]
	for _, st := range sts {
		if st.StopID == stopID {
			return st, true
		}
	}
	wantParent := parentStation(stopID, g.Stops)
	for _, st := range sts {
		if parentStation(st.StopID, g.Stops) == wantParent {
			return st, true
		}
	}
	return nil, false
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
