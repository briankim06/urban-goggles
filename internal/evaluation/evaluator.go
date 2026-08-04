package evaluation

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync/atomic"
	"time"

	"github.com/briankim06/urban-goggles/internal/graph"
	"github.com/briankim06/urban-goggles/internal/metrics"
	"github.com/briankim06/urban-goggles/internal/state"
	pb "github.com/briankim06/urban-goggles/proto/transit"
)

const (
	// predictionWindowSecs is how long a prediction stays live waiting for a
	// matching observed delay before it is finalized.
	predictionWindowSecs = 1800
	// minTrackConfidence excludes low-confidence tail impacts from tracking.
	minTrackConfidence = 0.3
	// maxTracked caps pendings per propagation result.
	maxTracked = 20
	// maxBaselineRoutes caps GetRouteDelays scans per propagation result.
	maxBaselineRoutes = 3
	// verifyMinSeconds and verifyRatio define verification: the observed
	// additional delay must be material and at least half the prediction.
	verifyMinSeconds = 15
	verifyRatio      = 0.5
	// sweepBatchSize bounds how many expired predictions one sweep finalizes.
	sweepBatchSize = 100
)

// pendingSeq disambiguates pending IDs created within the same nanosecond.
var pendingSeq atomic.Uint64

// HistoryRecorder is the slice of propagation.HistoricalStore the evaluator
// needs; observed transfer-pair outcomes are recorded through it.
type HistoryRecorder interface {
	RecordObservation(ctx context.Context, fromRoute, toRoute, stationID string, hourOfDay, sourceDelay, downstreamImpact int) error
}

// StopOutcomeRecorder is the slice of propagation.StopImpactStore the
// evaluator needs; observed per-stop outcomes are recorded through it.
type StopOutcomeRecorder interface {
	RecordStopOutcome(ctx context.Context, routeID, stationID string, hour, observedDelay int) error
}

// Evaluator closes the loop between published predictions and observed
// delays: TrackPrediction registers pendings, OnDelayEvent matches incoming
// events against them, and the sweeper finalizes expired pendings into
// metrics and historical observations.
type Evaluator struct {
	pending     *PendingStore
	graph       *graph.TransitGraph
	state       *state.DelayStateManager
	history     HistoryRecorder
	stopHistory StopOutcomeRecorder
	agencyID    string
	logger      *slog.Logger
}

// NewEvaluator wires an evaluator to the pending store, graph, live delay
// state, and historical stores. stopHistory may be nil (per-stop outcome
// recording disabled).
func NewEvaluator(
	pending *PendingStore,
	g *graph.TransitGraph,
	mgr *state.DelayStateManager,
	history HistoryRecorder,
	stopHistory StopOutcomeRecorder,
	agencyID string,
	logger *slog.Logger,
) *Evaluator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Evaluator{
		pending:     pending,
		graph:       g,
		state:       mgr,
		history:     history,
		stopHistory: stopHistory,
		agencyID:    agencyID,
		logger:      logger,
	}
}

