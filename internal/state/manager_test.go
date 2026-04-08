package state

import (
	"context"
	"testing"

	"github.com/redis/go-redis/v9"

	transit "github.com/briankim06/urban-goggles/proto/transit"
)

func skipIfNoRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("Redis not available: %v", err)
	}
	return rdb
}

func TestProcessEvent_FirstObservation(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	m := NewDelayStateManager(rdb, nil, nil)
	ev := &transit.DelayEvent{
		AgencyId:     "test",
		TripId:       "t1",
		RouteId:      "A",
		StopId:       "s1",
		DelaySeconds: 60,
		ObservedAt:   1000,
	}
	if err := m.ProcessEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}

	ds, err := m.GetDelay(ctx, "test", "t1", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if ds.DelaySeconds != 60 {
		t.Errorf("delay = %d, want 60", ds.DelaySeconds)
	}
	if ds.Confidence != 0.5 {
		t.Errorf("confidence = %f, want 0.5", ds.Confidence)
	}
	if ds.UpdateCount != 1 {
		t.Errorf("update_count = %d, want 1", ds.UpdateCount)
	}
}

func TestProcessEvent_OutOfOrder(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	m := NewDelayStateManager(rdb, nil, nil)
	// First event at t=2000.
	m.ProcessEvent(ctx, &transit.DelayEvent{
		AgencyId: "test", TripId: "t1", RouteId: "A", StopId: "s1",
		DelaySeconds: 60, ObservedAt: 2000,
	})
	// Older event at t=1000 — should be dropped.
	m.ProcessEvent(ctx, &transit.DelayEvent{
		AgencyId: "test", TripId: "t1", RouteId: "A", StopId: "s1",
		DelaySeconds: 300, ObservedAt: 1000,
	})

	ds, _ := m.GetDelay(ctx, "test", "t1", "s1")
	if ds.DelaySeconds != 60 {
		t.Errorf("out-of-order not rejected: delay = %d, want 60", ds.DelaySeconds)
	}
}

func TestProcessEvent_Smoothing(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	m := NewDelayStateManager(rdb, nil, nil)
	// First observation: delay=60, update_count=1.
	m.ProcessEvent(ctx, &transit.DelayEvent{
		AgencyId: "test", TripId: "t1", RouteId: "A", StopId: "s1",
		DelaySeconds: 60, ObservedAt: 1000,
	})
	// Jump >120s with update_count=1 (< 3): smoothing should apply.
	// smoothed = 0.7*300 + 0.3*60 = 210+18 = 228
	m.ProcessEvent(ctx, &transit.DelayEvent{
		AgencyId: "test", TripId: "t1", RouteId: "A", StopId: "s1",
		DelaySeconds: 300, ObservedAt: 2000,
	})

	ds, _ := m.GetDelay(ctx, "test", "t1", "s1")
	if ds.DelaySeconds != 228 {
		t.Errorf("smoothing: delay = %d, want 228", ds.DelaySeconds)
	}
}

func TestProcessEvent_ConfidenceIncrease(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	m := NewDelayStateManager(rdb, nil, nil)
	m.ProcessEvent(ctx, &transit.DelayEvent{
		AgencyId: "test", TripId: "t1", RouteId: "A", StopId: "s1",
		DelaySeconds: 60, ObservedAt: 1000,
	})
	// Within ±30s → confidence should increase to 0.6.
	m.ProcessEvent(ctx, &transit.DelayEvent{
		AgencyId: "test", TripId: "t1", RouteId: "A", StopId: "s1",
		DelaySeconds: 65, ObservedAt: 2000,
	})

	ds, _ := m.GetDelay(ctx, "test", "t1", "s1")
	if ds.Confidence < 0.59 || ds.Confidence > 0.61 {
		t.Errorf("confidence = %f, want ~0.6", ds.Confidence)
	}
}

func TestGetAllActiveDelays(t *testing.T) {
	rdb := skipIfNoRedis(t)
	defer rdb.Close()
	ctx := context.Background()
	rdb.FlushDB(ctx)

	m := NewDelayStateManager(rdb, nil, nil)
	// Below threshold.
	m.ProcessEvent(ctx, &transit.DelayEvent{
		AgencyId: "test", TripId: "t1", RouteId: "A", StopId: "s1",
		DelaySeconds: 30, ObservedAt: 1000,
	})
	// Above threshold.
	m.ProcessEvent(ctx, &transit.DelayEvent{
		AgencyId: "test", TripId: "t2", RouteId: "L", StopId: "s2",
		DelaySeconds: 120, ObservedAt: 1000,
	})

	active, err := m.GetAllActiveDelays(ctx, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 {
		t.Fatalf("expected 1 active delay, got %d", len(active))
	}
	if active[0].TripID != "t2" {
		t.Errorf("expected trip t2, got %s", active[0].TripID)
	}
}
