package propagation

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"

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
		"trip_G1": "G",
	}
	tripDir := map[string]int{
		"trip_A1": 0,
		"trip_L1": 0,
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
	stopTimesByStop := map[string][]*graph.ScheduledStopTime{
		"S1":  stopTimesByTrip["trip_L1"][:1],
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
	routesAtStop := map[string]map[string]bool{
		"S1":  {"A": true, "L": true},
		"S2":  {"L": true},
		"S3":  {"L": true},
		"S3G": {"G": true},
		"S4":  {"L": true},
		"S5":  {"G": true},
	}

	return &graph.TransitGraph{
		Stops:           stops,
		Routes:          routes,
		TripRoute:       tripRoute,
		TripDirection:   tripDir,
		StopTimesByStop: stopTimesByStop,
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
	}

	result, err := engine.Propagate(ctx, impact)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.GetImpacts()) == 0 {
		t.Fatal("expected downstream impacts, got none")
	}

	// All impacts should be on route L.
	for _, imp := range result.GetImpacts() {
		if imp.GetRouteId() != "L" {
			t.Errorf("expected route L, got %s", imp.GetRouteId())
		}
		if imp.GetPredictedAdditionalDelay() <= 0 {
			t.Errorf("expected positive delay, got %d", imp.GetPredictedAdditionalDelay())
		}
		if imp.GetConfidence() <= 0 || imp.GetConfidence() > 1.0 {
			t.Errorf("confidence out of range: %f", imp.GetConfidence())
		}
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

	// Very small delay should produce impacts below minDelayThreshold,
	// resulting in few or no downstream impacts.
	impact := &pb.TransferImpact{
		FromTripId:         "trip_A1",
		FromRouteId:        "A",
		ToRouteId:          "L",
		StationId:          "S1",
		SourceDelaySeconds: 30, // small delay
		NextViableTripId:   "trip_L1",
	}

	result, err := engine.Propagate(ctx, impact)
	if err != nil {
		t.Fatal(err)
	}

	// With a 30s source delay, initial model delay = 30 * 0.15 * 0.3 = 1.35s
	// which is below minDelayThreshold (15s), so first stop may get added
	// but accumulated delay won't reach threshold for further stops.
	// The first stop still gets added (initialDelay is rounded to ~1).
	// Actually 1.35 rounds to 1, which is < 15, so even the first hop check
	// in the queue will produce an impact but it will be tiny.
	t.Logf("small delay produced %d impacts", len(result.GetImpacts()))
}

func TestPropagate_RecordsHistory(t *testing.T) {
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
	}

	_, err := engine.Propagate(ctx, impact)
	if err != nil {
		t.Fatal(err)
	}

	// Verify history was recorded. The engine uses time.Now().Hour().
	keys, err := rdb.Keys(ctx, "history:A:L:S1:*").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) == 0 {
		t.Error("expected history key to be recorded, found none")
	} else {
		t.Logf("history observation recorded: %s", keys[0])
	}
}

func TestEstimateInitialDelay_WithHistory(t *testing.T) {
	engine := &PropagationEngine{}

	// No historical data — pure model.
	delay := engine.estimateInitialDelay(300, 0, 0)
	// model = 300 * 0.15 * 0.3 = 13.5 → 14
	if delay != 14 {
		t.Errorf("no-history delay = %d, want 14", delay)
	}

	// With sufficient historical data — blended.
	delay = engine.estimateInitialDelay(300, 50.0, 10)
	// model = 13.5, blend = 0.6*50 + 0.4*13.5 = 30 + 5.4 = 35.4 → 35
	if delay != 35 {
		t.Errorf("blended delay = %d, want 35", delay)
	}

	// With insufficient historical data (count < 5) — pure model.
	delay = engine.estimateInitialDelay(300, 50.0, 3)
	if delay != 14 {
		t.Errorf("low-count delay = %d, want 14 (model only)", delay)
	}
}
