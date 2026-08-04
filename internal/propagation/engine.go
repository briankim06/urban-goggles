package propagation

import (
	"container/heap"
	"context"
	"log/slog"
	"math"
	"time"

	"github.com/briankim06/urban-goggles/internal/graph"
	"github.com/briankim06/urban-goggles/internal/state"
	pb "github.com/briankim06/urban-goggles/proto/transit"
)

const (
	// shrinkageK controls how quickly historical observations dominate a
	// schedule-based estimate: weight = count / (count + shrinkageK).
	shrinkageK = 10.0
	// maxConnectingDelayAddSecs caps the connecting-delay addition to the
	// wait estimate when no scheduled headway is known.
	maxConnectingDelayAddSecs = 900
)

// PropagationEngine predicts cascading downstream delays when a transfer
// breaks, using a priority-queue-driven walk of the temporal graph
// conditioned on live network state and locally observed outcomes.
type PropagationEngine struct {
	graph    *graph.TransitGraph
	state    *state.DelayStateManager
	history  *HistoricalStore
	stopHist *StopImpactStore
	agencyID string
	cfg      Config
	logger   *slog.Logger
}

// NewPropagationEngine creates an engine wired to the graph, state, and
// historical stores. stopHist may be nil (per-stop blending disabled). A
// zero-valued cfg gets defaults.
func NewPropagationEngine(
	g *graph.TransitGraph,
	mgr *state.DelayStateManager,
	hist *HistoricalStore,
	stopHist *StopImpactStore,
	agencyID string,
	cfg Config,
	logger *slog.Logger,
) *PropagationEngine {
	if logger == nil {
		logger = slog.Default()
	}
	return &PropagationEngine{
		graph:    g,
		state:    mgr,
		history:  hist,
		stopHist: stopHist,
		agencyID: agencyID,
		cfg:      cfg.WithDefaults(),
		logger:   logger,
	}
}

// walk carries the per-Propagate traversal state.
type walk struct {
	pq      *impactQueue
	visited map[string]bool  // routeID:parentStationID
	live    map[string]int32 // routeID:parentStationID → max active positive delay
	tod     int              // seconds since midnight at detection time
	hour    int
}

// Propagate walks downstream from a broken transfer and predicts cascading
// delay impacts. Returns a PropagationResult with all downstream impacts.
func (e *PropagationEngine) Propagate(ctx context.Context, impact *pb.TransferImpact) (*pb.PropagationResult, error) {
	toRoute := impact.GetToRouteId()
	stationID := impact.GetStationId()

	// Determine direction from the next viable trip.
	directionID := 0
	if tripID := impact.GetNextViableTripId(); tripID != "" {
		if d, ok := e.graph.TripDirection[tripID]; ok {
			directionID = d
		}
	}

	result := &pb.PropagationResult{
		SourceTripId:       impact.GetFromTripId(),
		SourceStopId:       stationID,
		SourceDelaySeconds: impact.GetSourceDelaySeconds(),
		ComputedAt:         time.Now().Unix(),
	}

	tod := e.graph.SecsSinceMidnight(impact.GetDetectedAt())
	w := &walk{
		pq:      &impactQueue{},
		visited: make(map[string]bool),
		live:    e.snapshotLiveDelays(ctx),
		tod:     tod,
		hour:    tod / 3600 % 24,
	}
	heap.Init(w.pq)

	// Check historical data for a better prediction. History is keyed by the
	// parent transfer station, matching what the evaluation sweeper records.
	histAvg, histCount, _ := e.history.GetAverageImpact(
		ctx, impact.GetFromRouteId(), toRoute, e.graph.ParentStationID(stationID), w.hour,
	)

	connDelay := w.live[toRoute+":"+e.graph.ParentStationID(stationID)]
	initialDelay := e.estimateInitialDelay(impact, directionID, connDelay, histAvg, histCount)
	e.seedDownstream(ctx, w, toRoute, stationID, directionID, initialDelay, 1.0, 1, "dwell_increase")

	// Process the queue (highest impact first).
	for w.pq.Len() > 0 {
		if len(result.Impacts) >= e.cfg.MaxImpacts {
			break
		}
		item := heap.Pop(w.pq).(*queueItem)
		parentID := e.graph.ParentStationID(item.stopID)
		key := item.routeID + ":" + parentID
		if w.visited[key] {
			continue
		}
		w.visited[key] = true

		result.Impacts = append(result.Impacts, &pb.DownstreamImpact{
			RouteId:                  item.routeID,
			StopId:                   parentID,
			PredictedAdditionalDelay: item.delay,
			Confidence:               float32(item.confidence),
			HopsFromSource:           int32(item.hops),
			ImpactType:               item.impactType,
		})

		if item.hops < e.cfg.MaxDepth && item.delay >= e.cfg.MinDelayThreshold {
			// Check if this accumulated delay breaks further transfers here.
			e.propagateTransfers(w, parentID, item.routeID, item.delay, item.confidence, item.hops)

			// A cascade onto a new route ripples down that route too.
			if item.impactType == "cascade_transfer" {
				if dir, ok := e.deriveDirection(item.routeID, parentID, w.tod); ok {
					e.seedDownstream(ctx, w, item.routeID, parentID, dir,
						item.delay, item.confidence, item.hops, "cascade_dwell")
				}
			}
		}
	}

	e.logger.Debug("propagation complete",
		"source", impact.GetFromRouteId()+"→"+toRoute,
		"station", stationID,
		"downstream_impacts", len(result.GetImpacts()),
	)
	return result, nil
}

