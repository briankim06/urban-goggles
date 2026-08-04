package transfer

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/briankim06/urban-goggles/internal/graph"
	"github.com/briankim06/urban-goggles/internal/state"
	pb "github.com/briankim06/urban-goggles/proto/transit"
)

const (
	atRiskBuffer = 60 // seconds added to min_transfer_time for AT_RISK
)

// Publisher is the interface for emitting TransferImpact events downstream.
type Publisher interface {
	PublishImpact(ctx context.Context, impact *pb.TransferImpact) error
}

// TransferDetector evaluates delay events against the static schedule graph
// to detect broken and at-risk transfer connections.
type TransferDetector struct {
	graph     *graph.TransitGraph
	stateMgr  *state.DelayStateManager
	publisher Publisher
	logger    *slog.Logger
}

// NewTransferDetector constructs a detector. publisher may be nil if impacts
// should not be published (e.g. in tests).
func NewTransferDetector(
	g *graph.TransitGraph,
	mgr *state.DelayStateManager,
	pub Publisher,
	logger *slog.Logger,
) *TransferDetector {
	if logger == nil {
		logger = slog.Default()
	}
	return &TransferDetector{
		graph:     g,
		stateMgr:  mgr,
		publisher: pub,
		logger:    logger,
	}
}

// EvaluateDelay checks every outbound transfer from the delayed stop and
// returns any that are broken or at risk.
func (d *TransferDetector) EvaluateDelay(ctx context.Context, ev *pb.DelayEvent) ([]*pb.TransferImpact, error) {
	if ev.GetDelaySeconds() <= 0 {
		return nil, nil // no delay, no broken transfers
	}

	srcParent := d.graph.ParentStationID(ev.GetStopId())
	srcRoute := ev.GetRouteId()

	transfers := d.graph.GetTransfersFrom(ev.GetStopId())
	if len(transfers) == 0 {
		return nil, nil
	}

	// Time-of-day in seconds since midnight derived from the event's
	// scheduled arrival (or observed_at as fallback).
	scheduledArrSecs := d.todFromUnix(ev.GetScheduledArrival())
	if scheduledArrSecs == 0 {
		scheduledArrSecs = d.todFromUnix(ev.GetObservedAt())
	}

	var impacts []*pb.TransferImpact

	for _, xfer := range transfers {
		destStation := xfer.ToStopID
		minXferTime := xfer.MinTransferTime
		if minXferTime <= 0 {
			minXferTime = 120 // default 2 min if unspecified
		}

		// Determine which routes we check at the destination station.
		destRoutes := d.graph.GetRoutesAtStop(destStation)
		if len(destRoutes) == 0 {
			continue
		}

		for toRoute := range destRoutes {
			// Skip same route (you don't "transfer" to yourself).
			// For self-transfers (srcParent == destStation), also skip same route.
			if toRoute == srcRoute {
				continue
			}

			// Resolve the effective frame (early-morning arrivals may
			// connect to the previous service day's 24:xx+ trips), then
			// take the first departure the passenger could viably make per
			// schedule: margin >= minXfer by construction, so a schedule
			// artifact that was never a real connection cannot be "broken".
			_, effTod := d.graph.NextDeparturesAfter(destStation, toRoute, scheduledArrSecs, 1)
			deps := d.graph.GetNextDepartures(destStation, toRoute, effTod+minXferTime, 3)
			if len(deps) == 0 {
				continue // no viable service on connecting route
			}
			nextDep := deps[0]

			originalMargin := int32(nextDep.DepartureSecs - effTod)

			// The connection is a specific trip — use its observed delay,
			// signed: a late connecting train widens the effective margin,
			// an early-running one tightens it.
			connectingDelay := d.tripDelayAt(ctx, ev.GetAgencyId(), nextDep.TripID, destStation)
			remainingMargin := originalMargin - ev.GetDelaySeconds() + connectingDelay

			var level pb.TransferImpact_ImpactLevel
			if remainingMargin < int32(minXferTime) {
				level = pb.TransferImpact_BROKEN
			} else if remainingMargin < int32(minXferTime+atRiskBuffer) {
				level = pb.TransferImpact_AT_RISK
			} else {
				continue // transfer is fine
			}

			// For broken transfers, find next viable departure and record
			// the schedule frame so the evaluator can later compute the
			// passenger's realized wait.
			var nextViableTripID string
			var additionalWait, schedConnDep, earliestCatchSecs int32
			if level == pb.TransferImpact_BROKEN {
				// earliestCatch is already in the effective frame, so the
				// plain lookup is correct even past 86400.
				predictedArrSecs := effTod + int(ev.GetDelaySeconds())
				earliestCatch := predictedArrSecs + minXferTime
				schedConnDep = int32(nextDep.DepartureSecs)
				earliestCatchSecs = int32(earliestCatch)
				viableDeps := d.graph.GetNextDepartures(destStation, toRoute, earliestCatch, 1)
				if len(viableDeps) > 0 {
					nextViableTripID = viableDeps[0].TripID
					additionalWait = int32(viableDeps[0].DepartureSecs - nextDep.DepartureSecs)
					if additionalWait < 0 {
						additionalWait = 0
					}
				}
			}

			stationName := destStation
			if s, ok := d.graph.Stops[destStation]; ok {
				stationName = s.Name
			} else if s, ok := d.graph.Stops[srcParent]; ok {
				stationName = s.Name
			}

			impact := &pb.TransferImpact{
				FromTripId:                    ev.GetTripId(),
				FromRouteId:                   srcRoute,
				ToRouteId:                     toRoute,
				StationId:                     destStation,
				SourceDelaySeconds:            ev.GetDelaySeconds(),
				OriginalTransferMarginSeconds: originalMargin,
				RemainingMarginSeconds:        remainingMargin,
				Level:                         level,
				DetectedAt:                    time.Now().Unix(),
				NextViableTripId:              nextViableTripID,
				AdditionalWaitSeconds:         additionalWait,
				SchedConnectionDepartureSecs:  schedConnDep,
				EarliestCatchSecs:             earliestCatchSecs,
			}
			impacts = append(impacts, impact)

			levelStr := "AT_RISK"
			if level == pb.TransferImpact_BROKEN {
				levelStr = "BROKEN"
			}
			d.logger.Info(fmt.Sprintf("[%s] %s→%s at %s | %s train %ds late | next %s in %ds (+%ds wait)",
				levelStr, srcRoute, toRoute, stationName,
				srcRoute, ev.GetDelaySeconds(),
				toRoute, originalMargin, additionalWait),
			)
		}
	}

	// Publish all impacts.
	if d.publisher != nil {
		for _, imp := range impacts {
			if err := d.publisher.PublishImpact(ctx, imp); err != nil {
				d.logger.Error("publish impact", "err", err)
			}
		}
	}

	return impacts, nil
}

// tripDelayAt returns the observed signed delay of a specific trip at a
// station, 0 when no observation exists. One GET — replaces the previous
// route-wide SCAN per transfer pair.
func (d *TransferDetector) tripDelayAt(ctx context.Context, agencyID, tripID, stationID string) int32 {
	row, ok := d.graph.GetScheduledStopTime(tripID, stationID)
	if !ok {
		return 0
	}
	ds, err := d.stateMgr.GetDelay(ctx, agencyID, tripID, row.StopID)
	if err != nil || ds == nil {
		return 0
	}
	return ds.DelaySeconds
}

// todFromUnix converts a Unix timestamp to seconds since midnight in the
// agency's timezone. Returns 0 if ts is 0 (unset).
func (d *TransferDetector) todFromUnix(ts int64) int {
	if ts == 0 {
		return 0
	}
	return d.graph.SecsSinceMidnight(ts)
}
