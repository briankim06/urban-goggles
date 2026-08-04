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
		"trip_G2": "G",
	}
	tripDir := map[string]int{
		"trip_A1": 0,
		"trip_L1": 0,
		"trip_L2": 0,
		"trip_G1": 0,
		"trip_G2": 0,
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
	// scheduled headway (600s) at S1 during hour 8; a second G trip 1800s
	// behind trip_G1 does the same for G at S3 and lets deriveDirection find
	// a departure after 08:30.
	tripL2S1 := &graph.ScheduledStopTime{TripID: "trip_L2", StopID: "S1", StopSequence: 1, ArrivalSecs: 29400, DepartureSecs: 29400}
	stopTimesByTrip["trip_L2"] = []*graph.ScheduledStopTime{tripL2S1}
	stopTimesByTrip["trip_G2"] = []*graph.ScheduledStopTime{
		{TripID: "trip_G2", StopID: "S3G", StopSequence: 1, ArrivalSecs: 30900, DepartureSecs: 30900},
		{TripID: "trip_G2", StopID: "S5", StopSequence: 2, ArrivalSecs: 31020, DepartureSecs: 31020},
	}
	// Keyed by parent station and sorted by departure time, as BuildGraph
	// produces (G's platform S3G folds into parent S3).
	stopTimesByStop := map[string][]*graph.ScheduledStopTime{
		"S1": {stopTimesByTrip["trip_L1"][0], tripL2S1},
		"S2": stopTimesByTrip["trip_L1"][1:2],
		"S3": {stopTimesByTrip["trip_L1"][2], stopTimesByTrip["trip_G1"][0], stopTimesByTrip["trip_G2"][0]},
		"S4": stopTimesByTrip["trip_L1"][3:4],
		"S5": {stopTimesByTrip["trip_G1"][1], stopTimesByTrip["trip_G2"][1]},
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
	engine := NewPropagationEngine(g, mgr, hist, nil, "MTA", Config{}, nil)

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

	// Impacts land on route L directly, plus a cascade onto G via the S3
	// transfer — the transfer station itself and G's downstream stops.
	sawL := false
	for _, imp := range result.GetImpacts() {
		switch imp.GetRouteId() {
		case "L":
			sawL = true
		case "G":
			if it := imp.GetImpactType(); it != "cascade_transfer" && it != "cascade_dwell" {
				t.Errorf("G impact should be cascade_transfer or cascade_dwell, got %s", it)
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
	engine := NewPropagationEngine(g, mgr, hist, nil, "MTA", Config{}, nil)

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
	engine := NewPropagationEngine(g, mgr, hist, nil, "MTA", Config{}, nil)

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
	engine := NewPropagationEngine(g, mgr, hist, nil, "MTA", Config{}, nil)

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
	engine := NewPropagationEngine(g, nil, nil, nil, "MTA", Config{}, nil)

	base := &pb.TransferImpact{
		FromRouteId:        "A",
		ToRouteId:          "L",
		StationId:          "S1",
		SourceDelaySeconds: 300,
	}
	at := func(hour int, wait int32) *pb.TransferImpact {
		imp := proto.Clone(base).(*pb.TransferImpact)
		imp.DetectedAt = localUnix(hour)
		imp.AdditionalWaitSeconds = wait
		return imp
	}

	// No wait, no headway (hour 14), no history — legacy linear fallback:
	// 300 * 0.15 * 0.3 = 13.5 → 14.
	if d := engine.estimateInitialDelay(at(14, 0), 0, 0, 0, 0); d != 14 {
		t.Errorf("legacy fallback = %d, want 14", d)
	}

	// Detector-computed wait dominates when no headway exists.
	if d := engine.estimateInitialDelay(at(14, 600), 0, 0, 0, 0); d != 600 {
		t.Errorf("wait-based = %d, want 600", d)
	}

	// Headway guard: hour 8 has a 600s headway → half = 300 exceeds a small
	// detector wait of 200.
	if d := engine.estimateInitialDelay(at(8, 200), 0, 0, 0, 0); d != 300 {
		t.Errorf("headway-guarded = %d, want 300", d)
	}

	// A connecting route already 200s late pushes the next viable train
	// later: 300 + 200 = 500.
	if d := engine.estimateInitialDelay(at(8, 200), 0, 200, 0, 0); d != 500 {
		t.Errorf("connecting-delay = %d, want 500", d)
	}

	// Connecting delay is capped at one headway (600): 300 + 600 = 900.
	if d := engine.estimateInitialDelay(at(8, 200), 0, 900, 0, 0); d != 900 {
		t.Errorf("capped connecting-delay = %d, want 900", d)
	}

	// History shrinkage: w = 10/(10+10) = 0.5 → 0.5*50 + 0.5*300 = 175.
	if d := engine.estimateInitialDelay(at(8, 200), 0, 0, 50.0, 10); d != 175 {
		t.Errorf("history blend = %d, want 175", d)
	}

	// Sparse history still contributes, weighted lightly:
	// w = 3/13 → round(3/13*50 + 10/13*300) = 242.
	if d := engine.estimateInitialDelay(at(8, 200), 0, 0, 50.0, 3); d != 242 {
		t.Errorf("low-count blend = %d, want 242", d)
	}

	// Self-propagation (from == to): the train carries its own delay — no
	// transfer wait, no headway guard, no connecting-delay addition.
	self := at(8, 200)
	self.FromRouteId = "L"
	if d := engine.estimateInitialDelay(self, 0, 400, 0, 0); d != 300 {
		t.Errorf("self-propagation = %d, want 300 (source delay)", d)
	}
}

func brokenLTransfer() *pb.TransferImpact {
	return &pb.TransferImpact{
		FromTripId:         "trip_A1",
		FromRouteId:        "A",
		ToRouteId:          "L",
		StationId:          "S1",
		SourceDelaySeconds: 300,
		NextViableTripId:   "trip_L1",
		DetectedAt:         localUnix(8),
	}
}

func TestPropagate_CascadeWalksDownstream(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	g := buildPropGraph()
	mgr := state.NewDelayStateManager(rdb, g, nil)
	hist := NewHistoricalStore(rdb)
	engine := NewPropagationEngine(g, mgr, hist, nil, "MTA", Config{}, nil)

	result, err := engine.Propagate(ctx, brokenLTransfer())
	if err != nil {
		t.Fatal(err)
	}

	// The cascade onto G must ripple past the transfer station to S5.
	var sawS5 bool
	seen := make(map[string]bool)
	for _, imp := range result.GetImpacts() {
		key := imp.GetRouteId() + ":" + imp.GetStopId()
		if seen[key] {
			t.Errorf("duplicate impact for %s (bounce-back regression)", key)
		}
		seen[key] = true
		if imp.GetRouteId() == "G" && imp.GetStopId() == "S5" {
			sawS5 = true
			if imp.GetImpactType() != "cascade_dwell" {
				t.Errorf("G:S5 impact type = %s, want cascade_dwell", imp.GetImpactType())
			}
			if imp.GetHopsFromSource() != 2 {
				t.Errorf("G:S5 hops = %d, want 2", imp.GetHopsFromSource())
			}
		}
	}
	if !sawS5 {
		t.Error("cascade did not walk downstream to G:S5")
	}
	if len(result.GetImpacts()) > DefaultConfig().MaxImpacts {
		t.Errorf("impacts %d exceed cap", len(result.GetImpacts()))
	}
}

func TestPropagate_LiveDelayGatesCascade(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	g := buildPropGraph()
	mgr := state.NewDelayStateManager(rdb, g, nil)
	hist := NewHistoricalStore(rdb)
	engine := NewPropagationEngine(g, mgr, hist, nil, "MTA", Config{}, nil)

	// Control: with no live delays the cascade onto G fires.
	result, err := engine.Propagate(ctx, brokenLTransfer())
	if err != nil {
		t.Fatal(err)
	}
	if !hasRouteImpact(result, "G") {
		t.Fatal("control run should cascade onto G")
	}

	// G is already running 400s late at S3: the accumulated L delay (301s)
	// no longer exceeds min_transfer_time (60) + connecting delay (400), so
	// the late G train is still catchable and the cascade must not fire.
	if err := mgr.ProcessEvent(ctx, &pb.DelayEvent{
		AgencyId: "MTA", TripId: "trip_G1", RouteId: "G", StopId: "S3G",
		DelaySeconds: 400, ObservedAt: localUnix(8),
	}); err != nil {
		t.Fatal(err)
	}
	result, err = engine.Propagate(ctx, brokenLTransfer())
	if err != nil {
		t.Fatal(err)
	}
	if hasRouteImpact(result, "G") {
		t.Error("cascade onto G fired despite the connecting route running late")
	}
}

func hasRouteImpact(result *pb.PropagationResult, routeID string) bool {
	for _, imp := range result.GetImpacts() {
		if imp.GetRouteId() == routeID {
			return true
		}
	}
	return false
}

func TestPropagate_ImpactCap(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	g := buildPropGraph()
	mgr := state.NewDelayStateManager(rdb, g, nil)
	hist := NewHistoricalStore(rdb)
	engine := NewPropagationEngine(g, mgr, hist, nil, "MTA", Config{MaxImpacts: 3}, nil)

	result, err := engine.Propagate(ctx, brokenLTransfer())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.GetImpacts()) > 3 {
		t.Errorf("impacts = %d, want <= 3", len(result.GetImpacts()))
	}
}

func TestPropagate_PerStopBlend(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	g := buildPropGraph()
	mgr := state.NewDelayStateManager(rdb, g, nil)
	hist := NewHistoricalStore(rdb)
	stopStore := NewStopImpactStore(rdb)

	// 10 observed outcomes of 600s at L:S3 during hour 8.
	for i := 0; i < 10; i++ {
		if err := stopStore.RecordStopOutcome(ctx, "L", "S3", 8, 600); err != nil {
			t.Fatal(err)
		}
	}

	engine := NewPropagationEngine(g, mgr, hist, stopStore, "MTA", Config{}, nil)
	result, err := engine.Propagate(ctx, brokenLTransfer())
	if err != nil {
		t.Fatal(err)
	}

	// Model value at L:S3 is 301; blend = round(0.5*600 + 0.5*301) = 451.
	for _, imp := range result.GetImpacts() {
		if imp.GetRouteId() == "L" && imp.GetStopId() == "S3" {
			if imp.GetPredictedAdditionalDelay() != 451 {
				t.Errorf("blended L:S3 delay = %d, want 451", imp.GetPredictedAdditionalDelay())
			}
			return
		}
	}
	t.Error("no L:S3 impact found")
}

func TestPropagate_SelfRoute(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	g := buildPropGraph()
	mgr := state.NewDelayStateManager(rdb, g, nil)
	hist := NewHistoricalStore(rdb)
	engine := NewPropagationEngine(g, mgr, hist, nil, "MTA", Config{}, nil)

	// Synthetic self-propagation impact, as the gRPC handlers build: a
	// delayed L train carries its 300s delay down its own line.
	impact := &pb.TransferImpact{
		FromTripId:         "trip_L1",
		FromRouteId:        "L",
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
	if len(result.GetImpacts()) == 0 {
		t.Fatal("self-propagation produced no impacts")
	}
	for _, imp := range result.GetImpacts() {
		if imp.GetRouteId() == "L" && imp.GetStopId() == "S2" {
			if imp.GetPredictedAdditionalDelay() != 300 {
				t.Errorf("first-stop delay = %d, want 300 (source delay carried)", imp.GetPredictedAdditionalDelay())
			}
			return
		}
	}
	t.Error("no L:S2 impact found")
}
