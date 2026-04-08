package graph

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ---------- stops.txt ----------

func parseStops(dataDir string) (map[string]*Stop, error) {
	rows, hdr, err := readCSV(filepath.Join(dataDir, "stops.txt"))
	if err != nil {
		return nil, err
	}
	idx := colIndex(hdr, "stop_id", "stop_name", "stop_lat", "stop_lon", "location_type", "parent_station")

	stops := make(map[string]*Stop, len(rows))
	for _, r := range rows {
		lt, _ := strconv.Atoi(r[idx["location_type"]])
		lat, _ := strconv.ParseFloat(r[idx["stop_lat"]], 64)
		lon, _ := strconv.ParseFloat(r[idx["stop_lon"]], 64)
		stops[r[idx["stop_id"]]] = &Stop{
			ID:            r[idx["stop_id"]],
			Name:          r[idx["stop_name"]],
			Lat:           lat,
			Lon:           lon,
			LocationType:  lt,
			ParentStation: r[idx["parent_station"]],
		}
	}
	return stops, nil
}

// ---------- routes.txt ----------

func parseRoutes(dataDir string) (map[string]*Route, error) {
	rows, hdr, err := readCSV(filepath.Join(dataDir, "routes.txt"))
	if err != nil {
		return nil, err
	}
	idx := colIndex(hdr, "route_id", "route_short_name", "route_long_name", "route_color")

	routes := make(map[string]*Route, len(rows))
	for _, r := range rows {
		routes[r[idx["route_id"]]] = &Route{
			ID:        r[idx["route_id"]],
			ShortName: r[idx["route_short_name"]],
			LongName:  r[idx["route_long_name"]],
			Color:     r[idx["route_color"]],
		}
	}
	return routes, nil
}

// ---------- trips.txt ----------

func parseTrips(dataDir string) (map[string]*Trip, error) {
	rows, hdr, err := readCSV(filepath.Join(dataDir, "trips.txt"))
	if err != nil {
		return nil, err
	}
	idx := colIndex(hdr, "trip_id", "route_id", "service_id", "direction_id")

	trips := make(map[string]*Trip, len(rows))
	for _, r := range rows {
		dir, _ := strconv.Atoi(r[idx["direction_id"]])
		trips[r[idx["trip_id"]]] = &Trip{
			ID:          r[idx["trip_id"]],
			RouteID:     r[idx["route_id"]],
			ServiceID:   r[idx["service_id"]],
			DirectionID: dir,
		}
	}
	return trips, nil
}

// ---------- stop_times.txt ----------

func parseStopTimes(dataDir string) ([]*ScheduledStopTime, error) {
	rows, hdr, err := readCSV(filepath.Join(dataDir, "stop_times.txt"))
	if err != nil {
		return nil, err
	}
	idx := colIndex(hdr, "trip_id", "stop_id", "arrival_time", "departure_time", "stop_sequence")

	out := make([]*ScheduledStopTime, 0, len(rows))
	for _, r := range rows {
		arr, err := parseGTFSTime(r[idx["arrival_time"]])
		if err != nil {
			continue // skip malformed rows
		}
		dep, err := parseGTFSTime(r[idx["departure_time"]])
		if err != nil {
			dep = arr
		}
		seq, _ := strconv.Atoi(r[idx["stop_sequence"]])
		out = append(out, &ScheduledStopTime{
			TripID:        r[idx["trip_id"]],
			StopID:        r[idx["stop_id"]],
			ArrivalSecs:   arr,
			DepartureSecs: dep,
			StopSequence:  seq,
		})
	}
	return out, nil
}

// ---------- transfers.txt ----------

func parseTransfers(dataDir string) ([]*Transfer, error) {
	rows, hdr, err := readCSV(filepath.Join(dataDir, "transfers.txt"))
	if err != nil {
		return nil, err
	}
	idx := colIndex(hdr, "from_stop_id", "to_stop_id", "transfer_type", "min_transfer_time")

	out := make([]*Transfer, 0, len(rows))
	for _, r := range rows {
		tt, _ := strconv.Atoi(r[idx["transfer_type"]])
		mt, _ := strconv.Atoi(r[idx["min_transfer_time"]])
		out = append(out, &Transfer{
			FromStopID:      r[idx["from_stop_id"]],
			ToStopID:        r[idx["to_stop_id"]],
			TransferType:    tt,
			MinTransferTime: mt,
		})
	}
	return out, nil
}

