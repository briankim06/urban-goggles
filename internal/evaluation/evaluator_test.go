package evaluation

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/briankim06/urban-goggles/internal/graph"
	"github.com/briankim06/urban-goggles/internal/propagation"
	"github.com/briankim06/urban-goggles/internal/state"
	pb "github.com/briankim06/urban-goggles/proto/transit"
)

func skipIfNoRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("Redis not available: %v", err)
	}
	return rdb
}

// buildEvalGraph provides parent stations S1-S3 and trips on route L in both
// directions plus a G trip, enough for matching and direction checks.
func buildEvalGraph() *graph.TransitGraph {
	return &graph.TransitGraph{
		Stops: map[string]*graph.Stop{
			"S1":  {ID: "S1", Name: "First", LocationType: 1},
			"S2":  {ID: "S2", Name: "Second", LocationType: 1},
			"S2N": {ID: "S2N", Name: "Second N", ParentStation: "S2"},
			"S3":  {ID: "S3", Name: "Third", LocationType: 1},
		},
		TripRoute: map[string]string{
			"trip_L1":   "L",
			"trip_Lrev": "L",
			"trip_G1":   "G",
		},
		TripDirection: map[string]int{
			"trip_L1":   0,
			"trip_Lrev": 1,
			"trip_G1":   0,
		},
	}
}

type evalFixture struct {
	rdb       *redis.Client
	store     *PendingStore
	mgr       *state.DelayStateManager
	hist      *propagation.HistoricalStore
	stopStore *propagation.StopImpactStore
	eval      *Evaluator
}

func newEvalFixture(t *testing.T) *evalFixture {
	t.Helper()
	rdb := skipIfNoRedis(t)
	t.Cleanup(func() { rdb.Close() })
	rdb.FlushDB(context.Background())

	g := buildEvalGraph()
	mgr := state.NewDelayStateManager(rdb, g, nil)
	hist := propagation.NewHistoricalStore(rdb)
	stopStore := propagation.NewStopImpactStore(rdb)
	store := NewPendingStore(rdb)
	return &evalFixture{
		rdb:       rdb,
		store:     store,
		mgr:       mgr,
		hist:      hist,
		stopStore: stopStore,
		eval:      NewEvaluator(store, g, mgr, hist, stopStore, "MTA", nil),
	}
}

func testResult(now time.Time) (*pb.TransferImpact, *pb.PropagationResult) {
	src := &pb.TransferImpact{
		FromTripId:         "trip_A1",
		FromRouteId:        "A",
		ToRouteId:          "L",
		StationId:          "S1",
		SourceDelaySeconds: 300,
		NextViableTripId:   "trip_L1",
		DetectedAt:         now.Unix(),
	}
	res := &pb.PropagationResult{
		SourceTripId:       "trip_A1",
		SourceStopId:       "S1",
		SourceDelaySeconds: 300,
		ComputedAt:         now.Unix(),
		Impacts: []*pb.DownstreamImpact{
			// Accumulated-dwell stop pops first (heap orders by delay), so the
			// primary (smallest hop-1 delay on the receiving route) is NOT
			// index 0.
			{RouteId: "L", StopId: "S3", PredictedAdditionalDelay: 301, Confidence: 0.49, HopsFromSource: 1, ImpactType: "dwell_increase"},
			{RouteId: "L", StopId: "S2", PredictedAdditionalDelay: 300, Confidence: 1.0, HopsFromSource: 1, ImpactType: "dwell_increase"},
			{RouteId: "G", StopId: "S3", PredictedAdditionalDelay: 90, Confidence: 0.34, HopsFromSource: 2, ImpactType: "cascade_transfer"},
			{RouteId: "L", StopId: "S1", PredictedAdditionalDelay: 20, Confidence: 0.2, HopsFromSource: 3, ImpactType: "cascade_transfer"},
		},
	}
	return src, res
}

