package transfer

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/briankim06/urban-goggles/internal/graph"
	"github.com/briankim06/urban-goggles/internal/state"
	pb "github.com/briankim06/urban-goggles/proto/transit"
)

// unixAt returns a Unix timestamp for HH:MM:SS today in local time, matching
// what todFromUnix will decode back to seconds-since-midnight.
func unixAt(hour, min, sec int) int64 {
	now := time.Now()
	t := time.Date(now.Year(), now.Month(), now.Day(), hour, min, sec, 0, time.Local)
	return t.Unix()
}

// buildTestGraph creates a minimal synthetic graph with:
//   - Station "S1" served by routes A and L
//   - Transfer from S1 to S1 (intra-station cross-route)
//   - Route A trip departing S1 at 08:00 (28800s)
//   - Route L trip departing S1 at 08:03 (28980s) — 3 min headway
//   - Route L next trip departing S1 at 08:06 (29160s)
//   - MinTransferTime = 120s
func buildTestGraph() *graph.TransitGraph {
	stops := map[string]*graph.Stop{
		"S1": {ID: "S1", Name: "Test Station", LocationType: 1},
	}
	routes := map[string]*graph.Route{
		"A": {ID: "A", ShortName: "A"},
		"L": {ID: "L", ShortName: "L"},
	}
	tripRoute := map[string]string{
		"trip_A1": "A",
		"trip_L1": "L",
		"trip_L2": "L",
	}
	tripDir := map[string]int{
		"trip_A1": 0,
		"trip_L1": 0,
		"trip_L2": 0,
	}
	stopTimes := map[string][]*graph.ScheduledStopTime{
		"S1": {
			{TripID: "trip_A1", StopID: "S1", ArrivalSecs: 28800, DepartureSecs: 28800, StopSequence: 1},
			{TripID: "trip_L1", StopID: "S1", ArrivalSecs: 28980, DepartureSecs: 28980, StopSequence: 1},
			{TripID: "trip_L2", StopID: "S1", ArrivalSecs: 29160, DepartureSecs: 29160, StopSequence: 1},
		},
	}
	// Per-trip index, needed by tripDelayAt's GetScheduledStopTime lookup.
	stopTimesByTrip := map[string][]*graph.ScheduledStopTime{}
	for _, st := range stopTimes["S1"] {
		stopTimesByTrip[st.TripID] = []*graph.ScheduledStopTime{st}
	}
	transfers := map[string][]*graph.Transfer{
		"S1": {
			{FromStopID: "S1", ToStopID: "S1", TransferType: 2, MinTransferTime: 120},
		},
	}
	routesAtStop := map[string]map[string]bool{
		"S1": {"A": true, "L": true},
	}

	return &graph.TransitGraph{
		Stops:           stops,
		Routes:          routes,
		TripRoute:       tripRoute,
		TripDirection:   tripDir,
		StopTimesByStop: stopTimes,
		StopTimesByTrip: stopTimesByTrip,
		TransfersByStop: transfers,
		RoutesAtStop:    routesAtStop,
	}
}

func skipIfNoRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("Redis not available: %v", err)
	}
	return rdb
}

