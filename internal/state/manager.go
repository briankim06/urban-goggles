package state

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/briankim06/urban-goggles/internal/graph"
	"github.com/briankim06/urban-goggles/internal/metrics"
	transit "github.com/briankim06/urban-goggles/proto/transit"
)

const (
	keyTTL               = 2 * time.Hour
	smoothingThreshold   = 120 // seconds
	smoothingAlpha       = 0.7
	smoothingMaxUpdates  = 3
	confidenceInit       = 0.5
	confidenceStep       = 0.1
	confidenceMax        = 1.0
	confidenceCloseDelta = 30 // seconds
	activeDelayThreshold = 60 // seconds
)

// DelayState is the JSON value stored per key in Redis.
type DelayState struct {
	AgencyID     string  `json:"agency_id"`
	TripID       string  `json:"trip_id"`
	RouteID      string  `json:"route_id"`
	StopID       string  `json:"stop_id"`
	DelaySeconds int32   `json:"delay_seconds"`
	ObservedAt   int64   `json:"observed_at"`
	Confidence   float64 `json:"confidence"`
	UpdateCount  int     `json:"update_count"`
}

// DelayStateManager maintains reconciled delay state in Redis.
type DelayStateManager struct {
	rdb    *redis.Client
	graph  *graph.TransitGraph
	logger *slog.Logger
}

// NewDelayStateManager creates a manager backed by the given Redis client.
func NewDelayStateManager(rdb *redis.Client, g *graph.TransitGraph, logger *slog.Logger) *DelayStateManager {
	if logger == nil {
		logger = slog.Default()
	}
	return &DelayStateManager{rdb: rdb, graph: g, logger: logger}
}

func delayKey(agencyID, tripID, stopID string) string {
	return fmt.Sprintf("delay:%s:%s:%s", agencyID, tripID, stopID)
}

// ProcessEvent ingests a single DelayEvent, applying out-of-order rejection,
// exponential smoothing, and confidence scoring before writing to Redis.
func (m *DelayStateManager) ProcessEvent(ctx context.Context, ev *transit.DelayEvent) error {
	key := delayKey(ev.GetAgencyId(), ev.GetTripId(), ev.GetStopId())

	existing, err := m.getState(ctx, key)
	if err != nil && err != redis.Nil {
		return fmt.Errorf("get state %s: %w", key, err)
	}

	routeID := ev.GetRouteId()
	if routeID == "" {
		routeID = m.graph.TripRoute[ev.GetTripId()]
	}

	if existing == nil {
		// First observation.
		return m.setState(ctx, key, &DelayState{
			AgencyID:     ev.GetAgencyId(),
			TripID:       ev.GetTripId(),
			RouteID:      routeID,
			StopID:       ev.GetStopId(),
			DelaySeconds: ev.GetDelaySeconds(),
			ObservedAt:   ev.GetObservedAt(),
			Confidence:   confidenceInit,
			UpdateCount:  1,
		})
	}

	// Out-of-order rejection.
	if ev.GetObservedAt() < existing.ObservedAt {
		m.logger.Debug("out-of-order event dropped",
			"key", key,
			"event_ts", ev.GetObservedAt(),
			"stored_ts", existing.ObservedAt,
		)
		return nil
	}

	incoming := ev.GetDelaySeconds()
	stored := existing.DelaySeconds

	// Smoothing: large jump with few observations → dampen.
	finalDelay := incoming
	if abs32(incoming-stored) > smoothingThreshold && existing.UpdateCount < smoothingMaxUpdates {
		finalDelay = int32(math.Round(smoothingAlpha*float64(incoming) + (1-smoothingAlpha)*float64(stored)))
		m.logger.Debug("smoothing applied",
			"key", key,
			"incoming", incoming,
			"stored", stored,
			"smoothed", finalDelay,
		)
	}

	// Confidence scoring.
	conf := existing.Confidence
	if abs32(incoming-stored) <= confidenceCloseDelta {
		conf += confidenceStep
		if conf > confidenceMax {
			conf = confidenceMax
		}
	} else {
		conf = confidenceInit
	}

	return m.setState(ctx, key, &DelayState{
		AgencyID:     ev.GetAgencyId(),
		TripID:       ev.GetTripId(),
		RouteID:      routeID,
		StopID:       ev.GetStopId(),
		DelaySeconds: finalDelay,
		ObservedAt:   ev.GetObservedAt(),
		Confidence:   conf,
		UpdateCount:  existing.UpdateCount + 1,
	})
}

// GetDelay reads the current delay state for a specific trip+stop.
func (m *DelayStateManager) GetDelay(ctx context.Context, agencyID, tripID, stopID string) (*DelayState, error) {
	return m.getState(ctx, delayKey(agencyID, tripID, stopID))
}

// GetRouteDelays scans Redis for all delay keys belonging to a route. It uses
// SCAN to avoid blocking Redis and filters by route via the graph's TripRoute
// mapping.
func (m *DelayStateManager) GetRouteDelays(ctx context.Context, agencyID, routeID string) ([]*DelayState, error) {
	pattern := fmt.Sprintf("delay:%s:*", agencyID)
	var out []*DelayState

	iter := m.rdb.Scan(ctx, 0, pattern, 200).Iterator()
	for iter.Next(ctx) {
		raw, err := m.rdb.Get(ctx, iter.Val()).Bytes()
		if err != nil {
			continue
		}
		var ds DelayState
		if err := json.Unmarshal(raw, &ds); err != nil {
			continue
		}
		if ds.RouteID == routeID {
			out = append(out, &ds)
		}
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("scan route delays: %w", err)
	}
	return out, nil
}

// GetAllActiveDelays returns every delay above the active threshold (60s).
func (m *DelayStateManager) GetAllActiveDelays(ctx context.Context, agencyID string) ([]*DelayState, error) {
	pattern := fmt.Sprintf("delay:%s:*", agencyID)
	var out []*DelayState

	iter := m.rdb.Scan(ctx, 0, pattern, 200).Iterator()
	for iter.Next(ctx) {
		raw, err := m.rdb.Get(ctx, iter.Val()).Bytes()
		if err != nil {
			continue
		}
		var ds DelayState
		if err := json.Unmarshal(raw, &ds); err != nil {
			continue
		}
		if abs32(ds.DelaySeconds) > activeDelayThreshold {
			out = append(out, &ds)
		}
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("scan active delays: %w", err)
	}
	return out, nil
}

// --- internal helpers ---

func (m *DelayStateManager) getState(ctx context.Context, key string) (*DelayState, error) {
	start := time.Now()
	raw, err := m.rdb.Get(ctx, key).Bytes()
	metrics.RedisOperationDuration.WithLabelValues("get").Observe(time.Since(start).Seconds())
	if err != nil {
		return nil, err
	}
	var ds DelayState
	if err := json.Unmarshal(raw, &ds); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", key, err)
	}
	return &ds, nil
}

func (m *DelayStateManager) setState(ctx context.Context, key string, ds *DelayState) error {
	data, err := json.Marshal(ds)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	start := time.Now()
	err = m.rdb.Set(ctx, key, data, keyTTL).Err()
	metrics.RedisOperationDuration.WithLabelValues("set").Observe(time.Since(start).Seconds())
	return err
}

func abs32(x int32) int32 {
	if x < 0 {
		return -x
	}
	return x
}
