package graph

import "testing"

func buildResolveGraph() *TransitGraph {
	staticTrip := "AFA25GEN-A048-Weekday-00_130200_A..N"
	return &TransitGraph{
		Stops: map[string]*Stop{
			"S1":  {ID: "S1", LocationType: 1},
			"S1N": {ID: "S1N", ParentStation: "S1"},
			"S2":  {ID: "S2", LocationType: 1},
		},
		StopTimesByTrip: map[string][]*ScheduledStopTime{
			staticTrip: {
				{TripID: staticTrip, StopID: "S1N", StopSequence: 1, ArrivalSecs: 36000, DepartureSecs: 36030},
				{TripID: staticTrip, StopID: "S2", StopSequence: 2, ArrivalSecs: 36600, DepartureSecs: 36630},
			},
		},
		tripSuffix: map[string]string{
			"130200_A..N": staticTrip,
			"ambiguous":   "", // collision sentinel
		},
	}
}

func TestResolveTripID(t *testing.T) {
	g := buildResolveGraph()
	full := "AFA25GEN-A048-Weekday-00_130200_A..N"

	if got, ok := g.ResolveTripID(full); !ok || got != full {
		t.Errorf("exact match: got (%q, %v)", got, ok)
	}
	if got, ok := g.ResolveTripID("130200_A..N"); !ok || got != full {
		t.Errorf("suffix match: got (%q, %v)", got, ok)
	}
	if _, ok := g.ResolveTripID("ambiguous"); ok {
		t.Error("ambiguous suffix must be a miss")
	}
	if _, ok := g.ResolveTripID("unknown_trip"); ok {
		t.Error("unknown trip must be a miss")
	}
	if _, ok := g.ResolveTripID(""); ok {
		t.Error("empty trip must be a miss")
	}
}

func TestGetScheduledStopTime(t *testing.T) {
	g := buildResolveGraph()

	// Raw platform ID hit, via RT-shaped trip ID.
	st, ok := g.GetScheduledStopTime("130200_A..N", "S1N")
	if !ok || st.ArrivalSecs != 36000 {
		t.Fatalf("raw stop match: got (%+v, %v)", st, ok)
	}

	// Parent-station match: query with parent "S1", schedule holds child "S1N".
	st, ok = g.GetScheduledStopTime("130200_A..N", "S1")
	if !ok || st.ArrivalSecs != 36000 {
		t.Fatalf("parent stop match: got (%+v, %v)", st, ok)
	}

	// Unknown stop → miss.
	if _, ok := g.GetScheduledStopTime("130200_A..N", "S9"); ok {
		t.Error("unknown stop must be a miss")
	}
	// Unknown trip → miss.
	if _, ok := g.GetScheduledStopTime("nope", "S1"); ok {
		t.Error("unknown trip must be a miss")
	}
}
