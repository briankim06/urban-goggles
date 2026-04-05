package ingest

import "testing"

func TestParseRouteFromTripID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"130200_A..N", "A"},
		{"055150_6..N03R", "6"},
		{"094550_GS.N01R", "GS"},
		{"000000_L..S", "L"},
		{"noseparator", "noseparator"},
		{"", ""},
	}
	for _, c := range cases {
		got := ParseRouteFromTripID(c.in)
		if got != c.want {
			t.Errorf("ParseRouteFromTripID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
