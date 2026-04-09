// Command processor consumes DelayEvent messages from Kafka, maintains
// reconciled delay state in Redis, detects broken transfer connections,
// runs the propagation engine on broken transfers, and publishes results.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/IBM/sarama"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"

	"github.com/briankim06/urban-goggles/internal/graph"
	"github.com/briankim06/urban-goggles/internal/ingest"
	"github.com/briankim06/urban-goggles/internal/metrics"
	"github.com/briankim06/urban-goggles/internal/propagation"
	"github.com/briankim06/urban-goggles/internal/state"
	"github.com/briankim06/urban-goggles/internal/transfer"
	pb "github.com/briankim06/urban-goggles/proto/transit"
	"google.golang.org/protobuf/proto"
)

const (
	transferImpactsTopic    = "transfer-impacts"
	networkPredictionsTopic = "network-predictions"

	// Backpressure thresholds.
	catchUpEnterLag = 1000 // enter catch-up mode when consumer lag exceeds this
	catchUpExitLag  = 100  // exit catch-up mode when lag drops below this
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to config file")
	dataDir := flag.String("data", "data/gtfs_static", "path to GTFS static data")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := ingest.LoadConfig(*configPath)
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}

	// Load transit graph for today.
	g, err := graph.BuildGraph(*dataDir, time.Now())
	if err != nil {
		logger.Error("build graph", "err", err)
		os.Exit(1)
	}

	// Redis client.
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	defer rdb.Close()
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		logger.Error("redis ping", "err", err)
		os.Exit(1)
	}
	logger.Info("redis connected")

	mgr := state.NewDelayStateManager(rdb, g, logger)

	// Transfer impact Kafka publisher.
	impactPub, err := transfer.NewKafkaImpactPublisher(cfg.KafkaBrokers, transferImpactsTopic, 10)
	if err != nil {
		logger.Error("impact publisher", "err", err)
		os.Exit(1)
	}
	defer impactPub.Close()
	logger.Info("transfer-impacts topic ready")

	// Propagation engine + publisher.
	histStore := propagation.NewHistoricalStore(rdb)
	propEngine := propagation.NewPropagationEngine(g, mgr, histStore, logger)

	predPub, err := propagation.NewKafkaPredictionPublisher(cfg.KafkaBrokers, networkPredictionsTopic, 10)
	if err != nil {
		logger.Error("prediction publisher", "err", err)
		os.Exit(1)
	}
	defer predPub.Close()
	logger.Info("network-predictions topic ready")

	detector := transfer.NewTransferDetector(g, mgr, impactPub, logger)

	// Prometheus metrics endpoint.
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		logger.Info("metrics server starting", "addr", ":9091")
		if err := http.ListenAndServe(":9091", mux); err != nil {
			logger.Error("metrics server", "err", err)
		}
	}()

	// Kafka consumer.
	sc := sarama.NewConfig()
	sc.Consumer.Return.Errors = true
	sc.Consumer.Offsets.Initial = sarama.OffsetNewest

	client, err := sarama.NewConsumerGroup(cfg.KafkaBrokers, "processor-group", sc)
	if err != nil {
		logger.Error("kafka consumer group", "err", err)
		os.Exit(1)
	}
	defer client.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var catchUp atomic.Bool
	handler := &consumerHandler{
		mgr:       mgr,
		detector:  detector,
		engine:    propEngine,
		predPub:   predPub,
		catchUp:   &catchUp,
		logger:    logger,
	}

	// Stats reporter and lag monitor.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		reportStats(ctx, logger, mgr, cfg.AgencyID)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		monitorLag(ctx, logger, client, cfg.KafkaTopic, &catchUp)
	}()

	// Consume loop — re-enters on rebalance.
	logger.Info("processor starting", "topic", cfg.KafkaTopic)
	for {
		if err := client.Consume(ctx, []string{cfg.KafkaTopic}, handler); err != nil {
			logger.Error("consume", "err", err)
		}
		if ctx.Err() != nil {
			break
		}
	}
	logger.Info("shutdown signal received")
	wg.Wait()
}

// consumerHandler implements sarama.ConsumerGroupHandler.
type consumerHandler struct {
	mgr      *state.DelayStateManager
	detector *transfer.TransferDetector
	engine   *propagation.PropagationEngine
	predPub  *propagation.KafkaPredictionPublisher
	catchUp  *atomic.Bool
	logger   *slog.Logger
}

