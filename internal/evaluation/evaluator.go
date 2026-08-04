package evaluation

import (
	"context"
	"fmt"
	"log/slog"
	"math"
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
	hour := now.Hour()

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

	tracked := 0
	var firstErr error
	for i, imp := range res.GetImpacts() {
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

		p := &PendingPrediction{
			ID: fmt.Sprintf("%s:%s:%s:%d:%d",
				src.GetFromRouteId(), imp.GetRouteId(), imp.GetStopId(), now.UnixNano(), i),
			AgencyID:        e.agencyID,
			FromRoute:       src.GetFromRouteId(),
			RouteID:         imp.GetRouteId(),
			StationID:       imp.GetStopId(), // engine records parent station IDs
			Direction:       direction,
			SourceDelaySecs: src.GetSourceDelaySeconds(),
			PredictedSecs:   imp.GetPredictedAdditionalDelay(),
			BaselineSecs:    baselines[imp.GetRouteId()+":"+imp.GetStopId()],
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

	evDir, evDirKnown := e.graph.TripDirection[ev.GetTripId()]
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
			if err := e.pending.Update(ctx, p); err != nil {
				return err
			}
		}
	}
	return nil
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
			if err := e.history.RecordObservation(ctx,
				p.FromRoute, p.RouteID, p.StationID, p.Hour,
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