// TrackPrediction records the impacts of a propagation result as pending
// predictions awaiting verification. Impacts below minTrackConfidence are
// skipped, and at most maxTracked are stored. The hop-1 impact with the
// smallest predicted delay on the receiving route (the engine's initial-delay
// estimate, before dwell accumulation) is flagged primary — only it feeds the
// historical store on finalization.
func (e *Evaluator) TrackPrediction(ctx context.Context, src *pb.TransferImpact, res *pb.PropagationResult) error {
	now := time.Unix(res.GetComputedAt(), 0)
	if res.GetComputedAt() == 0 {
		now = time.Now()
	}
	// History buckets by agency-local hour, matching the engine's read side.
	hour := now.In(e.graph.Location()).Hour()

	// Direction of the receiving route, when derivable.
	toDirection := -1
	if tripID := src.GetNextViableTripId(); tripID != "" {
		if d, ok := e.graph.TripDirection[tripID]; ok {
			toDirection = d
		}
	}

	primaryIdx := -1
	var primaryDelay int32
	for i, imp := range res.GetImpacts() {
		if imp.GetHopsFromSource() != 1 || imp.GetRouteId() != src.GetToRouteId() {
			continue
		}
		if primaryIdx == -1 || imp.GetPredictedAdditionalDelay() < primaryDelay {
			primaryIdx = i
			primaryDelay = imp.GetPredictedAdditionalDelay()
		}
	}

	baselines := e.baselineDelays(ctx, res)

	// Track the primary first: impacts arrive in descending-delay order and
	// the primary is the smallest hop-1 delay, so a plain in-order loop
	// could evict it at the cap — exactly during large disruptions.
	order := make([]int, 0, len(res.GetImpacts()))
	if primaryIdx >= 0 {
		order = append(order, primaryIdx)
	}
	for i := range res.GetImpacts() {
		if i != primaryIdx {
			order = append(order, i)
		}
	}

	tracked := 0
	var firstErr error
	for _, i := range order {
		imp := res.GetImpacts()[i]
		if tracked >= maxTracked {
			break
		}
		if float64(imp.GetConfidence()) < minTrackConfidence && i != primaryIdx {
			continue
		}

		direction := -1
		if imp.GetRouteId() == src.GetToRouteId() {
			direction = toDirection
		}

		// ID uniqueness needs real wall-clock nanos plus a sequence: `now`
		// derives from ComputedAt (whole seconds), so same-second
		// propagations of one pair would otherwise collide and silently
		// overwrite each other's pendings.
		p := &PendingPrediction{
			ID: fmt.Sprintf("%s:%s:%s:%d:%d",
				src.GetFromRouteId(), imp.GetRouteId(), imp.GetStopId(),
				time.Now().UnixNano(), pendingSeq.Add(1)),
			AgencyID:        e.agencyID,
			FromRoute:       src.GetFromRouteId(),
			RouteID:         imp.GetRouteId(),
			StationID:       imp.GetStopId(), // engine records parent station IDs
			SourceStation:   e.graph.ParentStationID(src.GetStationId()),
			Direction:       direction,
			SourceDelaySecs:   src.GetSourceDelaySeconds(),
			PredictedSecs:     imp.GetPredictedAdditionalDelay(),
			BaselineSecs:      baselines[imp.GetRouteId()+":"+imp.GetStopId()],
			SchedConnDepSecs:  src.GetSchedConnectionDepartureSecs(),
			EarliestCatchSecs: src.GetEarliestCatchSecs(),
			Confidence:      float64(imp.GetConfidence()),
			Hops:            imp.GetHopsFromSource(),
			Hour:            hour,
			IsPrimary:       i == primaryIdx,
			CreatedAt:       now.Unix(),
			ExpiresAt:       now.Unix() + predictionWindowSecs,
		}
		if err := e.pending.Add(ctx, p); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		metrics.EvalPredictionsTracked.Inc()
		tracked++
	}
	return firstErr
}

// baselineDelays captures the delay already present on each impacted
// (route, station) at prediction time, so matching can attribute only the
// *additional* delay to the cascade. Scans are capped at maxBaselineRoutes
// routes; stations on uncaptured routes get baseline 0.
func (e *Evaluator) baselineDelays(ctx context.Context, res *pb.PropagationResult) map[string]int32 {
	out := make(map[string]int32)
	seen := make(map[string]bool)
	for _, imp := range res.GetImpacts() {
		route := imp.GetRouteId()
		if seen[route] {
			continue
		}
		if len(seen) >= maxBaselineRoutes {
			break
		}
		seen[route] = true

		delays, err := e.state.GetRouteDelays(ctx, e.agencyID, route)
		if err != nil {
			continue
		}
		for _, ds := range delays {
			key := route + ":" + e.graph.ParentStationID(ds.StopID)
			if ds.DelaySeconds > out[key] {
				out[key] = ds.DelaySeconds
			}
		}
	}
	return out
}

// OnDelayEvent matches an observed delay event against pending predictions
// for the same route and station, updating their observed maximum.
func (e *Evaluator) OnDelayEvent(ctx context.Context, ev *pb.DelayEvent) error {
	if t := ev.GetType(); t != pb.DelayEvent_ARRIVAL_DELAY && t != pb.DelayEvent_DEPARTURE_DELAY {
		return nil // cancellations/skips carry delay 0 and must not falsify pendings
	}
	routeID := ev.GetRouteId()
	if routeID == "" {
		routeID = e.graph.TripRoute[ev.GetTripId()]
	}
	if routeID == "" {
		return nil
	}
	station := e.graph.ParentStationID(ev.GetStopId())
	now := time.Now()

	pendings, err := e.pending.FindActive(ctx, e.agencyID, routeID, station, now)
	if err != nil || len(pendings) == 0 {
		return err
	}

	// No locking on the read-modify-write below: delay-events is partitioned
	// by route_id and pendings are matched by route, so every event that can
	// touch a given pending arrives on one partition and is processed
	// sequentially.
	evDir, evDirKnown := e.graph.TripDirection[ev.GetTripId()]
	var firstErr error
	for _, p := range pendings {
		if ev.GetObservedAt() < p.CreatedAt || ev.GetObservedAt() > p.ExpiresAt {
			continue
		}
		if p.Direction >= 0 && evDirKnown && evDir != p.Direction {
			continue
		}
		additional := ev.GetDelaySeconds() - p.BaselineSecs
		if additional < 0 {
			additional = 0
		}
		if !p.Matched || additional > p.ObservedMax {
			p.Matched = true
			p.ObservedMax = additional
			// A transient failure on one pending must not abandon the rest.
			if err := e.pending.Update(ctx, p); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// RunSweeper finalizes expired predictions on the given interval until ctx
// is cancelled.
func (e *Evaluator) RunSweeper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.sweepOnce(ctx, time.Now()); err != nil {
				e.logger.Warn("evaluation sweep", "err", err)
			}
		}
	}
}

