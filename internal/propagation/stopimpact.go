package propagation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// stopOutcome is the JSON payload stored per observation. At (nanoseconds)
// makes each member unique — sorted-set members dedupe, so identical
// outcomes would otherwise collapse into one and corrupt sample counts.
type stopOutcome struct {
	Observed int   `json:"observed"`
	At       int64 `json:"at"`
}

// StopImpactStore records observed per-stop delay outcomes, backed by Redis
// sorted sets keyed stopimpact:{routeID}:{stationID}:{hourOfDay}. It lets the
// engine replace its uniform dwell model with locally observed impacts where
// enough data exists.
type StopImpactStore struct {
	rdb *redis.Client
}

// NewStopImpactStore wraps an existing Redis client.
func NewStopImpactStore(rdb *redis.Client) *StopImpactStore {
	return &StopImpactStore{rdb: rdb}
}

func stopImpactKey(routeID, stationID string, hour int) string {
	return fmt.Sprintf("stopimpact:%s:%s:%d", routeID, stationID, hour)
}

// RecordStopOutcome adds one observed outcome, scored by the current Unix
// timestamp, trimming entries older than the retention window.
func (s *StopImpactStore) RecordStopOutcome(
	ctx context.Context,
	routeID, stationID string,
	hour, observedDelay int,
) error {
	key := stopImpactKey(routeID, stationID, hour)
	now := time.Now().Unix()

	data, err := json.Marshal(stopOutcome{Observed: observedDelay, At: time.Now().UnixNano()})
	if err != nil {
		return err
	}

	pipe := s.rdb.Pipeline()
	pipe.ZAdd(ctx, key, redis.Z{Score: float64(now), Member: string(data)})
	cutoff := float64(now - historyRetentionSecs)
	pipe.ZRemRangeByScore(ctx, key, "-inf", fmt.Sprintf("%f", cutoff))
	_, err = pipe.Exec(ctx)
	return err
}

// GetAverageStopImpact returns the mean observed outcome and sample count for
// the (route, station, hour) bucket over the retention window. avg is 0 and
// count is 0 when no data exists.
func (s *StopImpactStore) GetAverageStopImpact(
	ctx context.Context,
	routeID, stationID string,
	hour int,
) (avg float64, count int, err error) {
	key := stopImpactKey(routeID, stationID, hour)
	cutoff := float64(time.Now().Unix() - historyRetentionSecs)

	members, err := s.rdb.ZRangeByScore(ctx, key, &redis.ZRangeBy{
		Min: fmt.Sprintf("%f", cutoff),
		Max: "+inf",
	}).Result()
	if err != nil {
		return 0, 0, err
	}

	var sum int
	for _, m := range members {
		var o stopOutcome
		if err := json.Unmarshal([]byte(m), &o); err != nil {
			continue
		}
		sum += o.Observed
		count++
	}
	if count == 0 {
		return 0, 0, nil
	}
	return float64(sum) / float64(count), count, nil
}