// snapshotLiveDelays captures the current active positive delays as
// routeID:parentStationID → max delay, from a single scan. Degrades to an
// empty map (schedule-only predictions) when state is unavailable.
func (e *PropagationEngine) snapshotLiveDelays(ctx context.Context) map[string]int32 {
	out := make(map[string]int32)
	if e.state == nil {
		return out
	}
	delays, err := e.state.GetAllActiveDelays(ctx, e.agencyID)
	if err != nil {
		e.logger.Warn("live delay snapshot", "err", err)
		return out
	}
	for _, ds := range delays {
		if ds.DelaySeconds <= 0 {
			continue // only lateness changes whether a connection is catchable
		}
		key := ds.RouteID + ":" + e.graph.ParentStationID(ds.StopID)
		if ds.DelaySeconds > out[key] {
			out[key] = ds.DelaySeconds
		}
	}
	return out
}

// seedDownstream enqueues the stops after stationID on routeID/direction,
// accumulating dwell delay and decaying confidence per stop. Emitted delays
// for stops past the first are blended with locally observed outcomes when
// enough samples exist; the model accumulator itself stays pure so one
// stop's history cannot distort later stops.
func (e *PropagationEngine) seedDownstream(
	ctx context.Context, w *walk,
	routeID, stationID string, directionID int,
	startDelay int32, baseConf float64, hops int, impactType string,
) {
	downstream := e.graph.GetDownstreamStops(routeID, stationID, directionID)
	if len(downstream) == 0 {
		return
	}

	heap.Push(w.pq, &queueItem{
		routeID:     routeID,
		stopID:      downstream[0].StopID,
		delay:       startDelay,
		confidence:  baseConf,
		hops:        hops,
		impactType:  impactType,
		directionID: directionID,
	})

	accumulated := startDelay
	for i := 1; i < len(downstream); i++ {
		extraDwell := float64(e.cfg.BaseDwellSecs) * e.cfg.LoadIncreasePct * e.cfg.DwellElasticity
		accumulated += int32(math.Round(extraDwell))
		conf := baseConf * math.Pow(e.cfg.ConfidenceDecay, float64(i+1))
		if conf < e.cfg.MinConfidence || accumulated < e.cfg.MinDelayThreshold {
			break
		}
		heap.Push(w.pq, &queueItem{
			routeID:     routeID,
			stopID:      downstream[i].StopID,
			delay:       e.blendPerStop(ctx, w, routeID, downstream[i].StopID, accumulated),
			confidence:  conf,
			hops:        hops,
			impactType:  impactType,
			directionID: directionID,
		})
	}
}

// blendPerStop shrinkage-blends the model's accumulated delay with observed
// outcomes at this stop when enough samples exist.
func (e *PropagationEngine) blendPerStop(ctx context.Context, w *walk, routeID, stopID string, modelDelay int32) int32 {
	if e.stopHist == nil {
		return modelDelay
	}
	stationID := e.graph.ParentStationID(stopID)
	avg, n, err := e.stopHist.GetAverageStopImpact(ctx, routeID, stationID, w.hour)
	if err != nil || n < e.cfg.PerStopMinCount {
		return modelDelay
	}
	wgt := float64(n) / (float64(n) + shrinkageK)
	return int32(math.Round(wgt*avg + (1-wgt)*float64(modelDelay)))
}

// deriveDirection picks the direction of the next scheduled departure on
// routeID at stationID after tod — the next train to leave is the one that
// inherits a cascade.
func (e *PropagationEngine) deriveDirection(routeID, stationID string, tod int) (int, bool) {
	deps, _ := e.graph.NextDeparturesAfter(stationID, routeID, tod, 1)
	if len(deps) == 0 {
		return 0, false
	}
	d, ok := e.graph.TripDirection[deps[0].TripID]
	return d, ok
}

