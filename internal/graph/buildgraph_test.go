package graph

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeGTFSFixture writes a minimal but complete GTFS static directory:
// one station pair, one route, one trip, a calendar valid for the given
// date, and two transfers — one real (type 2) and one "not possible"
// (type 3).
func writeGTFSFixture(t *testing.T, date time.Time) string {
	t.Helper()
	dir := t.TempDir()
	day := date.Format("20060102")
	files := map[string]string{
		"agency.txt": "agency_id,agency_name,agency_url,agency_timezone\nTST,Test,http://x,America/New_York\n",
		"stops.txt":  "stop_id,stop_name,stop_lat,stop_lon,location_type,parent_station\nS1,First,0,0,1,\nS2,Second,0,0,1,\n",
		"routes.txt": "route_id,route_short_name,route_long_name,route_color\nL,L,Test Line,808183\n",
		"trips.txt":  "route_id,service_id,trip_id,direction_id\nL,WK,t1,0\n",
		"calendar.txt": "service_id,monday,tuesday,wednesday,thursday,friday,saturday,sunday,start_date,end_date\n" +
			"WK,1,1,1,1,1,1,1," + day + "," + day + "\n",
		"stop_times.txt": "trip_id,arrival_time,departure_time,stop_id,stop_sequence\nt1,08:00:00,08:00:00,S1,1\nt1,08:05:00,08:05:00,S2,2\n",
		"transfers.txt": "from_stop_id,to_stop_id,transfer_type,min_transfer_time\n" +
			"S1,S2,2,120\n" +
			"S2,S1,3,\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestBuildGraph_FiltersImpossibleTransfers(t *testing.T) {
	date := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	g, err := BuildGraph(writeGTFSFixture(t, date), date)
	if err != nil {
		t.Fatal(err)
	}

	// The type-2 transfer survives; the type-3 ("not possible") is dropped.
	if xf := g.GetTransfersFrom("S1"); len(xf) != 1 || xf[0].ToStopID != "S2" {
		t.Errorf("S1 transfers = %v, want the single type-2 S1→S2", xf)
	}
	if xf := g.GetTransfersFrom("S2"); len(xf) != 0 {
		t.Errorf("S2 transfers = %v, want none (type 3 must be filtered)", xf)
	}

	// Sanity: the fixture built a real graph with the agency TZ applied.
	if g.Location().String() != "America/New_York" {
		t.Errorf("tz = %s, want America/New_York", g.Location())
	}
	if len(g.StopTimesByTrip["t1"]) != 2 {
		t.Errorf("trip t1 has %d stop times, want 2", len(g.StopTimesByTrip["t1"]))
	}
}
