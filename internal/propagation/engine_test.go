package propagation

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"google.golang.org/protobuf/proto"

	"github.com/briankim06/urban-goggles/internal/graph"
	"github.com/briankim06/urban-goggles/internal/state"
	pb "github.com/briankim06/urban-goggles/proto/transit"
)

// buildPropGraph creates a synthetic graph with:
//   - Station S1 (transfer point) served by routes A and L
//   - Route L has downstream stops: S2, S3, S4 (direction 0)
//   - Station S3 has a transfer to route G at stop S3G
//   - Route G has downstream stops: S5 (direction 0)
func buildPropGraph() *graph.TransitGraph {
	stops := map[string]*graph.Stop{
		"S1":  {ID: "S1", Name: "Transfer Hub", LocationType: 1},
		"S2":  {ID: "S2", Name: "Second Stop", LocationType: 1},
		"S3":  {ID: "S3", Name: "Third Stop", LocationType: 1},
		"S3G": {ID: "S3G", Name: "Third Stop G Platform", ParentStation: "S3", LocationType: 0},
		"S4":  {ID: "S4", Name: "Fourth Stop", LocationType: 1},
		"S5":  {ID: "S5", Name: "Fifth Stop", LocationType: 1},
	}
	routes := map[string]*graph.Route{
		"A": {ID: "A", ShortName: "A"},
		"L": {ID: "L", ShortName: "L"},
		"G": {ID: "G", ShortName: "G"},
	}
	tripRoute := map[string]string{
		"trip_A1": "A",
		"trip_L1": "L",
		"trip_L2": "L",
		"trip_G1": "G",
	}
	tripDir := map[string]int{
		"trip_A1": 0,
		"trip_L1": 0,
		"trip_L2": 0,
		"trip_G1": 0,
	}
	// Route L stops: S1 (seq 1) → S2 (seq 2) → S3 (seq 3) → S4 (seq 4)
	// Route G stops: S3G (seq 1) → S5 (seq 2)
	stopTimesByTrip := map[string][]*graph.ScheduledStopTime{
		"trip_L1": {
			{TripID: "trip_L1", StopID: "S1", StopSequence: 1, ArrivalSecs: 28800, DepartureSecs: 28800},
			{TripID: "trip_L1", StopID: "S2", StopSequence: 2, ArrivalSecs: 28920, DepartureSecs: 28920},
			{TripID: "trip_L1", StopID: "S3", StopSequence: 3, ArrivalSecs: 29040, DepartureSecs: 29040},
			{TripID: "trip_L1", StopID: "S4", StopSequence: 4, ArrivalSecs: 29160, DepartureSecs: 29160},
		},
		"trip_G1": {
			{TripID: "trip_G1", StopID: "S3G", StopSequence: 1, ArrivalSecs: 29100, DepartureSecs: 29100},
			{TripID: "trip_G1", StopID: "S5", StopSequence: 2, ArrivalSecs: 29220, DepartureSecs: 29220},
		},
	}
	// A second L trip 600s behind trip_L1 gives route L a computable
	// scheduled headway (600s) at S1 during hour 8.
	tripL2S1 := &graph.ScheduledStopTime{TripID: "trip_L2", StopID: "S1", StopSequence: 1, ArrivalSecs: 29400, DepartureSecs: 29400}
	stopTimesByTrip["trip_L2"] = []*graph.ScheduledStopTime{tripL2S1}
	stopTimesByStop := map[string][]*graph.ScheduledStopTime{
		"S1":  {stopTimesByTrip["trip_L1"][0], tripL2S1},
		"S2":  stopTimesByTrip["trip_L1"][1:2],
		"S3":  stopTimesByTrip["trip_L1"][2:3],
		"S4":  stopTimesByTrip["trip_L1"][3:4],
		"S3G": stopTimesByTrip["trip_G1"][:1],
		"S5":  stopTimesByTrip["trip_G1"][1:2],
	}
	transfers := map[string][]*graph.Transfer{
		"S1": {
			{FromStopID: "S1", ToStopID: "S1", TransferType: 2, MinTransferTime: 120},
		},
		"S3": {
			{FromStopID: "S3", ToStopID: "S3G", TransferType: 2, MinTransferTime: 60},
		},
	}
	// Keyed by parent station, as BuildGraph does — S3 includes route G via
	// its child platform S3G.
	routesAtStop := map[string]map[string]bool{
		"S1": {"A": true, "L": true},
		"S2": {"L": true},
		"S3": {"L": true, "G": true},
		"S4": {"L": true},
		"S5": {"G": true},
	}

	g := &graph.TransitGraph{
		Stops:           stops,
		Routes:          routes,
		TripRoute:       tripRoute,
		TripDirection:   tripDir,
		StopTimesByStop: stopTimesByStop,
		StopTimesByTrip: stopTimesByTrip,
		TransfersByStop: transfers,
		RoutesAtStop:    routesAtStop,
	}
	g.BuildHeadwayIndex()
	return g
}