func TestPendingStore_AddFindPop(t *testing.T) {
	f := newEvalFixture(t)
	ctx := context.Background()
	now := time.Now()

	p := &PendingPrediction{
		ID: "A:L:S2:1:0", AgencyID: "MTA", FromRoute: "A", RouteID: "L", StationID: "S2",
		Direction: 0, PredictedSecs: 300, Confidence: 1.0, Hops: 1, Hour: 8, IsPrimary: true,
		CreatedAt: now.Unix(), ExpiresAt: now.Unix() + 60,
	}
	if err := f.store.Add(ctx, p); err != nil {
		t.Fatal(err)
	}

	active, err := f.store.FindActive(ctx, "MTA", "L", "S2", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != p.ID {
		t.Fatalf("FindActive = %v, want 1 result with ID %s", active, p.ID)
	}

	// Wrong route or station → no match.
	if got, _ := f.store.FindActive(ctx, "MTA", "G", "S2", now); len(got) != 0 {
		t.Errorf("wrong route matched: %v", got)
	}
	if got, _ := f.store.FindActive(ctx, "MTA", "L", "S3", now); len(got) != 0 {
		t.Errorf("wrong station matched: %v", got)
	}

	// After the window, FindActive misses but PopExpired claims it.
	later := now.Add(120 * time.Second)
	if got, _ := f.store.FindActive(ctx, "MTA", "L", "S2", later); len(got) != 0 {
		t.Errorf("expired prediction still active: %v", got)
	}
	popped, err := f.store.PopExpired(ctx, later, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(popped) != 1 || popped[0].ID != p.ID {
		t.Fatalf("PopExpired = %v, want the expired prediction", popped)
	}
	// Idempotent: second pop returns nothing.
	if again, _ := f.store.PopExpired(ctx, later, 10); len(again) != 0 {
		t.Errorf("second PopExpired returned %v", again)
	}
}

func TestTrackPrediction_BaselineAndPrimary(t *testing.T) {
	f := newEvalFixture(t)
	ctx := context.Background()
	now := time.Now()

	// Route L already has a 60s delay at S2 before the prediction is made.
	if err := f.mgr.ProcessEvent(ctx, &pb.DelayEvent{
		AgencyId: "MTA", TripId: "trip_L1", RouteId: "L", StopId: "S2",
		DelaySeconds: 60, ObservedAt: now.Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	src, res := testResult(now)
	if err := f.eval.TrackPrediction(ctx, src, res); err != nil {
		t.Fatal(err)
	}

	active, err := f.store.FindActive(ctx, "MTA", "L", "S2", now)
	if err != nil || len(active) != 1 {
		t.Fatalf("expected 1 pending at L/S2, got %d (err %v)", len(active), err)
	}
	p := active[0]
	if !p.IsPrimary {
		t.Error("smallest hop-1 impact on receiving route should be primary")
	}
	if p.BaselineSecs != 60 {
		t.Errorf("baseline = %d, want 60", p.BaselineSecs)
	}
	if p.Direction != 0 {
		t.Errorf("direction = %d, want 0 (from next viable trip)", p.Direction)
	}
	if p.SourceStation != "S1" {
		t.Errorf("source station = %q, want S1 (the transfer station)", p.SourceStation)
	}

	// The S3 dwell impact is tracked but not primary; the 0.2-confidence
	// cascade impact is filtered out.
	s3, _ := f.store.FindActive(ctx, "MTA", "L", "S3", now)
	if len(s3) != 1 || s3[0].IsPrimary {
		t.Errorf("S3 pending = %+v, want 1 non-primary", s3)
	}
	if lowConf, _ := f.store.FindActive(ctx, "MTA", "L", "S1", now); len(lowConf) != 0 {
		t.Errorf("low-confidence impact should not be tracked: %v", lowConf)
	}
	// Cascade impact on G carries no direction.
	gPend, _ := f.store.FindActive(ctx, "MTA", "G", "S3", now)
	if len(gPend) != 1 || gPend[0].Direction != -1 {
		t.Errorf("G pending = %+v, want 1 with direction -1", gPend)
	}
}

func TestOnDelayEvent_MatchesAndRespectsDirection(t *testing.T) {
	f := newEvalFixture(t)
	ctx := context.Background()
	now := time.Now()

	src, res := testResult(now)
	if err := f.eval.TrackPrediction(ctx, src, res); err != nil {
		t.Fatal(err)
	}

	// Matching event on L at S2 (child platform normalizes to S2), dir 0.
	if err := f.eval.OnDelayEvent(ctx, &pb.DelayEvent{
		AgencyId: "MTA", TripId: "trip_L1", RouteId: "L", StopId: "S2N",
		DelaySeconds: 140, ObservedAt: now.Unix() + 300,
	}); err != nil {
		t.Fatal(err)
	}
	active, _ := f.store.FindActive(ctx, "MTA", "L", "S2", now)
	if len(active) != 1 || !active[0].Matched || active[0].ObservedMax != 140 {
		t.Fatalf("after match: %+v, want Matched with ObservedMax 140", active)
	}

	// Opposite-direction trip must not update the pending.
	if err := f.eval.OnDelayEvent(ctx, &pb.DelayEvent{
		AgencyId: "MTA", TripId: "trip_Lrev", RouteId: "L", StopId: "S2",
		DelaySeconds: 900, ObservedAt: now.Unix() + 400,
	}); err != nil {
		t.Fatal(err)
	}
	active, _ = f.store.FindActive(ctx, "MTA", "L", "S2", now)
	if active[0].ObservedMax != 140 {
		t.Errorf("opposite-direction event updated ObservedMax to %d", active[0].ObservedMax)
	}

	// Event outside the window (observed after expiry) must not match.
	if err := f.eval.OnDelayEvent(ctx, &pb.DelayEvent{
		AgencyId: "MTA", TripId: "trip_L1", RouteId: "L", StopId: "S2",
		DelaySeconds: 999, ObservedAt: now.Unix() + predictionWindowSecs + 60,
	}); err != nil {
		t.Fatal(err)
	}
	active, _ = f.store.FindActive(ctx, "MTA", "L", "S2", now)
	if active[0].ObservedMax != 140 {
		t.Errorf("out-of-window event updated ObservedMax to %d", active[0].ObservedMax)
	}
}

func TestSweep_WritesObservedOutcomesToHistory(t *testing.T) {
	f := newEvalFixture(t)
	ctx := context.Background()
	now := time.Now()

	verified := &PendingPrediction{
		ID: "A:L:S2:1:0", AgencyID: "MTA", FromRoute: "A", RouteID: "L", StationID: "S2",
		SourceStation: "S1",
		Direction:     0, SourceDelaySecs: 300, PredictedSecs: 100, Confidence: 1.0,
		Hops: 1, Hour: 8, IsPrimary: true,
		CreatedAt: now.Unix(), ExpiresAt: now.Unix() + 1,
		ObservedMax: 80, Matched: true,
	}
	falsified := &PendingPrediction{
		ID: "A:L:S3:2:0", AgencyID: "MTA", FromRoute: "A", RouteID: "L", StationID: "S3",
		Direction: 0, SourceDelaySecs: 300, PredictedSecs: 100, Confidence: 0.7,
		Hops: 1, Hour: 9, IsPrimary: true,
		CreatedAt: now.Unix(), ExpiresAt: now.Unix() + 1,
	}
	nonPrimary := &PendingPrediction{
		ID: "A:L:S4:3:0", AgencyID: "MTA", FromRoute: "A", RouteID: "L", StationID: "S4",
		Direction: 0, SourceDelaySecs: 300, PredictedSecs: 50, Confidence: 0.5,
		Hops: 2, Hour: 8,
		CreatedAt: now.Unix(), ExpiresAt: now.Unix() + 1,
	}
	nonPrimaryMatched := &PendingPrediction{
		ID: "A:G:S3:4:0", AgencyID: "MTA", FromRoute: "A", RouteID: "G", StationID: "S3",
		Direction: -1, SourceDelaySecs: 300, PredictedSecs: 50, Confidence: 0.5,
		Hops: 2, Hour: 8,
		CreatedAt: now.Unix(), ExpiresAt: now.Unix() + 1,
		ObservedMax: 40, Matched: true,
	}
	for _, p := range []*PendingPrediction{verified, falsified, nonPrimary, nonPrimaryMatched} {
		if err := f.store.Add(ctx, p); err != nil {
			t.Fatal(err)
		}
	}

	if err := f.eval.sweepOnce(ctx, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}

	// Verified primary → its observed outcome (80) lands in history at the
	// TRANSFER station (SourceStation), the key the engine reads.
	avg, count, err := f.hist.GetAverageImpact(ctx, "A", "L", "S1", 8)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || avg != 80 {
		t.Errorf("verified history = (%v, %d), want (80, 1)", avg, count)
	}
	if _, count, _ = f.hist.GetAverageImpact(ctx, "A", "L", "S2", 8); count != 0 {
		t.Error("history must not be keyed by the impacted stop when SourceStation is set")
	}

	// Falsified primary without SourceStation (pre-migration pending) →
	// observed outcome 0 lands at its StationID via the fallback.
	avg, count, err = f.hist.GetAverageImpact(ctx, "A", "L", "S3", 9)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || avg != 0 {
		t.Errorf("falsified history = (%v, %d), want (0, 1)", avg, count)
	}

	// Non-primary → finalized without touching the transfer-pair history,
	// but its outcome (0, falsified) lands in the per-stop store.
	if _, count, _ = f.hist.GetAverageImpact(ctx, "A", "L", "S4", 8); count != 0 {
		t.Error("non-primary prediction wrote history")
	}
	avg, count, err = f.stopStore.GetAverageStopImpact(ctx, "L", "S4", 8)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || avg != 0 {
		t.Errorf("falsified non-primary stop outcome = (%v, %d), want (0, 1)", avg, count)
	}

	// Matched non-primary records its observed value per-stop.
	avg, count, err = f.stopStore.GetAverageStopImpact(ctx, "G", "S3", 8)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 || avg != 40 {
		t.Errorf("matched non-primary stop outcome = (%v, %d), want (40, 1)", avg, count)
	}

	// Primaries never write the per-stop store.
	if _, count, _ = f.stopStore.GetAverageStopImpact(ctx, "L", "S2", 8); count != 0 {
		t.Error("primary prediction wrote per-stop store")
	}

	// Everything was claimed; nothing left to pop.
	if left, _ := f.store.PopExpired(ctx, now.Add(time.Hour), 10); len(left) != 0 {
		t.Errorf("unclaimed pendings remain: %v", left)
	}
}

func TestOnDelayEvent_IgnoresNonDelayTypes(t *testing.T) {
	f := newEvalFixture(t)
	ctx := context.Background()
	now := time.Now()

	src, res := testResult(now)
	if err := f.eval.TrackPrediction(ctx, src, res); err != nil {
		t.Fatal(err)
	}

	// A cancellation on the predicted route/station carries delay 0 and must
	// not be treated as an observed outcome.
	if err := f.eval.OnDelayEvent(ctx, &pb.DelayEvent{
		AgencyId: "MTA", TripId: "trip_L1", RouteId: "L", StopId: "S2",
		ObservedAt: now.Unix() + 300,
		Type:       pb.DelayEvent_TRIP_CANCELLED,
	}); err != nil {
		t.Fatal(err)
	}

	active, _ := f.store.FindActive(ctx, "MTA", "L", "S2", now)
	if len(active) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(active))
	}
	if active[0].Matched {
		t.Error("TRIP_CANCELLED must not match a pending prediction")
	}
}

// TestFeedbackLoop_EngineReadsWhatSweeperWrites is the closed-loop
// regression: predictions tracked from a transfer impact at station S1 must,
// after finalization, be readable at the exact history key the propagation
// engine queries (from-route, to-route, TRANSFER station, hour).
func TestFeedbackLoop_EngineReadsWhatSweeperWrites(t *testing.T) {
	f := newEvalFixture(t)
	ctx := context.Background()
	now := time.Now()

	src, res := testResult(now)
	if err := f.eval.TrackPrediction(ctx, src, res); err != nil {
		t.Fatal(err)
	}

	// Force expiry and finalize everything.
	if err := f.eval.sweepOnce(ctx, now.Add(predictionWindowSecs*time.Second+time.Minute)); err != nil {
		t.Fatal(err)
	}

	hour := time.Unix(res.GetComputedAt(), 0).Hour()
	_, count, err := f.hist.GetAverageImpact(ctx, "A", "L", "S1", hour)
	if err != nil {
		t.Fatal(err)
	}
	if count < 1 {
		t.Fatalf("engine's read key history:A:L:S1:%d has count %d, want >= 1 — feedback loop not closed", hour, count)
	}
}

func TestTrackPrediction_PrimarySurvivesCap(t *testing.T) {
	f := newEvalFixture(t)
	ctx := context.Background()
	now := time.Now()

	// maxTracked+5 impacts in descending-delay order, all above the
	// confidence filter, with the primary (smallest hop-1 delay on the
	// receiving route) last — a plain in-order loop would cut it at the cap.
	src, _ := testResult(now)
	res := &pb.PropagationResult{ComputedAt: now.Unix()}
	for i := 0; i < maxTracked+4; i++ {
		res.Impacts = append(res.Impacts, &pb.DownstreamImpact{
			RouteId: "G", StopId: fmt.Sprintf("X%d", i),
			PredictedAdditionalDelay: int32(2000 - i), Confidence: 0.7, HopsFromSource: 2,
		})
	}
	res.Impacts = append(res.Impacts, &pb.DownstreamImpact{
		RouteId: "L", StopId: "S2",
		PredictedAdditionalDelay: 300, Confidence: 1.0, HopsFromSource: 1,
	})

	if err := f.eval.TrackPrediction(ctx, src, res); err != nil {
		t.Fatal(err)
	}

	primary, _ := f.store.FindActive(ctx, "MTA", "L", "S2", now)
	if len(primary) != 1 || !primary[0].IsPrimary {
		t.Fatalf("primary was evicted by the cap: %v", primary)
	}

	total := 0
	for _, imp := range res.GetImpacts() {
		ps, _ := f.store.FindActive(ctx, "MTA", imp.GetRouteId(), imp.GetStopId(), now)
		total += len(ps)
	}
	if total > maxTracked {
		t.Errorf("tracked %d pendings, cap is %d", total, maxTracked)
	}
}

func TestTrackPrediction_UniqueIDsInSameSecond(t *testing.T) {
	f := newEvalFixture(t)
	ctx := context.Background()
	now := time.Now()

	// Two propagations of the same pair with identical ComputedAt (whole
	// seconds). Before the fix their pendings shared IDs and the second
	// silently overwrote the first.
	src, res := testResult(now)
	if err := f.eval.TrackPrediction(ctx, src, res); err != nil {
		t.Fatal(err)
	}
	if err := f.eval.TrackPrediction(ctx, src, res); err != nil {
		t.Fatal(err)
	}

	active, err := f.store.FindActive(ctx, "MTA", "L", "S2", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 2 {
		t.Fatalf("got %d pendings at L/S2, want 2 (same-second ID collision)", len(active))
	}
	if active[0].ID == active[1].ID {
		t.Error("pending IDs must be unique")
	}
}

func TestUpdate_DoesNotResurrectFinalized(t *testing.T) {
	f := newEvalFixture(t)
	ctx := context.Background()
	now := time.Now()

	p := &PendingPrediction{
		ID: "A:L:S2:9:0", AgencyID: "MTA", FromRoute: "A", RouteID: "L", StationID: "S2",
		Direction: 0, PredictedSecs: 300, Confidence: 1.0, Hops: 1, Hour: 8,
		CreatedAt: now.Unix(), ExpiresAt: now.Unix() + 1,
	}
	if err := f.store.Add(ctx, p); err != nil {
		t.Fatal(err)
	}

	// Sweeper claims and deletes it.
	popped, err := f.store.PopExpired(ctx, now.Add(30*time.Second), 10)
	if err != nil || len(popped) != 1 {
		t.Fatalf("PopExpired = (%v, %v), want 1", popped, err)
	}

	// A racing Update must be a no-op, not a resurrection.
	p.Matched = true
	p.ObservedMax = 500
	if err := f.store.Update(ctx, p); err != nil {
		t.Fatalf("Update after finalize must return nil, got %v", err)
	}
	if n, _ := f.rdb.Exists(ctx, "pending:pred:"+p.ID).Result(); n != 0 {
		t.Error("Update resurrected a finalized pending as an immortal key")
	}
}

func TestAdd_IndexKeyHasTTL(t *testing.T) {
	f := newEvalFixture(t)
	ctx := context.Background()
	now := time.Now()

	p := &PendingPrediction{
		ID: "A:L:S2:10:0", AgencyID: "MTA", FromRoute: "A", RouteID: "L", StationID: "S2",
		Direction: 0, PredictedSecs: 300, Confidence: 1.0, Hops: 1, Hour: 8,
		CreatedAt: now.Unix(), ExpiresAt: now.Unix() + 60,
	}
	if err := f.store.Add(ctx, p); err != nil {
		t.Fatal(err)
	}
	ttl, err := f.rdb.TTL(ctx, "pending:idx:MTA:L:S2").Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 {
		t.Errorf("index ZSET has no TTL (%v) — leaked members would live forever", ttl)
	}
}
