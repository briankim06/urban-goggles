// Package evaluation closes the prediction feedback loop: it records
// propagation predictions as pending, matches them against actual delay
// events arriving later, and finalizes them as verified or falsified —
// feeding observed outcomes (never predictions) back into the historical
// store and exposing accuracy metrics.
package evaluation

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	pendingKeyPrefix = "pending:pred:"
	indexKeyPrefix   = "pending:idx:"
	expiryKey        = "pending:expiry"

	// pendingIndexTTL bounds index-ZSET leakage: members whose JSON key
	// TTL-evicted before sweeping cannot be cleaned individually (their
	// route/station live only in the JSON), so the whole index expires when
	// no pendings have been added for this long. Must exceed the prediction
	// window (30 min) plus the sweeper's slack.
	pendingIndexTTL = 50 * time.Minute
)

// PendingPrediction is one tracked downstream-impact prediction awaiting
// verification against observed delays.
type PendingPrediction struct {
	ID        string `json:"id"`
	AgencyID  string `json:"agency_id"`
	FromRoute string `json:"from_route"`
	RouteID   string `json:"route_id"`   // receiving route the impact was predicted on
	StationID string `json:"station_id"` // impacted stop, used for event matching
	// SourceStation is the transfer station the prediction originated from —
	// the station history is keyed by, matching the engine's read side.
	SourceStation string `json:"source_station,omitempty"`
	Direction     int    `json:"direction"` // -1 when unknown

	SourceDelaySecs int32   `json:"source_delay_secs"`
	PredictedSecs   int32   `json:"predicted_secs"`
	BaselineSecs    int32   `json:"baseline_secs"` // delay already present at prediction time
	Confidence      float64 `json:"confidence"`
	Hops            int32   `json:"hops"`
	Hour            int     `json:"hour"`       // hour-of-day at prediction time
	IsPrimary       bool    `json:"is_primary"` // hop-1 first-downstream impact → writes history

	CreatedAt   int64 `json:"created_at"`
	ExpiresAt   int64 `json:"expires_at"`
	ObservedMax int32 `json:"observed_max"`
	Matched     bool  `json:"matched"`
}

// PendingStore persists pending predictions in Redis:
//
//	pending:pred:{id}                       STRING JSON (TTL backstop)
//	pending:idx:{agency}:{route}:{station}  ZSET id → expires_at (match lookup)
//	pending:expiry                          ZSET id → expires_at (sweeper)
type PendingStore struct {
	rdb *redis.Client
}

// NewPendingStore wraps an existing Redis client.
func NewPendingStore(rdb *redis.Client) *PendingStore {
	return &PendingStore{rdb: rdb}
}

func pendingKey(id string) string { return pendingKeyPrefix + id }

func indexKey(agencyID, routeID, stationID string) string {
	return fmt.Sprintf("%s%s:%s:%s", indexKeyPrefix, agencyID, routeID, stationID)
}

// Add stores a pending prediction and registers it in the match index and
// the expiry set.
func (s *PendingStore) Add(ctx context.Context, p *PendingPrediction) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	ttl := time.Until(time.Unix(p.ExpiresAt, 0)) + 10*time.Minute
	if ttl <= 0 {
		return fmt.Errorf("pending prediction %s already expired", p.ID)
	}
	idxKey := indexKey(p.AgencyID, p.RouteID, p.StationID)
	pipe := s.rdb.Pipeline()
	pipe.Set(ctx, pendingKey(p.ID), data, ttl)
	pipe.ZAdd(ctx, idxKey, redis.Z{Score: float64(p.ExpiresAt), Member: p.ID})
	pipe.Expire(ctx, idxKey, pendingIndexTTL)
	pipe.ZAdd(ctx, expiryKey, redis.Z{Score: float64(p.ExpiresAt), Member: p.ID})
	_, err = pipe.Exec(ctx)
	return err
}

// FindActive returns all pending predictions for (agency, route, station)
// whose window has not yet expired at now.
func (s *PendingStore) FindActive(ctx context.Context, agencyID, routeID, stationID string, now time.Time) ([]*PendingPrediction, error) {
	ids, err := s.rdb.ZRangeByScore(ctx, indexKey(agencyID, routeID, stationID), &redis.ZRangeBy{
		Min: fmt.Sprintf("%d", now.Unix()),
		Max: "+inf",
	}).Result()
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	var out []*PendingPrediction
	for _, id := range ids {
		p, err := s.get(ctx, id)
		if err != nil {
			continue // expired between index read and get
		}
		out = append(out, p)
	}
	return out, nil
}

// Update rewrites a pending prediction in place, preserving its TTL. The
// write is existence-guarded (SET XX): if the sweeper finalized and deleted
// the pending between FindActive and Update, recreating it here would leave
// an un-indexed key with no TTL — instead the late observation is dropped,
// which is correct for an already-finalized prediction.
func (s *PendingStore) Update(ctx context.Context, p *PendingPrediction) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	err = s.rdb.SetArgs(ctx, pendingKey(p.ID), data, redis.SetArgs{
		Mode:    "XX",
		KeepTTL: true,
	}).Err()
	if err == redis.Nil {
		return nil // key gone: pending already finalized
	}
	return err
}

// PopExpired atomically claims up to limit predictions whose window ended at
// or before now, removing them from the expiry set and match index. The
// returned predictions are ready to be finalized.
func (s *PendingStore) PopExpired(ctx context.Context, now time.Time, limit int) ([]*PendingPrediction, error) {
	ids, err := s.rdb.ZRangeByScore(ctx, expiryKey, &redis.ZRangeBy{
		Min:   "-inf",
		Max:   fmt.Sprintf("%d", now.Unix()),
		Count: int64(limit),
	}).Result()
	if err != nil || len(ids) == 0 {
		return nil, err
	}

	var out []*PendingPrediction
	for _, id := range ids {
		// Claim: only the caller that removes the id from the expiry set
		// finalizes it (guards against concurrent sweepers).
		removed, err := s.rdb.ZRem(ctx, expiryKey, id).Result()
		if err != nil || removed == 0 {
			continue
		}
		p, err := s.get(ctx, id)
		if err != nil {
			continue // JSON key already TTL-evicted; nothing to finalize
		}
		pipe := s.rdb.Pipeline()
		pipe.ZRem(ctx, indexKey(p.AgencyID, p.RouteID, p.StationID), id)
		pipe.Del(ctx, pendingKey(id))
		_, _ = pipe.Exec(ctx)
		out = append(out, p)
	}
	return out, nil
}

func (s *PendingStore) get(ctx context.Context, id string) (*PendingPrediction, error) {
	data, err := s.rdb.Get(ctx, pendingKey(id)).Bytes()
	if err != nil {
		return nil, err
	}
	var p PendingPrediction
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return &p, nil
}