// localUnix returns a Unix timestamp whose local time falls at the given
// hour, so tests exercising the headway lookup are deterministic regardless
// of when (or in which timezone) they run.
func localUnix(hour int) int64 {
	return time.Date(2026, 1, 15, hour, 30, 0, 0, time.Local).Unix()
}

func skipIfNoRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("Redis not available: %v", err)
	}
	return rdb
}

func TestPropagate_DownstreamStops(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	g := buildPropGraph()
	mgr := state.NewDelayStateManager(rdb, g, nil)
	hist := NewHistoricalStore(rdb)
	engine := NewPropagationEngine(g, mgr, hist, nil)

	impact := &pb.TransferImpact{
		FromTripId:         "trip_A1",
		FromRouteId:        "A",
		ToRouteId:          "L",
		StationId:          "S1",
		SourceDelaySeconds: 300,
		NextViableTripId:   "trip_L1",
		DetectedAt:         localUnix(8), // headway bucket hour 8 → initial delay 300
	}

	result, err := engine.Propagate(ctx, impact)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.GetImpacts()) == 0 {
		t.Fatal("expected downstream impacts, got none")
	}

	// Impacts land on route L directly, plus a possible cascade onto G via
	// the S3 transfer now that the initial delay is headway-based.
	sawL := false
	for _, imp := range result.GetImpacts() {
		switch imp.GetRouteId() {
		case "L":
			sawL = true
		case "G":
			if imp.GetImpactType() != "cascade_transfer" {
				t.Errorf("G impact should be cascade_transfer, got %s", imp.GetImpactType())
			}
		default:
			t.Errorf("unexpected route %s", imp.GetRouteId())
		}
		if imp.GetPredictedAdditionalDelay() <= 0 {
			t.Errorf("expected positive delay, got %d", imp.GetPredictedAdditionalDelay())
		}
		if imp.GetConfidence() <= 0 || imp.GetConfidence() > 1.0 {
			t.Errorf("confidence out of range: %f", imp.GetConfidence())
		}
	}
	if !sawL {
		t.Error("expected at least one impact on route L")
	}

	t.Logf("propagation produced %d downstream impacts", len(result.GetImpacts()))
	for _, imp := range result.GetImpacts() {
		t.Logf("  %s stop=%s delay=%ds conf=%.2f hops=%d type=%s",
			imp.GetRouteId(), imp.GetStopId(), imp.GetPredictedAdditionalDelay(),
			imp.GetConfidence(), imp.GetHopsFromSource(), imp.GetImpactType())
	}
}

func TestPropagate_ConfidenceDecays(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	g := buildPropGraph()
	mgr := state.NewDelayStateManager(rdb, g, nil)
	hist := NewHistoricalStore(rdb)
	engine := NewPropagationEngine(g, mgr, hist, nil)

	impact := &pb.TransferImpact{
		FromTripId:         "trip_A1",
		FromRouteId:        "A",
		ToRouteId:          "L",
		StationId:          "S1",
		SourceDelaySeconds: 300,
		NextViableTripId:   "trip_L1",
		DetectedAt:         localUnix(8),
	}

	result, err := engine.Propagate(ctx, impact)
	if err != nil {
		t.Fatal(err)
	}

	// Confidence should decrease as hops increase.
	confByHop := make(map[int32]float32)
	for _, imp := range result.GetImpacts() {
		existing, ok := confByHop[imp.GetHopsFromSource()]
		if !ok || imp.GetConfidence() > existing {
			confByHop[imp.GetHopsFromSource()] = imp.GetConfidence()
		}
	}

	// If we have hop 1, its max confidence should be >= any hop 2 confidence.
	if c1, ok := confByHop[1]; ok {
		for hop, c := range confByHop {
			if hop > 1 && c > c1 {
				t.Errorf("hop %d confidence %.2f > hop 1 confidence %.2f", hop, c, c1)
			}
		}
	}
}

