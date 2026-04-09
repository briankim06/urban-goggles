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
	baseDwellSecs     = 30   // assumed base dwell time per stop (seconds)
	dwellElasticity   = 0.3  // extra_dwell = base_dwell * load_pct * elasticity
	loadIncreasePct   = 0.15 // fraction of riders affected by a broken transfer
	confidenceDecay   = 0.7  // per-hop multiplier
	minConfidence     = 0.2  // stop propagating below this
	minDelayThreshold = 15   // stop propagating below this many seconds
	maxDepth          = 5    // recursive propagation depth limit
)

// PropagationEngine predicts cascading downstream delays when a transfer
// breaks, using a priority-queue-driven walk of the temporal graph.
type PropagationEngine struct {
	graph   *graph.TransitGraph
	state   *state.DelayStateManager
	history *HistoricalStore
	logger  *slog.Logger
}

// NewPropagationEngine creates an engine wired to the graph, state, and
// historical stores.
func NewPropagationEngine(
	g *graph.TransitGraph,
	mgr *state.DelayStateManager,
	hist *HistoricalStore,
	logger *slog.Logger,
) *PropagationEngine {
	if logger == nil {
		logger = slog.Default()
	}
	return &PropagationEngine{
		graph:   g,
		state:   mgr,
		history: hist,
		logger:  logger,
	}
}

// Propagate walks downstream from a broken transfer and predicts cascading
// delay impacts. Returns a PropagationResult with all downstream impacts.
func (e *PropagationEngine) Propagate(ctx context.Context, impact *pb.TransferImpact) (*pb.PropagationResult, error) {
	toRoute := impact.GetToRouteId()
	stationID := impact.GetStationId()
	sourceDelay := impact.GetSourceDelaySeconds()

	// Determine direction from the next viable trip.
	directionID := 0
	if tripID := impact.GetNextViableTripId(); tripID != "" {
		if d, ok := e.graph.TripDirection[tripID]; ok {
			directionID = d
		}
	}

	downstream := e.graph.GetDownstreamStops(toRoute, stationID, directionID)

	result := &pb.PropagationResult{
		SourceTripId:       impact.GetFromTripId(),
		SourceStopId:       stationID,
		SourceDelaySeconds: sourceDelay,
		ComputedAt:         time.Now().Unix(),
	}

	if len(downstream) == 0 {
		return result, nil
	}

	// Check historical data for a better prediction.
	hour := time.Now().Hour()
	histAvg, histCount, _ := e.history.GetAverageImpact(
		ctx, impact.GetFromRouteId(), toRoute, stationID, hour,
	)

	// Seed the priority queue with the first downstream stop.
	initialDelay := e.estimateInitialDelay(sourceDelay, histAvg, histCount)
	pq := &impactQueue{}
	heap.Init(pq)
	heap.Push(pq, &queueItem{
		routeID:     toRoute,
		stopID:      downstream[0].StopID,
		delay:       initialDelay,
		confidence:  1.0,
		hops:        1,
		impactType:  "dwell_increase",
		directionID: directionID,
	})

	// Add remaining downstream stops with accumulated dwell increase.
	accumulatedDelay := initialDelay
	for i := 1; i < len(downstream); i++ {
		extraDwell := float64(baseDwellSecs) * loadIncreasePct * dwellElasticity
		accumulatedDelay += int32(math.Round(extraDwell))
		conf := confidenceDecay * math.Pow(confidenceDecay, float64(i))
		if conf < minConfidence || accumulatedDelay < minDelayThreshold {
			break
		}
		heap.Push(pq, &queueItem{
			routeID:     toRoute,
			stopID:      downstream[i].StopID,
			delay:       accumulatedDelay,
			confidence:  conf,
			hops:        1,
			impactType:  "dwell_increase",
			directionID: directionID,
		})
	}

	// Process the queue (highest impact first).
	visited := make(map[string]bool)
	for pq.Len() > 0 {
		item := heap.Pop(pq).(*queueItem)
		key := item.routeID + ":" + item.stopID
		if visited[key] {
			continue
		}
		visited[key] = true

		parentID := e.graph.ParentStationID(item.stopID)
		stopName := item.stopID
		if s, ok := e.graph.Stops[parentID]; ok {
			stopName = s.Name
		}

		result.Impacts = append(result.Impacts, &pb.DownstreamImpact{
			RouteId:                  item.routeID,
			StopId:                   parentID,
			PredictedAdditionalDelay: item.delay,
			Confidence:               float32(item.confidence),
			HopsFromSource:           int32(item.hops),
			ImpactType:               item.impactType,
		})

		// Recursive propagation: check if this accumulated delay breaks
		// further transfers at this stop.
		if item.hops < maxDepth && item.delay >= minDelayThreshold {
			e.propagateTransfers(ctx, pq, parentID, item.routeID, item.delay, item.confidence, item.hops, visited)
		}

		_ = stopName // used for potential debug logging
	}

	// Record this propagation as a historical observation.
	totalImpact := int32(0)
	for _, imp := range result.GetImpacts() {
		if imp.GetPredictedAdditionalDelay() > totalImpact {
			totalImpact = imp.GetPredictedAdditionalDelay()
		}
	}
	_ = e.history.RecordObservation(ctx,
		impact.GetFromRouteId(), toRoute, stationID, hour,
		int(sourceDelay), int(totalImpact),
	)

	e.logger.Debug("propagation complete",
		"source", impact.GetFromRouteId()+"→"+toRoute,
		"station", stationID,
		"downstream_impacts", len(result.GetImpacts()),
	)
	return result, nil
}

// propagateTransfers checks if the accumulated delay at a stop breaks any
// outbound transfers and, if so, adds the downstream stops of those
// connecting routes to the priority queue.
func (e *PropagationEngine) propagateTransfers(
	ctx context.Context,
	pq *impactQueue,
	stationID, sourceRoute string,
	delay int32,
	parentConf float64,
	parentHops int,
	visited map[string]bool,
) {
	transfers := e.graph.GetTransfersFrom(stationID)
	for _, xfer := range transfers {
		destRoutes := e.graph.GetRoutesAtStop(xfer.ToStopID)
		for toRoute := range destRoutes {
			if toRoute == sourceRoute {
				continue
			}
			if visited[toRoute+":"+xfer.ToStopID] {
				continue
			}

			minXfer := xfer.MinTransferTime
			if minXfer <= 0 {
				minXfer = 120
			}
			// Simplified check: if the accumulated delay exceeds the
			// typical transfer margin, this connection is likely broken.
			if int(delay) < minXfer {
				continue
			}

			newConf := parentConf * confidenceDecay
			if newConf < minConfidence {
				continue
			}
			newDelay := int32(float64(delay) * dwellElasticity)
			if newDelay < minDelayThreshold {
				continue
			}

			heap.Push(pq, &queueItem{
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
// downstream stop. If historical data is available, blend it with the model.
func (e *PropagationEngine) estimateInitialDelay(sourceDelay int32, histAvg float64, histCount int) int32 {
	modelDelay := float64(sourceDelay) * loadIncreasePct * dwellElasticity
	if histCount >= 5 {
		// Weight historical average more heavily when we have enough data.
		blend := 0.6*histAvg + 0.4*modelDelay
		return int32(math.Round(blend))
	}
	return int32(math.Round(modelDelay))
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
