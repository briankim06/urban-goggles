package graph

import (
	"fmt"
	"testing"
)

// buildHeadwayGraph creates a minimal graph with two directions of route "L"
// at station "S1": direction 0 every 600s, direction 1 every 300s, all within
// the 08:00 hour.
func buildHeadwayGraph() *TransitGraph {
	g := &TransitGraph{
		Stops:           map[string]*Stop{"S1": {ID: "S1", Name: "First", LocationType: 1}},
		TripRoute:       map[string]string{},
		TripDirection:   map[string]int{},
		StopTimesByStop: map[string][]*ScheduledStopTime{},
	}

	base := 8 * 3600
	var times []*ScheduledStopTime
	for i := 0; i < 4; i++ {
		trip := fmt.Sprintf("L0-%d", i)
		g.TripRoute[trip] = "L"
		g.TripDirection[trip] = 0
		times = append(times, &ScheduledStopTime{TripID: trip, StopID: "S1", DepartureSecs: base + i*600})
	}
	for i := 0; i < 5; i++ {
		trip := fmt.Sprintf("L1-%d", i)
		g.TripRoute[trip] = "L"
		g.TripDirection[trip] = 1
		times = append(times, &ScheduledStopTime{TripID: trip, StopID: "S1", DepartureSecs: base + i*300})
	}
	g.StopTimesByStop["S1"] = times
	return g
}

func TestHeadway_DirectionSeparation(t *testing.T) {
	g := buildHeadwayGraph()
	g.BuildHeadwayIndex()

	h0, ok := g.GetScheduledHeadway("L", 0, "S1", 8*3600+100)
	if !ok || h0 != 600 {
		t.Fatalf("direction 0: got (%v, %v), want (600, true)", h0, ok)
	}
	h1, ok := g.GetScheduledHeadway("L", 1, "S1", 8*3600+100)
	if !ok || h1 != 300 {
		t.Fatalf("direction 1: got (%v, %v), want (300, true)", h1, ok)
	}
}

func TestHeadway_EmptyBucket(t *testing.T) {
	g := buildHeadwayGraph()
	g.BuildHeadwayIndex()

	if _, ok := g.GetScheduledHeadway("L", 0, "S1", 14*3600); ok {
		t.Fatal("expected miss for hour with no departures")
	}
	if _, ok := g.GetScheduledHeadway("Q", 0, "S1", 8*3600); ok {
		t.Fatal("expected miss for unknown route")
	}
}

func TestHeadway_NilIndex(t *testing.T) {
	g := buildHeadwayGraph() // BuildHeadwayIndex NOT called
	if _, ok := g.GetScheduledHeadway("L", 0, "S1", 8*3600); ok {
		t.Fatal("expected miss on nil index")
	}
}

func TestHeadway_LongGapExcluded(t *testing.T) {
	g := &TransitGraph{
		Stops:         map[string]*Stop{"S1": {ID: "S1", LocationType: 1}},
		TripRoute:     map[string]string{"a": "L", "b": "L"},
		TripDirection: map[string]int{"a": 0, "b": 0},
		StopTimesByStop: map[string][]*ScheduledStopTime{
			"S1": {
				{TripID: "a", StopID: "S1", DepartureSecs: 8 * 3600},
				{TripID: "b", StopID: "S1", DepartureSecs: 8*3600 + 4*3600}, // 4h gap
			},
		},
	}
	g.BuildHeadwayIndex()
	if _, ok := g.GetScheduledHeadway("L", 0, "S1", 8*3600); ok {
		t.Fatal("expected 4h gap to be excluded from headway")
	}
}

func TestHeadway_PastMidnight(t *testing.T) {
	g := &TransitGraph{
		Stops:         map[string]*Stop{"S1": {ID: "S1", LocationType: 1}},
		TripRoute:     map[string]string{"a": "L", "b": "L", "c": "L"},
		TripDirection: map[string]int{"a": 0, "b": 0, "c": 0},
		StopTimesByStop: map[string][]*ScheduledStopTime{
			"S1": {
				{TripID: "a", StopID: "S1", DepartureSecs: 25 * 3600}, // 01:00 next day
				{TripID: "b", StopID: "S1", DepartureSecs: 25*3600 + 1200},
				{TripID: "c", StopID: "S1", DepartureSecs: 25*3600 + 2400},
			},
		},
	}
	g.BuildHeadwayIndex()
	h, ok := g.GetScheduledHeadway("L", 0, "S1", 25*3600+60)
	if !ok || h != 1200 {
		t.Fatalf("past-midnight bucket: got (%v, %v), want (1200, true)", h, ok)
	}
}

func TestHeadway_ChildStopNormalized(t *testing.T) {
	g := buildHeadwayGraph()
	g.Stops["S1N"] = &Stop{ID: "S1N", ParentStation: "S1"}
	g.BuildHeadwayIndex()
	h, ok := g.GetScheduledHeadway("L", 0, "S1N", 8*3600)
	if !ok || h != 600 {
		t.Fatalf("child stop lookup: got (%v, %v), want (600, true)", h, ok)
	}
}