func TestPropagate_SmallDelay_NoImpacts(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	g := buildPropGraph()
	mgr := state.NewDelayStateManager(rdb, g, nil)
	hist := NewHistoricalStore(rdb)
	engine := NewPropagationEngine(g, mgr, hist, nil)

	// Very small delay, no next-viable-wait, and an hour with no scheduled
	// departures (14:00 — the fixture's headway exists only for hour 8), so
	// the estimate falls back to the legacy linear model:
	// 30 * 0.15 * 0.3 = 1.35s, below minDelayThreshold (15s). The first stop
	// still gets a tiny impact but nothing accumulates past the threshold.
	impact := &pb.TransferImpact{
		FromTripId:         "trip_A1",
		FromRouteId:        "A",
		ToRouteId:          "L",
		StationId:          "S1",
		SourceDelaySeconds: 30, // small delay
		NextViableTripId:   "trip_L1",
		DetectedAt:         localUnix(14),
	}

	result, err := engine.Propagate(ctx, impact)
	if err != nil {
		t.Fatal(err)
	}

	for _, imp := range result.GetImpacts() {
		if imp.GetPredictedAdditionalDelay() >= 15 {
			t.Errorf("small delay produced impact >= threshold: %d", imp.GetPredictedAdditionalDelay())
		}
	}
	t.Logf("small delay produced %d impacts", len(result.GetImpacts()))
}

func TestPropagate_DoesNotRecordHistory(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	g := buildPropGraph()
	mgr := state.NewDelayStateManager(rdb, g, nil)
	hist := NewHistoricalStore(rdb)
	engine := NewPropagationEngine(g, mgr, hist, nil)

	impact := &pb.TransferImpact{
		FromTripId:         "trip_A1",
		FromRouteId:        "A",
		ToRouteId:          "L",
		StationId:          "S1",
		SourceDelaySeconds: 300,
		NextViableTripId:   "trip_L1",
		DetectedAt:         localUnix(8),
	}

	_, err := engine.Propagate(ctx, impact)
	if err != nil {
		t.Fatal(err)
	}

	// Propagate must NOT write its own predictions into the historical
	// store — only observed outcomes (via the evaluation sweeper) belong
	// there.
	keys, err := rdb.Keys(ctx, "history:*").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Errorf("Propagate wrote history keys itself: %v", keys)
	}
}

func TestEstimateInitialDelay(t *testing.T) {
	g := buildPropGraph()
	engine := &PropagationEngine{graph: g}

	base := &pb.TransferImpact{
		FromRouteId:        "A",
		ToRouteId:          "L",
		StationId:          "S1",
		SourceDelaySeconds: 300,
	}

	// No wait, no headway (hour 14), no history — legacy linear fallback:
	// 300 * 0.15 * 0.3 = 13.5 → 14.
	imp := proto.Clone(base).(*pb.TransferImpact)
	imp.DetectedAt = localUnix(14)
	if d := engine.estimateInitialDelay(imp, 0, 0, 0); d != 14 {
		t.Errorf("legacy fallback = %d, want 14", d)
	}

	// Detector-computed wait dominates when no headway exists.
	imp = proto.Clone(base).(*pb.TransferImpact)
	imp.DetectedAt = localUnix(14)
	imp.AdditionalWaitSeconds = 600
	if d := engine.estimateInitialDelay(imp, 0, 0, 0); d != 600 {
		t.Errorf("wait-based = %d, want 600", d)
	}

	// Headway guard: hour 8 has a 600s headway → half = 300 exceeds a small
	// detector wait of 200.
	imp = proto.Clone(base).(*pb.TransferImpact)
	imp.DetectedAt = localUnix(8)
	imp.AdditionalWaitSeconds = 200
	if d := engine.estimateInitialDelay(imp, 0, 0, 0); d != 300 {
		t.Errorf("headway-guarded = %d, want 300", d)
	}

	// Sufficient history blends 60/40 with the schedule estimate:
	// 0.6*50 + 0.4*300 = 150.
	imp = proto.Clone(base).(*pb.TransferImpact)
	imp.DetectedAt = localUnix(8)
	imp.AdditionalWaitSeconds = 200
	if d := engine.estimateInitialDelay(imp, 0, 50.0, 10); d != 150 {
		t.Errorf("history blend = %d, want 150", d)
	}

	// Insufficient history (count < 5) — schedule estimate only.
	imp = proto.Clone(base).(*pb.TransferImpact)
	imp.DetectedAt = localUnix(8)
	imp.AdditionalWaitSeconds = 200
	if d := engine.estimateInitialDelay(imp, 0, 50.0, 3); d != 300 {
		t.Errorf("low-count = %d, want 300", d)
	}
}
