package graph

import "testing"

// buildOvernightGraph schedules route L at S1 with a morning trip at 05:00
// (18000s) and an overnight trip at 24:35 (88500s, GTFS past-midnight time).
func buildOvernightGraph() *TransitGraph {
	return &TransitGraph{
		Stops: map[string]*Stop{"S1": {ID: "S1", LocationType: 1}},
		TripRoute: map[string]string{
			"morning":   "L",
			"overnight": "L",
		},
		TripDirection: map[string]int{"morning": 0, "overnight": 0},
		StopTimesByStop: map[string][]*ScheduledStopTime{
			"S1": {
				{TripID: "morning", StopID: "S1", DepartureSecs: 18000},
				{TripID: "overnight", StopID: "S1", DepartureSecs: 88500},
			},
		},
	}
}

func TestNextDeparturesAfter_MidnightWrap(t *testing.T) {
	g := buildOvernightGraph()

	// 00:30 (tod 1800): the overnight 24:35 trip is 300s away in the wrapped
	// frame (88200), which beats the morning trip's 16200s wait.
	deps, effTod := g.NextDeparturesAfter("S1", "L", 1800, 1)
	if len(deps) != 1 || deps[0].TripID != "overnight" {
		t.Fatalf("00:30 query: got %v, want the overnight trip", deps)
	}
	if effTod != 1800+86400 {
		t.Errorf("effectiveTod = %d, want %d (wrapped frame)", effTod, 1800+86400)
	}
	if wait := deps[0].DepartureSecs - effTod; wait != 300 {
		t.Errorf("wait = %d, want 300", wait)
	}

	// 08:20 (tod 30000): outside the overnight window — unwrapped behavior,
	// next is the overnight trip's raw slot? No: first departure >= 30000 is
	// 88500 in the raw frame, which is what the old behavior returned too.
	deps, effTod = g.NextDeparturesAfter("S1", "L", 30000, 1)
	if effTod != 30000 {
		t.Errorf("daytime query must not wrap: effTod = %d", effTod)
	}
	if len(deps) != 1 || deps[0].DepartureSecs != 88500 {
		t.Errorf("daytime query: got %v", deps)
	}

	// 00:30 with no overnight service → falls back to the morning trip in
	// the raw frame.
	g2 := buildOvernightGraph()
	g2.StopTimesByStop["S1"] = g2.StopTimesByStop["S1"][:1] // morning only
	deps, effTod = g2.NextDeparturesAfter("S1", "L", 1800, 1)
	if len(deps) != 1 || deps[0].TripID != "morning" || effTod != 1800 {
		t.Errorf("no-overnight fallback: got (%v, %d), want morning trip at effTod 1800", deps, effTod)
	}
}