// realizedWait computes the passenger-experienced additional wait for a
// primary pending: the first train on the receiving route that was actually
// boardable (scheduled departure plus its observed delay, at or after the
// passenger's earliest catchable time) versus the scheduled connection. A
// candidate with no delay observation ran on time — absence of train delay
// is signal here, not a falsification. A cancelled candidate leaves no
// delay keys and counts as on-time-boardable — accepted first-order error.
// Returns ok=false when no boardable candidate exists within the horizon.
func (e *Evaluator) realizedWait(ctx context.Context, p *PendingPrediction) (int32, bool) {
	// SchedConnDepSecs is already in the detector's effective frame, so the
	// plain schedule lookup is correct even past 86400.
	deps := e.graph.GetNextDepartures(p.SourceStation, p.RouteID, int(p.SchedConnDepSecs), 10)
	horizon := int(p.EarliestCatchSecs) + predictionWindowSecs
	for _, st := range deps {
		if st.DepartureSecs > horizon {
			break
		}
		if p.Direction >= 0 {
			if d, ok := e.graph.TripDirection[st.TripID]; ok && d != p.Direction {
				continue
			}
		}
		var delay int32
		if row, ok := e.graph.GetScheduledStopTime(st.TripID, p.SourceStation); ok {
			if ds, err := e.state.GetDelay(ctx, p.AgencyID, st.TripID, row.StopID); err == nil && ds != nil {
				delay = ds.DelaySeconds
			}
		}
		realized := st.DepartureSecs + int(delay)
		if realized >= int(p.EarliestCatchSecs) {
			wait := int32(realized) - p.SchedConnDepSecs
			if wait < 0 {
				wait = 0
			}
			return wait, true
		}
	}
	return 0, false
}

// sweepOnce pops expired predictions and finalizes them: outcome metrics for
// all, plus a historical observation for primaries. A falsified prediction
// records its (possibly zero) observed outcome — real outcomes, not
// predictions, are what the history store must contain.
func (e *Evaluator) sweepOnce(ctx context.Context, now time.Time) error {
	expired, err := e.pending.PopExpired(ctx, now, sweepBatchSize)
	if err != nil {
		return err
	}
	for _, p := range expired {
		// Primaries predict passenger wait, so their outcome is the
		// realized wait, not observed train delay. Pendings without a
		// schedule frame (pre-migration, or self-propagation impacts where
		// the train carries its own delay) keep the event-matched outcome.
		if p.IsPrimary && p.SchedConnDepSecs > 0 {
			if wait, ok := e.realizedWait(ctx, p); ok {
				p.ObservedMax = wait
				p.Matched = true
			}
		}

		verified := p.ObservedMax >= verifyMinSeconds &&
			float64(p.ObservedMax) >= verifyRatio*float64(p.PredictedSecs)
		outcome := "falsified"
		if verified {
			outcome = "verified"
		}

		absErr := math.Abs(float64(p.PredictedSecs - p.ObservedMax))
		metrics.EvalPredictionsFinalized.WithLabelValues(outcome).Inc()
		metrics.EvalPredictionAbsError.Observe(absErr)
		metrics.EvalPredictionSquaredError.Add(absErr * absErr)
		metrics.EvalPredictionsByConfidence.WithLabelValues(ConfidenceBucket(p.Confidence), outcome).Inc()

		if p.IsPrimary {
			// History is keyed by the transfer station — the same key the
			// engine reads. Fall back to the impacted stop for pendings
			// written before SourceStation existed.
			station := p.SourceStation
			if station == "" {
				station = p.StationID
			}
			if err := e.history.RecordObservation(ctx,
				p.FromRoute, p.RouteID, station, p.Hour,
				int(p.SourceDelaySecs), int(p.ObservedMax),
			); err != nil {
				e.logger.Warn("record observation", "err", err, "id", p.ID)
			}
		} else if e.stopHistory != nil {
			// Non-primary outcomes feed the per-stop store (primaries feed
			// the transfer-pair store; recording them in both would let the
			// engine double-count the same signal at the first stop).
			if err := e.stopHistory.RecordStopOutcome(ctx,
				p.RouteID, p.StationID, p.Hour, int(p.ObservedMax),
			); err != nil {
				e.logger.Warn("record stop outcome", "err", err, "id", p.ID)
			}
		}
	}
	if len(expired) > 0 {
		e.logger.Debug("evaluation sweep finalized", "count", len(expired))
	}
	return nil
}

// ConfidenceBucket maps a confidence score to its calibration bucket label.
func ConfidenceBucket(c float64) string {
	switch {
	case c < 0.4:
		return "0.2-0.4"
	case c < 0.6:
		return "0.4-0.6"
	case c < 0.8:
		return "0.6-0.8"
	default:
		return "0.8-1.0"
	}
}
