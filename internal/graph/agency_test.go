package graph

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeAgencyFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agency.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestParseAgencyTZ(t *testing.T) {
	dir := writeAgencyFile(t, "agency_id,agency_name,agency_url,agency_timezone\nMTA,Metropolitan Transportation Authority,http://www.mta.info,America/New_York\n")
	tz, err := parseAgencyTZ(dir)
	if err != nil {
		t.Fatal(err)
	}
	if tz != "America/New_York" {
		t.Errorf("tz = %q, want America/New_York", tz)
	}

	// Missing file → empty, no error (fallback path).
	tz, err = parseAgencyTZ(t.TempDir())
	if err != nil || tz != "" {
		t.Errorf("missing agency.txt: got (%q, %v), want (\"\", nil)", tz, err)
	}

	// Missing column → empty.
	dir = writeAgencyFile(t, "agency_id,agency_name\nMTA,MTA\n")
	if tz, _ = parseAgencyTZ(dir); tz != "" {
		t.Errorf("missing column: got %q, want empty", tz)
	}
}

func TestLocation_Fallback(t *testing.T) {
	// Hand-built graphs (and a nil graph) fall back to process-local time.
	var g *TransitGraph
	if g.Location() != time.Local {
		t.Error("nil graph must fall back to time.Local")
	}
	if (&TransitGraph{}).Location() != time.Local {
		t.Error("graph without tz must fall back to time.Local")
	}
}

func TestSecsSinceMidnight_AgencyTZ(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	g := &TransitGraph{tz: ny}

	// 08:30:00 in New York, regardless of the process timezone.
	ts := time.Date(2026, 1, 15, 8, 30, 0, 0, ny).Unix()
	if got := g.SecsSinceMidnight(ts); got != 8*3600+30*60 {
		t.Errorf("SecsSinceMidnight = %d, want %d", got, 8*3600+30*60)
	}

	// The same instant is 13:30 UTC — a UTC-local process would previously
	// have computed 48600s instead of 30600s.
	utcView := time.Unix(ts, 0).UTC()
	if utcView.Hour() == 8 {
		t.Skip("process TZ happens to match agency TZ; cross-check not meaningful")
	}
}