// propagateTransfers checks if the accumulated delay at a stop breaks any
// outbound transfers and, if so, queues the connecting routes. A connecting
// route that is already running late widens the effective margin — the late
// train is easier to catch.
func (e *PropagationEngine) propagateTransfers(
	w *walk,
	stationID, sourceRoute string,
	delay int32,
	parentConf float64,
	parentHops int,
) {
	transfers := e.graph.GetTransfersFrom(stationID)
	for _, xfer := range transfers {
		destParent := e.graph.ParentStationID(xfer.ToStopID)
		destRoutes := e.graph.GetRoutesAtStop(xfer.ToStopID)
		for toRoute := range destRoutes {
			if toRoute == sourceRoute {
				continue
			}
			if w.visited[toRoute+":"+destParent] {
				continue
			}

			minXfer := xfer.MinTransferTime
			if minXfer <= 0 {
				minXfer = 120
			}
			connDelay := w.live[toRoute+":"+destParent]
			if int(delay) < minXfer+int(connDelay) {
				continue
			}

			newConf := parentConf * e.cfg.ConfidenceDecay
			if newConf < e.cfg.MinConfidence {
				continue
			}
			newDelay := int32(float64(delay) * e.cfg.DwellElasticity)
			if newDelay < e.cfg.MinDelayThreshold {
				continue
			}

			heap.Push(w.pq, &queueItem{
				routeID:    toRoute,
				stopID:     xfer.ToStopID,
				delay:      newDelay,
				confidence: newConf,
				hops:       parentHops + 1,
				impactType: "cascade_transfer",
			})
		}
	}
}

// estimateInitialDelay computes the predicted additional delay on the first
// downstream stop. For transfers, the primary signal is schedule-based — the
// wait until the next viable trip on the receiving route, guarded by half
// the scheduled headway — plus the receiving route's current delay (capped
// at one headway: beyond that the passenger catches the following train).
// For self-propagation (from == to route) the train simply carries its own
// delay. History, when plentiful, is shrinkage-blended in.
func (e *PropagationEngine) estimateInitialDelay(
	impact *pb.TransferImpact,
	directionID int,
	connectingDelay int32,
	histAvg float64,
	histCount int,
) int32 {
	var scheduleEst float64
	if impact.GetFromRouteId() != "" && impact.GetFromRouteId() == impact.GetToRouteId() {
		scheduleEst = float64(impact.GetSourceDelaySeconds())
	} else {
		scheduleEst = float64(impact.GetAdditionalWaitSeconds())
		tod := e.graph.SecsSinceMidnight(impact.GetDetectedAt())
		hw, hwOK := e.graph.GetScheduledHeadway(impact.GetToRouteId(), directionID, impact.GetStationId(), tod)
		if hwOK {
			if half := hw / 2; half > scheduleEst {
				scheduleEst = half
			}
		}
		if scheduleEst <= 0 {
			scheduleEst = float64(impact.GetSourceDelaySeconds()) * e.cfg.LoadIncreasePct * e.cfg.DwellElasticity
		}
		if connectingDelay > 0 {
			add := float64(connectingDelay)
			if hwOK {
				if add > hw {
					add = hw
				}
			} else if add > maxConnectingDelayAddSecs {
				add = maxConnectingDelayAddSecs
			}
			scheduleEst += add
		}
	}
	wgt := float64(histCount) / (float64(histCount) + shrinkageK)
	return int32(math.Round(wgt*histAvg + (1-wgt)*scheduleEst))
}

// --- priority queue ---

type queueItem struct {
	routeID     string
	stopID      string
	delay       int32
	confidence  float64
	hops        int
	impactType  string
	directionID int
	index       int // heap index
}

type impactQueue []*queueItem

func (q impactQueue) Len() int            { return len(q) }
func (q impactQueue) Less(i, j int) bool   { return q[i].delay > q[j].delay } // max-heap by delay
func (q impactQueue) Swap(i, j int)        { q[i], q[j] = q[j], q[i]; q[i].index = i; q[j].index = j }
func (q *impactQueue) Push(x interface{})  { item := x.(*queueItem); item.index = len(*q); *q = append(*q, item) }
func (q *impactQueue) Pop() interface{} {
	old := *q
	n := len(old)
	item := old[n-1]
	old[n-1] = nil
	item.index = -1
	*q = old[:n-1]
	return item
}