func TestEvaluateDelay_BrokenTransfer(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	g := buildTestGraph()
	mgr := state.NewDelayStateManager(rdb, g, nil)
	det := NewTransferDetector(g, mgr, nil, nil)

	// A train arrives at S1 scheduled at 08:00 (28800s) with 120s delay.
	// Predicted arrival = 08:02 (28920s).
	// Next L departs at 08:03 (28980s). Original margin = 180s.
	// Remaining margin = 180 - 120 = 60s. MinTransferTime = 120s.
	// 60 < 120 → BROKEN.
	// Next viable L after 28920+120=29040 → trip_L2 at 29160.
	// additional_wait = 29160 - 28980 = 180s.
	ev := &pb.DelayEvent{
		AgencyId:         "test",
		TripId:           "trip_A1",
		RouteId:          "A",
		StopId:           "S1",
		DelaySeconds:     120,
		ObservedAt:       unixAt(8, 0, 0),
		ScheduledArrival: unixAt(8, 0, 0),
	}

	impacts, err := det.EvaluateDelay(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(impacts) == 0 {
		t.Fatal("expected at least one impact, got none")
	}

	imp := impacts[0]
	if imp.Level != pb.TransferImpact_BROKEN {
		t.Errorf("level = %v, want BROKEN", imp.Level)
	}
	if imp.FromRouteId != "A" || imp.ToRouteId != "L" {
		t.Errorf("routes = %s→%s, want A→L", imp.FromRouteId, imp.ToRouteId)
	}
	if imp.AdditionalWaitSeconds != 180 {
		t.Errorf("additional_wait = %d, want 180", imp.AdditionalWaitSeconds)
	}
	// Schedule frame for realized-wait evaluation: missed connection at
	// 28980, earliest catch = 28800 + 120 delay + 120 min transfer = 29040.
	if imp.SchedConnectionDepartureSecs != 28980 {
		t.Errorf("sched_connection_departure = %d, want 28980", imp.SchedConnectionDepartureSecs)
	}
	if imp.EarliestCatchSecs != 29040 {
		t.Errorf("earliest_catch = %d, want 29040", imp.EarliestCatchSecs)
	}
	t.Logf("impact: %s→%s level=%v margin=%ds additional_wait=%ds",
		imp.FromRouteId, imp.ToRouteId, imp.Level,
		imp.RemainingMarginSeconds, imp.AdditionalWaitSeconds)
}

func TestEvaluateDelay_AtRisk(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	g := buildTestGraph()
	mgr := state.NewDelayStateManager(rdb, g, nil)
	det := NewTransferDetector(g, mgr, nil, nil)

	// A train 30s late. Original margin to L = 180s.
	// Remaining = 180 - 30 = 150s. MinTransferTime = 120.
	// 150 < 120 + 60 (180) → AT_RISK.
	ev := &pb.DelayEvent{
		AgencyId:         "test",
		TripId:           "trip_A1",
		RouteId:          "A",
		StopId:           "S1",
		DelaySeconds:     30,
		ObservedAt:       unixAt(8, 0, 0),
		ScheduledArrival: unixAt(8, 0, 0),
	}

	impacts, err := det.EvaluateDelay(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(impacts) == 0 {
		t.Fatal("expected at least one impact, got none")
	}
	if impacts[0].Level != pb.TransferImpact_AT_RISK {
		t.Errorf("level = %v, want AT_RISK", impacts[0].Level)
	}
	t.Logf("impact: level=%v remaining_margin=%ds", impacts[0].Level, impacts[0].RemainingMarginSeconds)
}

func TestEvaluateDelay_BothRoutesDelayed(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	g := buildTestGraph()
	mgr := state.NewDelayStateManager(rdb, g, nil)
	det := NewTransferDetector(g, mgr, nil, nil)

	// Seed L delay at S1: L is also 90s late.
	mgr.ProcessEvent(ctx, &pb.DelayEvent{
		AgencyId: "test", TripId: "trip_L1", RouteId: "L", StopId: "S1",
		DelaySeconds: 90, ObservedAt: unixAt(8, 0, 0),
	})

	// A train 120s late → would be BROKEN without considering L's delay.
	// But L is 90s late, so effective remaining = 180 - 120 + 90 = 150.
	// 150 >= 120 but < 180 (120+60) → AT_RISK, not BROKEN.
	ev := &pb.DelayEvent{
		AgencyId:         "test",
		TripId:           "trip_A1",
		RouteId:          "A",
		StopId:           "S1",
		DelaySeconds:     120,
		ObservedAt:       unixAt(8, 0, 1),
		ScheduledArrival: unixAt(8, 0, 0),
	}

	impacts, err := det.EvaluateDelay(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(impacts) == 0 {
		t.Fatal("expected impact")
	}
	if impacts[0].Level != pb.TransferImpact_AT_RISK {
		t.Errorf("level = %v, want AT_RISK (L also delayed)", impacts[0].Level)
	}
	t.Logf("both-delayed: level=%v remaining=%ds", impacts[0].Level, impacts[0].RemainingMarginSeconds)
}

func TestEvaluateDelay_NoDelay_NoImpact(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	g := buildTestGraph()
	mgr := state.NewDelayStateManager(rdb, g, nil)
	det := NewTransferDetector(g, mgr, nil, nil)

	ev := &pb.DelayEvent{
		AgencyId: "test", TripId: "trip_A1", RouteId: "A", StopId: "S1",
		DelaySeconds: 0, ObservedAt: unixAt(8, 0, 0),
	}
	impacts, err := det.EvaluateDelay(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(impacts) != 0 {
		t.Errorf("expected no impacts for on-time train, got %d", len(impacts))
	}
}

// TestEvaluateDelay_OvernightTransfer covers the midnight wrap: an arrival
// just after local midnight must connect against the previous service day's
// GTFS 24:xx+ trips, not the next morning's first train.
func TestEvaluateDelay_OvernightTransfer(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	g := buildTestGraph()
	// Overnight service: A arrives 24:30 (88200s); L departs 24:35 (88500s)
	// and 24:50 (89400s). Appended in sorted order after the morning trips.
	g.TripRoute["trip_A2"] = "A"
	g.TripRoute["trip_L3"] = "L"
	g.TripRoute["trip_L4"] = "L"
	g.TripDirection["trip_A2"] = 0
	g.TripDirection["trip_L3"] = 0
	g.TripDirection["trip_L4"] = 0
	g.StopTimesByStop["S1"] = append(g.StopTimesByStop["S1"],
		&graph.ScheduledStopTime{TripID: "trip_A2", StopID: "S1", ArrivalSecs: 88200, DepartureSecs: 88200, StopSequence: 1},
		&graph.ScheduledStopTime{TripID: "trip_L3", StopID: "S1", ArrivalSecs: 88500, DepartureSecs: 88500, StopSequence: 1},
		&graph.ScheduledStopTime{TripID: "trip_L4", StopID: "S1", ArrivalSecs: 89400, DepartureSecs: 89400, StopSequence: 1},
	)

	mgr := state.NewDelayStateManager(rdb, g, nil)
	det := NewTransferDetector(g, mgr, nil, nil)

	// A arrives scheduled 00:30 local (tod 1800 → effective 88200 = 24:30)
	// with 240s delay. Margin to L at 24:35 = 300s; remaining = 60 < 120 →
	// BROKEN. Next viable after 88200+240+120 = 88560 → trip_L4 at 89400;
	// additional wait = 89400 - 88500 = 900s.
	ev := &pb.DelayEvent{
		AgencyId:         "test",
		TripId:           "trip_A2",
		RouteId:          "A",
		StopId:           "S1",
		DelaySeconds:     240,
		ObservedAt:       unixAt(0, 30, 0),
		ScheduledArrival: unixAt(0, 30, 0),
	}

	impacts, err := det.EvaluateDelay(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(impacts) == 0 {
		t.Fatal("overnight broken transfer not detected (midnight-wrap regression)")
	}
	imp := impacts[0]
	if imp.Level != pb.TransferImpact_BROKEN {
		t.Errorf("level = %v, want BROKEN", imp.Level)
	}
	if imp.OriginalTransferMarginSeconds != 300 {
		t.Errorf("margin = %d, want 300 (was ~4.5h against the morning trip before the fix)",
			imp.OriginalTransferMarginSeconds)
	}
	if imp.NextViableTripId != "trip_L4" {
		t.Errorf("next viable = %q, want trip_L4", imp.NextViableTripId)
	}
	if imp.AdditionalWaitSeconds != 900 {
		t.Errorf("additional_wait = %d, want 900", imp.AdditionalWaitSeconds)
	}
	// The frame stays in the effective (post-midnight) representation.
	if imp.SchedConnectionDepartureSecs != 88500 {
		t.Errorf("sched_connection_departure = %d, want 88500", imp.SchedConnectionDepartureSecs)
	}
	if imp.EarliestCatchSecs != 88560 {
		t.Errorf("earliest_catch = %d, want 88560", imp.EarliestCatchSecs)
	}
}

// TestEvaluateDelay_NeverViablePairIgnored: a schedule pair whose margin is
// below min_transfer_time was never a real connection — it must not go
// BROKEN on a trivial delay. The baseline is the first VIABLE departure.
func TestEvaluateDelay_NeverViablePairIgnored(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	g := buildTestGraph()
	// Insert an L trip at 28900 — only 100s after the A arrival, below the
	// 120s min transfer time: an artifact no passenger could catch.
	l0 := &graph.ScheduledStopTime{TripID: "trip_L0", StopID: "S1", ArrivalSecs: 28900, DepartureSecs: 28900, StopSequence: 1}
	g.TripRoute["trip_L0"] = "L"
	g.TripDirection["trip_L0"] = 0
	g.StopTimesByTrip["trip_L0"] = []*graph.ScheduledStopTime{l0}
	s1 := g.StopTimesByStop["S1"]
	g.StopTimesByStop["S1"] = append([]*graph.ScheduledStopTime{s1[0], l0}, s1[1:]...)

	mgr := state.NewDelayStateManager(rdb, g, nil)
	det := NewTransferDetector(g, mgr, nil, nil)

	// 30s delay: previously the 100s-margin artifact went BROKEN
	// (remaining 70 < 120). Now the baseline is trip_L1 (margin 180) →
	// remaining 150 → AT_RISK.
	ev := &pb.DelayEvent{
		AgencyId: "test", TripId: "trip_A1", RouteId: "A", StopId: "S1",
		DelaySeconds: 30, ObservedAt: unixAt(8, 0, 0), ScheduledArrival: unixAt(8, 0, 0),
	}
	impacts, err := det.EvaluateDelay(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(impacts) == 0 {
		t.Fatal("expected an impact against the viable connection")
	}
	imp := impacts[0]
	if imp.Level == pb.TransferImpact_BROKEN {
		t.Error("never-viable schedule artifact classified BROKEN")
	}
	if imp.OriginalTransferMarginSeconds != 180 {
		t.Errorf("margin = %d, want 180 (the viable trip_L1 connection, not the 100s artifact)",
			imp.OriginalTransferMarginSeconds)
	}
}

// TestEvaluateDelay_EarlyConnectingTrainTightens: an early-running
// connection shrinks the real margin — previously negative delays were
// ignored and this scenario was misclassified AT_RISK.
func TestEvaluateDelay_EarlyConnectingTrainTightens(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	g := buildTestGraph()
	mgr := state.NewDelayStateManager(rdb, g, nil)
	det := NewTransferDetector(g, mgr, nil, nil)

	// The connection trip runs 60s early.
	if err := mgr.ProcessEvent(ctx, &pb.DelayEvent{
		AgencyId: "test", TripId: "trip_L1", RouteId: "L", StopId: "S1",
		DelaySeconds: -60, ObservedAt: unixAt(8, 0, 0),
	}); err != nil {
		t.Fatal(err)
	}

	// A 30s source delay: remaining = 180 - 30 - 60 = 90 < 120 → BROKEN.
	ev := &pb.DelayEvent{
		AgencyId: "test", TripId: "trip_A1", RouteId: "A", StopId: "S1",
		DelaySeconds: 30, ObservedAt: unixAt(8, 0, 1), ScheduledArrival: unixAt(8, 0, 0),
	}
	impacts, err := det.EvaluateDelay(ctx, ev)
	if err != nil {
		t.Fatal(err)
	}
	if len(impacts) == 0 {
		t.Fatal("expected impact")
	}
	if impacts[0].Level != pb.TransferImpact_BROKEN {
		t.Errorf("level = %v, want BROKEN (early connecting train tightens the margin)", impacts[0].Level)
	}
	if impacts[0].RemainingMarginSeconds != 90 {
		t.Errorf("remaining = %d, want 90", impacts[0].RemainingMarginSeconds)
	}
}