func (*consumerHandler) Setup(_ sarama.ConsumerGroupSession) error   { return nil }
func (*consumerHandler) Cleanup(_ sarama.ConsumerGroupSession) error { return nil }

func (h *consumerHandler) ConsumeClaim(sess sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	partition := strconv.Itoa(int(claim.Partition()))
	topic := claim.Topic()
	for msg := range claim.Messages() {
		start := time.Now()

		// Track consumer lag from high-water mark.
		lag := claim.HighWaterMarkOffset() - msg.Offset
		if lag < 0 {
			lag = 0
		}
		metrics.KafkaConsumerLag.WithLabelValues(topic, partition).Set(float64(lag))
		totalConsumerLag.Store(lag)

		var ev pb.DelayEvent
		if err := proto.Unmarshal(msg.Value, &ev); err != nil {
			h.logger.Warn("unmarshal", "err", err, "offset", msg.Offset)
			sess.MarkMessage(msg, "")
			continue
		}
		if err := h.mgr.ProcessEvent(sess.Context(), &ev); err != nil {
			h.logger.Error("process event", "err", err, "trip", ev.GetTripId())
		}

		// In catch-up mode, skip expensive transfer detection and propagation.
		if !h.catchUp.Load() {
			impacts, err := h.detector.EvaluateDelay(sess.Context(), &ev)
			if err != nil {
				h.logger.Error("evaluate delay", "err", err, "trip", ev.GetTripId())
			}

			for _, impact := range impacts {
				if impact.GetLevel() == pb.TransferImpact_BROKEN {
					metrics.ProcessorBrokenTransfers.Inc()
					go h.runPropagation(sess.Context(), impact)
				}
			}
		}

		metrics.ProcessorEventsProcessed.Inc()
		metrics.ProcessorEventDuration.Observe(time.Since(start).Seconds())
		sess.MarkMessage(msg, "")
	}
	return nil
}

func (h *consumerHandler) runPropagation(ctx context.Context, impact *pb.TransferImpact) {
	result, err := h.engine.Propagate(ctx, impact)
	if err != nil {
		h.logger.Error("propagation", "err", err)
		return
	}
	metrics.ProcessorPropagationFanOut.Observe(float64(len(result.GetImpacts())))
	if len(result.GetImpacts()) == 0 {
		return
	}
	h.logger.Info("propagation result",
		"source", impact.GetFromRouteId()+"→"+impact.GetToRouteId(),
		"station", impact.GetStationId(),
		"downstream_impacts", len(result.GetImpacts()),
	)
	if err := h.predPub.Publish(ctx, result); err != nil {
		h.logger.Error("publish prediction", "err", err)
	}
}

// monitorLag periodically checks the accumulated lag from consumer claims
// and toggles catch-up mode.
func monitorLag(ctx context.Context, logger *slog.Logger, _ sarama.ConsumerGroup, _ string, catchUp *atomic.Bool) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			// Lag is tracked per-partition inside ConsumeClaim via
			// HighWaterMarkOffset. The total is accumulated in
			// totalConsumerLag (set by ConsumeClaim). We read it here
			// to control backpressure.
			lag := totalConsumerLag.Load()
			wasCatchUp := catchUp.Load()
			if lag > catchUpEnterLag && !wasCatchUp {
				catchUp.Store(true)
				logger.Warn("entering catch-up mode", "lag", lag)
			} else if lag < catchUpExitLag && wasCatchUp {
				catchUp.Store(false)
				logger.Info("exiting catch-up mode", "lag", lag)
			}
		}
	}
}

// totalConsumerLag is updated by ConsumeClaim with the sum of per-partition lag.
var totalConsumerLag atomic.Int64

func reportStats(ctx context.Context, logger *slog.Logger, mgr *state.DelayStateManager, agencyID string) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			delays, err := mgr.GetAllActiveDelays(ctx, agencyID)
			if err != nil {
				logger.Warn("stats fetch", "err", err)
				continue
			}
			metrics.ProcessorActiveDelays.Set(float64(len(delays)))
			if len(delays) == 0 {
				logger.Info("stats", "active_delays", 0)
				continue
			}
			var sum, max int32
			for _, d := range delays {
				s := d.DelaySeconds
				if s < 0 {
					s = -s
				}
				sum += s
				if s > max {
					max = s
				}
			}
			avg := float64(sum) / float64(len(delays))
			logger.Info("stats",
				"active_delays", len(delays),
				"avg_delay_s", fmt.Sprintf("%.0f", avg),
				"max_delay_s", max,
			)
		}
	}
}