// ---------- calendar.txt + calendar_dates.txt ----------

// activeServices returns the set of service_id strings that are active on the
// given date, according to calendar.txt and calendar_dates.txt.
func activeServices(dataDir string, date time.Time) (map[string]bool, error) {
	active := make(map[string]bool)

	// calendar.txt
	rows, hdr, err := readCSV(filepath.Join(dataDir, "calendar.txt"))
	if err != nil {
		return nil, err
	}
	dayCol := [7]string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}
	idx := colIndex(hdr, "service_id", "start_date", "end_date",
		"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday")

	dateStr := date.Format("20060102")
	wd := int(date.Weekday()) // 0=Sun..6=Sat
	// Map Go Weekday (0=Sun) to GTFS column order (0=Mon)
	gtfsDay := (wd + 6) % 7

	for _, r := range rows {
		start := r[idx["start_date"]]
		end := r[idx["end_date"]]
		if dateStr < start || dateStr > end {
			continue
		}
		if r[idx[dayCol[gtfsDay]]] == "1" {
			active[r[idx["service_id"]]] = true
		}
	}

	// calendar_dates.txt (overrides)
	cdPath := filepath.Join(dataDir, "calendar_dates.txt")
	if info, err := os.Stat(cdPath); err == nil && info.Size() > 0 {
		rows, hdr, err = readCSV(cdPath)
		if err != nil {
			return nil, err
		}
		idx = colIndex(hdr, "service_id", "date", "exception_type")
		for _, r := range rows {
			if r[idx["date"]] != dateStr {
				continue
			}
			sid := r[idx["service_id"]]
			switch r[idx["exception_type"]] {
			case "1": // service added
				active[sid] = true
			case "2": // service removed
				delete(active, sid)
			}
		}
	}
	return active, nil
}

// ---------- helpers ----------

// parseGTFSTime parses "HH:MM:SS" into seconds since midnight. Hours >=24 are
// valid in GTFS for past-midnight trips (e.g. 25:30:00 = 91800 seconds).
func parseGTFSTime(s string) (int, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 3 {
		return 0, fmt.Errorf("bad GTFS time %q", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, err
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, err
	}
	sec, err := strconv.Atoi(parts[2])
	if err != nil {
		return 0, err
	}
	return h*3600 + m*60 + sec, nil
}

// readCSV opens a file and returns all records plus the header row.
func readCSV(path string) ([][]string, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	r := csv.NewReader(f)
	r.LazyQuotes = true
	r.FieldsPerRecord = -1 // flexible column count

	hdr, err := r.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("read header %s: %w", path, err)
	}
	// Trim BOM if present
	if len(hdr) > 0 {
		hdr[0] = strings.TrimPrefix(hdr[0], "\xef\xbb\xbf")
	}

	padTo := len(hdr) + 1 // +1 so missing-column sentinel index is safe
	var rows [][]string
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			continue // skip malformed lines
		}
		for len(rec) < padTo {
			rec = append(rec, "")
		}
		rows = append(rows, rec)
	}
	return rows, hdr, nil
}

// colIndex builds a map of column-name → column-index from the header row.
// Requested names that are not found map to a sentinel index that will yield
// the empty string when accessed (by extending each record to max length).
func colIndex(hdr []string, names ...string) map[string]int {
	m := make(map[string]int, len(names))
	hdrMap := make(map[string]int, len(hdr))
	for i, h := range hdr {
		hdrMap[strings.TrimSpace(h)] = i
	}
	for _, n := range names {
		if i, ok := hdrMap[n]; ok {
			m[n] = i
		} else {
			m[n] = len(hdr) // will OOB — we handle below
		}
	}
	return m
}
