package server

import (
	"context"
	"log/slog"
	"time"

	"github.com/briankim06/urban-goggles/internal/graph"
	"github.com/briankim06/urban-goggles/internal/metrics"
	"github.com/briankim06/urban-goggles/internal/propagation"
	"github.com/briankim06/urban-goggles/internal/state"
	pb "github.com/briankim06/urban-goggles/proto/transit"
)

// TransitService implements the TransitPropagation gRPC service.
type TransitService struct {
	pb.UnimplementedTransitPropagationServer

	graph    *graph.TransitGraph
	stateMgr *state.DelayStateManager
	alerts   *AlertBroker
	engine   *propagation.PropagationEngine
	history  *propagation.HistoricalStore
	agencyID string
	logger   *slog.Logger
}

// NewTransitService constructs a service wired to the delay state manager,
// alert broker, propagation engine, and historical store.
func NewTransitService(
	g *graph.TransitGraph,
	mgr *state.DelayStateManager,
	alerts *AlertBroker,
	engine *propagation.PropagationEngine,
	history *propagation.HistoricalStore,
	agencyID string,
	logger *slog.Logger,
) *TransitService {
	if logger == nil {
		logger = slog.Default()
	}
	return &TransitService{
		graph:    g,
		stateMgr: mgr,
		alerts:   alerts,
		engine:   engine,
		history:  history,
		agencyID: agencyID,
		logger:   logger,
	}
}

// GetNetworkState returns all active delays grouped by route.
func (s *TransitService) GetNetworkState(ctx context.Context, _ *pb.NetworkStateRequest) (*pb.NetworkStateResponse, error) {
	start := time.Now()
	defer func() {
		metrics.ServerGRPCRequests.WithLabelValues("GetNetworkState").Inc()
		metrics.ServerGRPCDuration.WithLabelValues("GetNetworkState").Observe(time.Since(start).Seconds())
	}()
	delays, err := s.stateMgr.GetAllActiveDelays(ctx, s.agencyID)
	if err != nil {
		return nil, err
	}

	byRoute := make(map[string]*pb.RouteState)
	for _, d := range delays {
		rs, ok := byRoute[d.RouteID]
		if !ok {
			rs = &pb.RouteState{RouteId: d.RouteID}
			byRoute[d.RouteID] = rs
		}
		stopName := d.StopID
		if stop, ok := s.graph.Stops[d.StopID]; ok {
			stopName = stop.Name
		} else {
			parentID := s.graph.ParentStationID(d.StopID)
			if stop, ok := s.graph.Stops[parentID]; ok {
				stopName = stop.Name
			}
		}
		rs.Delays = append(rs.Delays, &pb.StopDelay{
			StopId:       d.StopID,
			StopName:     stopName,
			DelaySeconds: d.DelaySeconds,
			Confidence:   float32(d.Confidence),
		})
	}

	return &pb.NetworkStateResponse{
		Routes: byRoute,
		AsOf:   time.Now().Unix(),
	}, nil
}

// GetRouteDelays returns delays for a single route.
func (s *TransitService) GetRouteDelays(ctx context.Context, req *pb.RouteDelaysRequest) (*pb.RouteDelaysResponse, error) {
	start := time.Now()
	defer func() {
		metrics.ServerGRPCRequests.WithLabelValues("GetRouteDelays").Inc()
		metrics.ServerGRPCDuration.WithLabelValues("GetRouteDelays").Observe(time.Since(start).Seconds())
	}()
	delays, err := s.stateMgr.GetRouteDelays(ctx, s.agencyID, req.GetRouteId())
	if err != nil {
		return nil, err
	}

	resp := &pb.RouteDelaysResponse{}
	for _, d := range delays {
		stopName := d.StopID
		if stop, ok := s.graph.Stops[d.StopID]; ok {
			stopName = stop.Name
		}
		resp.Delays = append(resp.Delays, &pb.StopDelay{
			StopId:       d.StopID,
			StopName:     stopName,
			DelaySeconds: d.DelaySeconds,
			Confidence:   float32(d.Confidence),
		})
	}
	return resp, nil
}

// StreamAlerts implements server-side streaming of TransferImpact events.
// Clients may optionally filter by route_ids or station_ids.
func (s *TransitService) StreamAlerts(req *pb.StreamAlertsRequest, stream pb.TransitPropagation_StreamAlertsServer) error {
	ch, unsub := s.alerts.Subscribe(64)
	defer unsub()

	routeFilter := make(map[string]bool, len(req.GetRouteIds()))
	for _, r := range req.GetRouteIds() {
		routeFilter[r] = true
	}
	stationFilter := make(map[string]bool, len(req.GetStationIds()))
	for _, st := range req.GetStationIds() {
		stationFilter[st] = true
	}
	hasFilter := len(routeFilter) > 0 || len(stationFilter) > 0

	metrics.ServerGRPCRequests.WithLabelValues("StreamAlerts").Inc()
	metrics.ServerActiveStreams.Inc()
	defer metrics.ServerActiveStreams.Dec()

	s.logger.Info("stream client connected",
		"route_filter", req.GetRouteIds(),
		"station_filter", req.GetStationIds(),
	)
	defer s.logger.Info("stream client disconnected")

	for {
		select {
		case <-stream.Context().Done():
			return nil
		case impact, ok := <-ch:
			if !ok {
				return nil
			}
			if hasFilter && !matchesFilter(impact, routeFilter, stationFilter) {
				continue
			}
			if err := stream.Send(impact); err != nil {
				return nil // client gone
			}
		}
	}
}

