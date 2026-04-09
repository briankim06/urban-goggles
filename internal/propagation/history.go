package propagation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	historyRetentionDays = 30
	historyRetentionSecs = historyRetentionDays * 24 * 3600
)

// observation is the JSON payload stored in each sorted-set member.
type observation struct {
	SourceDelay      int `json:"source_delay"`
	DownstreamImpact int `json:"downstream_impact"`
}

// HistoricalStore records and queries downstream-impact observations for
// transfer pairs, backed by Redis sorted sets keyed by
// history:{fromRoute}:{toRoute}:{stationID}:{hourOfDay}.
type HistoricalStore struct {
	rdb *redis.Client
}

// NewHistoricalStore wraps an existing Redis client.
func NewHistoricalStore(rdb *redis.Client) *HistoricalStore {
	return &HistoricalStore{rdb: rdb}
}

func historyKey(fromRoute, toRoute, stationID string, hour int) string {
	return fmt.Sprintf("history:%s:%s:%s:%d", fromRoute, toRoute, stationID, hour)
}

// RecordObservation adds one data point to the sorted set, scored by the
// current Unix timestamp. After writing, entries older than 30 days are
// trimmed.
func (h *HistoricalStore) RecordObservation(
	ctx context.Context,
	fromRoute, toRoute, stationID string,
	hourOfDay, sourceDelay, downstreamImpact int,
) error {
	key := historyKey(fromRoute, toRoute, stationID, hourOfDay)
	now := time.Now().Unix()

	data, err := json.Marshal(observation{
		SourceDelay:      sourceDelay,
		DownstreamImpact: downstreamImpact,
	})
	if err != nil {
		return err
	}

	pipe := h.rdb.Pipeline()
	pipe.ZAdd(ctx, key, redis.Z{Score: float64(now), Member: string(data)})
	// Trim entries older than 30 days.
	cutoff := float64(now - historyRetentionSecs)
	pipe.ZRemRangeByScore(ctx, key, "-inf", fmt.Sprintf("%f", cutoff))
	_, err = pipe.Exec(ctx)
	return err
}

// GetAverageImpact returns the mean downstream impact and sample count from
// all observations in the last 30 days. If no data exists, avgImpact is 0 and
// count is 0.
func (h *HistoricalStore) GetAverageImpact(
	ctx context.Context,
	fromRoute, toRoute, stationID string,
	hourOfDay int,
) (avgImpact float64, count int, err error) {
	key := historyKey(fromRoute, toRoute, stationID, hourOfDay)
	cutoff := float64(time.Now().Unix() - historyRetentionSecs)

	members, err := h.rdb.ZRangeByScore(ctx, key, &redis.ZRangeBy{
		Min: fmt.Sprintf("%f", cutoff),
		Max: "+inf",
	}).Result()
	if err != nil {
		return 0, 0, err
	}
	if len(members) == 0 {
		return 0, 0, nil
	}

	var sum int
	for _, m := range members {
		var obs observation
		if err := json.Unmarshal([]byte(m), &obs); err != nil {
			continue
		}
		sum += obs.DownstreamImpact
		count++
	}
	if count == 0 {
		return 0, 0, nil
	}
	return float64(sum) / float64(count), count, nil
}

// GetTransferStats returns reliability-style stats for all transfer pairs at
// a station in a given hour. It scans for matching history keys.
func (h *HistoricalStore) GetTransferStats(
	ctx context.Context,
	stationID string,
	hourOfDay int,
) ([]TransferStat, error) {
	pattern := fmt.Sprintf("history:*:*:%s:%d", stationID, hourOfDay)
	var stats []TransferStat

	iter := h.rdb.Scan(ctx, 0, pattern, 100).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		// Parse fromRoute:toRoute from key.
		var from, to, station string
		var hour int
		if _, err := fmt.Sscanf(key, "history:%[^:]:%[^:]:%[^:]:%d", &from, &to, &station, &hour); err != nil {
			continue
		}

		avgImpact, count, err := h.GetAverageImpact(ctx, from, to, stationID, hourOfDay)
		if err != nil || count == 0 {
			continue
		}
		stats = append(stats, TransferStat{
			FromRouteID:       from,
			ToRouteID:         to,
			StationID:         stationID,
			AvgDelayWhenBroken: avgImpact,
			SampleCount:       count,
		})
	}
	return stats, iter.Err()
}

// TransferStat summarizes historical reliability for one transfer pair.
type TransferStat struct {
	FromRouteID        string
	ToRouteID          string
	StationID          string
	AvgDelayWhenBroken float64
	SampleCount        int
}
