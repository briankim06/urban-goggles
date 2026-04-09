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
		StopTimesByTrip: map[string][]*graph.ScheduledStopTime{},
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
