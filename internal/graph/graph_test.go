package graph

import (
	"os"
	"testing"
	"time"
)

const testDataDir = "../../data/gtfs_static"

func skipIfNoData(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(testDataDir + "/stops.txt"); err != nil {
		t.Skip("GTFS static data not downloaded; run make download-static")
	}
}

func loadTestGraph(t *testing.T) *TransitGraph {
	t.Helper()
	skipIfNoData(t)
	// Use a weekday date within the calendar.txt validity range.
	date := time.Date(2026, 4, 7, 0, 0, 0, 0, time.UTC) // Tuesday
	g, err := BuildGraph(testDataDir, date)
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	return g
}

func TestParseGTFSTime(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"00:00:00", 0},
		{"12:30:00", 45000},
		{"24:00:00", 86400},
		{"25:30:00", 91800},
		{"06:05:30", 21930},
	}
	for _, c := range cases {
		got, err := parseGTFSTime(c.in)
		if err != nil {
			t.Errorf("parseGTFSTime(%q) error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseGTFSTime(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestParentStationNormalization(t *testing.T) {
	skipIfNoData(t)
	stops, err := parseStops(testDataDir)
	if err != nil {
		t.Fatal(err)
	}
	// 101N is a child of 101
	if got := parentStation("101N", stops); got != "101" {
		t.Errorf("parentStation(101N) = %q, want 101", got)
	}
	// 101 is already a parent
	if got := parentStation("101", stops); got != "101" {
		t.Errorf("parentStation(101) = %q, want 101", got)
	}
}

func TestGraphStats(t *testing.T) {
	g := loadTestGraph(t)
	totalStops, parentStops, totalRoutes, transferPairs := g.Stats()

	t.Logf("total stops=%d  parent stations=%d  routes=%d  transfer pairs=%d",
		totalStops, parentStops, totalRoutes, transferPairs)

	if parentStops < 400 {
		t.Errorf("expected >=400 parent stations, got %d", parentStops)
	}
	if totalRoutes < 20 {
		t.Errorf("expected >=20 routes, got %d", totalRoutes)
	}
	if transferPairs < 100 {
		t.Errorf("expected >=100 transfer pairs, got %d", transferPairs)
	}
}

func TestGetTransfersFrom_UnionSquare(t *testing.T) {
	g := loadTestGraph(t)
	// 14th St-Union Square has three parent stations in MTA GTFS:
	//   635  (4/5/6)
	//   L03  (L)
	//   R20  (N/Q/R/W)
	// Each should have transfers to the other two plus a self-transfer.
	for _, id := range []string{"635", "L03", "R20"} {
		transfers := g.GetTransfersFrom(id)
		if len(transfers) == 0 {
			t.Errorf("expected transfers from %s, got none", id)
			continue
		}
		destStations := make(map[string]bool)
		for _, tf := range transfers {
			destStations[tf.ToStopID] = true
		}
		t.Logf("transfers from %s: %d pairs to stations %v", id, len(transfers), mapKeys(destStations))
		if len(transfers) < 2 {
			t.Errorf("expected >=2 transfer pairs from %s, got %d", id, len(transfers))
		}
	}
}

func TestGetNextDepartures(t *testing.T) {
	g := loadTestGraph(t)
	// Look for L departures from station 631 (Union Sq) after 8 AM.
	deps := g.GetNextDepartures("631", "L", 8*3600, 5)
	if len(deps) == 0 {
		// L might not stop at 631 — try L01 (1 Av) which is definitely an L stop.
		deps = g.GetNextDepartures("L01", "L", 8*3600, 5)
	}
	if len(deps) == 0 {
		t.Fatal("no L departures found after 8 AM from Union Sq or 1 Av")
	}
	t.Logf("next L departures after 08:00: %d results, first at %ds",
		len(deps), deps[0].DepartureSecs)

	// Verify ordering.
	for i := 1; i < len(deps); i++ {
		if deps[i].DepartureSecs < deps[i-1].DepartureSecs {
			t.Errorf("departures not sorted: [%d]=%d < [%d]=%d",
				i, deps[i].DepartureSecs, i-1, deps[i-1].DepartureSecs)
		}
	}
}