// PredictImpact runs the propagation engine for a specific delayed trip at a
// stop. It looks up the current delay from Redis, builds a synthetic
// TransferImpact, and returns the cascading downstream predictions.
func (s *TransitService) PredictImpact(ctx context.Context, req *pb.PredictImpactRequest) (*pb.PropagationResult, error) {
	start := time.Now()
	defer func() {
		metrics.ServerGRPCRequests.WithLabelValues("PredictImpact").Inc()
		metrics.ServerGRPCDuration.WithLabelValues("PredictImpact").Observe(time.Since(start).Seconds())
	}()
	tripID := req.GetTripId()
	stopID := req.GetStopId()

	parentID := s.graph.ParentStationID(stopID)

	// Look up current delay for this trip+stop.
	delay, err := s.stateMgr.GetDelay(ctx, s.agencyID, tripID, stopID)
	if err != nil {
		return nil, err
	}
	if delay == nil {
		return &pb.PropagationResult{
			SourceTripId:       tripID,
			SourceStopId:       parentID,
			SourceDelaySeconds: 0,
			ComputedAt:         time.Now().Unix(),
		}, nil
	}

	// Determine the route for this trip.
	routeID := s.graph.TripRoute[tripID]

	// Build a synthetic TransferImpact to feed the engine.
	impact := &pb.TransferImpact{
		FromTripId:          tripID,
		FromRouteId:         routeID,
		StationId:           parentID,
		SourceDelaySeconds:  delay.DelaySeconds,
	}

	result, err := s.engine.Propagate(ctx, impact)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// GetBlastRadius finds all current delays at a station, runs propagation for
// each, and returns the combined cascading impacts.
func (s *TransitService) GetBlastRadius(ctx context.Context, req *pb.BlastRadiusRequest) (*pb.BlastRadiusResponse, error) {
	start := time.Now()
	defer func() {
		metrics.ServerGRPCRequests.WithLabelValues("GetBlastRadius").Inc()
		metrics.ServerGRPCDuration.WithLabelValues("GetBlastRadius").Observe(time.Since(start).Seconds())
	}()
	stationID := s.graph.ParentStationID(req.GetStationId())

	// Get all routes serving this station.
	routes := s.graph.GetRoutesAtStop(stationID)

	resp := &pb.BlastRadiusResponse{}
	for routeID := range routes {
		delays, err := s.stateMgr.GetRouteDelays(ctx, s.agencyID, routeID)
		if err != nil {
			s.logger.Warn("blast radius: get delays", "route", routeID, "err", err)
			continue
		}
		for _, d := range delays {
			delayParent := s.graph.ParentStationID(d.StopID)
			if delayParent != stationID {
				continue
			}
			impact := &pb.TransferImpact{
				FromTripId:         d.TripID,
				FromRouteId:        routeID,
				StationId:          stationID,
				SourceDelaySeconds: d.DelaySeconds,
			}
			result, err := s.engine.Propagate(ctx, impact)
			if err != nil {
				s.logger.Warn("blast radius: propagate", "trip", d.TripID, "err", err)
				continue
			}
			if len(result.GetImpacts()) > 0 {
				resp.Cascades = append(resp.Cascades, result)
			}
		}
	}
	return resp, nil
}

// GetTransferReliability returns historical reliability stats for transfers
// at a station during a specific hour of day.
func (s *TransitService) GetTransferReliability(ctx context.Context, req *pb.TransferReliabilityRequest) (*pb.TransferReliabilityResponse, error) {
	start := time.Now()
	defer func() {
		metrics.ServerGRPCRequests.WithLabelValues("GetTransferReliability").Inc()
		metrics.ServerGRPCDuration.WithLabelValues("GetTransferReliability").Observe(time.Since(start).Seconds())
	}()
	stationID := s.graph.ParentStationID(req.GetStationId())
	hour := int(req.GetHourOfDay())

	stats, err := s.history.GetTransferStats(ctx, stationID, hour)
	if err != nil {
		return nil, err
	}

	resp := &pb.TransferReliabilityResponse{}
	for _, st := range stats {
		resp.Transfers = append(resp.Transfers, &pb.TransferStats{
			FromRouteId:         st.FromRouteID,
			ToRouteId:           st.ToRouteID,
			StationId:           st.StationID,
			AvgDelayWhenBroken:  float32(st.AvgDelayWhenBroken),
			SampleCount:         int32(st.SampleCount),
		})
	}
	return resp, nil
}

func matchesFilter(impact *pb.TransferImpact, routes, stations map[string]bool) bool {
	if len(routes) > 0 {
		if routes[impact.GetFromRouteId()] || routes[impact.GetToRouteId()] {
			return true
		}
	}
	if len(stations) > 0 {
		if stations[impact.GetStationId()] {
			return true
		}
	}
	return false
}
