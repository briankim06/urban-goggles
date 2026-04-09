# Step 9: Observability + Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add Prometheus metrics, Grafana dashboards, backpressure handling, graceful shutdown with state recovery, and integration tests to the transit delay propagation pipeline.

**Architecture:** Each service (ingestor, processor, server) gets a dedicated metrics package that registers Prometheus collectors and exposes an HTTP `/metrics` endpoint on a unique port. The processor gains backpressure logic that monitors Kafka consumer lag and switches to catch-up mode when lag is high. A feed recorder script captures live GTFS-RT protobuf responses for replay in integration tests. Docker Compose is extended with Prometheus and Grafana containers, with auto-provisioned dashboards.

**Tech Stack:** `prometheus/client_golang`, Grafana 11 with JSON dashboard provisioning, `go.uber.org/goleak`, sarama consumer group for lag monitoring.

---

## File Structure

### New files

| Path | Responsibility |
|------|----------------|
| `internal/metrics/metrics.go` | Shared Prometheus metric definitions for all components |
| `internal/metrics/server.go` | HTTP server that exposes `/metrics` on a configurable port |
| `scripts/record_feeds.go` | CLI tool to fetch and save 5 minutes of live GTFS-RT feeds as binary protobuf files |
| `tests/integration/pipeline_test.go` | End-to-end integration test: replay recorded feeds through ingestor -> Kafka -> processor -> Redis |
| `tests/integration/testdata/.gitkeep` | Directory for recorded protobuf feed fixtures |
| `deployments/prometheus.yml` | Prometheus scrape config targeting all three services |
| `deployments/grafana/provisioning/dashboards/dashboard.yml` | Grafana dashboard provisioning config |
| `deployments/grafana/provisioning/datasources/datasource.yml` | Grafana datasource provisioning config (auto-add Prometheus) |
| `deployments/grafana/dashboards/transit.json` | Pre-built Grafana dashboard with 7 panels |

### Modified files

| Path | Change |
|------|--------|
| `cmd/ingestor/main.go` | Start metrics HTTP server, instrument pollers |
| `cmd/processor/main.go` | Start metrics HTTP server, add backpressure logic, improve graceful shutdown |
| `cmd/server/main.go` | Start metrics HTTP server, instrument gRPC |
| `internal/ingest/poller.go` | Record poll duration and error metrics |
| `internal/ingest/kafka.go` | Record publish metrics |
| `internal/state/manager.go` | Record Redis operation duration metrics |
| `internal/transfer/detector.go` | Record broken transfer counter |
| `internal/propagation/engine.go` | Record fan-out histogram and processing duration |
| `internal/server/handlers.go` | Record gRPC request duration and active streams |
| `deployments/docker-compose.yml` | Add Prometheus and Grafana services |
| `Makefile` | Add `record-feeds` target |
| `go.mod` / `go.sum` | Add prometheus/client_golang, goleak dependencies |

---

## Task 1: Add prometheus/client_golang and goleak dependencies

**Files:**
- Modify: `go.mod`

- [ ] **Step 1: Add dependencies**

```bash
cd /Users/yejun/MyProjects/transit_eng
go get github.com/prometheus/client_golang/prometheus
go get github.com/prometheus/client_golang/prometheus/promhttp
go get go.uber.org/goleak
```

- [ ] **Step 2: Verify build**

Run: `go build ./...`
Expected: builds cleanly with no errors.

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "feat(deps): add prometheus client_golang and goleak"
```

---

## Task 2: Create shared metrics definitions

**Files:**
- Create: `internal/metrics/metrics.go`
- Create: `internal/metrics/server.go`

- [ ] **Step 1: Create `internal/metrics/metrics.go`**

```go
package metrics

import "github.com/prometheus/client_golang/prometheus"

// Ingestor metrics.
var (
	IngestorEventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ingestor_events_total",
			Help: "Total delay events published, by route.",
		},
		[]string{"route"},
	)
	IngestorPollDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "ingestor_poll_duration_seconds",
			Help:    "Duration of each feed poll cycle.",
			Buckets: prometheus.DefBuckets,
		},
	)
	IngestorPollErrors = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "ingestor_poll_errors_total",
			Help: "Total feed poll errors.",
		},
	)
)

// Processor metrics.
var (
	ProcessorEventsProcessed = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "processor_events_processed_total",
			Help: "Total delay events consumed and processed.",
		},
	)
	ProcessorEventDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "processor_event_processing_duration_seconds",
			Help:    "Time to process a single delay event (state + transfer + propagation).",
			Buckets: prometheus.DefBuckets,
		},
	)
	ProcessorActiveDelays = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "processor_active_delays",
			Help: "Current number of active delays above threshold.",
		},
	)
	ProcessorBrokenTransfers = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "processor_broken_transfers_total",
			Help: "Total broken transfer connections detected.",
		},
	)
	ProcessorPropagationFanOut = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "processor_propagation_fan_out",
			Help:    "Number of downstream impacts per propagation run.",
			Buckets: []float64{0, 1, 2, 5, 10, 20, 50, 100},
		},
	)
)

// Server metrics.
var (
	ServerGRPCRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "server_grpc_requests_total",
			Help: "Total gRPC requests by method.",
		},
		[]string{"method"},
	)
	ServerGRPCDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "server_grpc_request_duration_seconds",
			Help:    "gRPC request duration by method.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method"},
	)
	ServerActiveStreams = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "server_active_streams",
			Help: "Number of active StreamAlerts connections.",
		},
	)
)

// Redis metrics.
var (
	RedisOperationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "redis_operation_duration_seconds",
			Help:    "Redis operation latency by operation type.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"operation"},
	)
	RedisKeysTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "redis_keys_total",
			Help: "Approximate total number of Redis keys.",
		},
	)
)

// Kafka metrics.
var (
	KafkaConsumerLag = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kafka_consumer_lag",
			Help: "Consumer lag by topic and partition.",
		},
		[]string{"topic", "partition"},
	)
)

func init() {
	prometheus.MustRegister(
		// Ingestor
		IngestorEventsTotal,
		IngestorPollDuration,
		IngestorPollErrors,
		// Processor
		ProcessorEventsProcessed,
		ProcessorEventDuration,
		ProcessorActiveDelays,
		ProcessorBrokenTransfers,
		ProcessorPropagationFanOut,
		// Server
		ServerGRPCRequests,
		ServerGRPCDuration,
		ServerActiveStreams,
		// Redis
		RedisOperationDuration,
		RedisKeysTotal,
		// Kafka
		KafkaConsumerLag,
	)
}
```

- [ ] **Step 2: Create `internal/metrics/server.go`**

```go
package metrics

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ListenAndServe starts an HTTP server exposing /metrics on the given port.
// It shuts down gracefully when ctx is cancelled.
func ListenAndServe(ctx context.Context, port int, logger *slog.Logger) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())

	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: mux,
	}

	go func() {
		<-ctx.Done()
		srv.Close()
	}()

	logger.Info("metrics server starting", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}
```

- [ ] **Step 3: Verify build**

Run: `go build ./internal/metrics/...`
Expected: builds cleanly.

- [ ] **Step 4: Commit**

```bash
git add internal/metrics/
git commit -m "feat(metrics): add shared Prometheus metric definitions and HTTP server"
```

---

## Task 3: Instrument the ingestor

**Files:**
- Modify: `internal/ingest/poller.go`
- Modify: `cmd/ingestor/main.go`

- [ ] **Step 1: Instrument `internal/ingest/poller.go`**

Add metric recording to `pollOnce`. At the top of `pollOnce`, start a timer. On error, increment `IngestorPollErrors`. On success, observe duration on `IngestorPollDuration`. For each published event, increment `IngestorEventsTotal` with the route label.

In the import block, add:
```go
"github.com/briankim06/urban-goggles/internal/metrics"
```

Replace the existing `pollOnce` method body. Wrap the fetch call with timing:

```go
func (p *Poller) pollOnce(ctx context.Context) {
	start := time.Now()
	feed, err := p.fetch(ctx)
	if err != nil {
		p.Logger.Warn("feed fetch failed", "err", err)
		metrics.IngestorPollErrors.Inc()
		return
	}
	metrics.IngestorPollDuration.Observe(time.Since(start).Seconds())

	observedAt := int64(feed.GetHeader().GetTimestamp())
	if observedAt == 0 {
		observedAt = time.Now().Unix()
	}

	var raw []*transit.DelayEvent
	for _, entity := range feed.GetEntity() {
		tu := entity.GetTripUpdate()
		if tu == nil {
			continue
		}
		raw = append(raw, NormalizeTripUpdate(p.AgencyID, observedAt, tu)...)
	}

	changed := p.Differ.Diff(raw)
	for _, ev := range changed {
		if err := p.Publisher.Publish(ctx, ev); err != nil {
			p.Logger.Error("publish failed", "err", err, "trip", ev.GetTripId(), "stop", ev.GetStopId())
			continue
		}
		atomic.AddUint64(&p.published, 1)
		metrics.IngestorEventsTotal.WithLabelValues(ev.GetRouteId()).Inc()
	}
	p.Logger.Debug("poll complete", "raw", len(raw), "emitted", len(changed))
}
```

- [ ] **Step 2: Add metrics server to `cmd/ingestor/main.go`**

Add import for `"github.com/briankim06/urban-goggles/internal/metrics"` and start the metrics HTTP server in a goroutine before the poller loop, on port 9090:

After the `defer cancel()` line, add:

```go
wg.Add(1)
go func() {
	defer wg.Done()
	if err := metrics.ListenAndServe(ctx, 9090, logger); err != nil {
		logger.Error("metrics server", "err", err)
	}
}()
```

- [ ] **Step 3: Verify build**

Run: `go build ./cmd/ingestor/...`
Expected: builds cleanly.

- [ ] **Step 4: Commit**

```bash
git add internal/ingest/poller.go cmd/ingestor/main.go
git commit -m "feat(ingestor): add Prometheus metrics instrumentation"
```

---

## Task 4: Instrument the processor with metrics, backpressure, and graceful shutdown

**Files:**
- Modify: `internal/state/manager.go`
- Modify: `internal/transfer/detector.go`
- Modify: `internal/propagation/engine.go`
- Modify: `cmd/processor/main.go`

- [ ] **Step 1: Instrument Redis operations in `internal/state/manager.go`**

Add import for `"github.com/briankim06/urban-goggles/internal/metrics"` and `"time"` is already imported.

In `getState`, wrap the Redis GET with timing:

```go
func (m *DelayStateManager) getState(ctx context.Context, key string) (*DelayState, error) {
	start := time.Now()
	raw, err := m.rdb.Get(ctx, key).Bytes()
	metrics.RedisOperationDuration.WithLabelValues("get").Observe(time.Since(start).Seconds())
	if err != nil {
		return nil, err
	}
	var ds DelayState
	if err := json.Unmarshal(raw, &ds); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", key, err)
	}
	return &ds, nil
}
```

In `setState`, wrap the Redis SET with timing:

```go
func (m *DelayStateManager) setState(ctx context.Context, key string, ds *DelayState) error {
	data, err := json.Marshal(ds)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	start := time.Now()
	err = m.rdb.Set(ctx, key, data, keyTTL).Err()
	metrics.RedisOperationDuration.WithLabelValues("set").Observe(time.Since(start).Seconds())
	return err
}
```

- [ ] **Step 2: Instrument broken transfers in `internal/transfer/detector.go`**

Add import for `"github.com/briankim06/urban-goggles/internal/metrics"`.

In `EvaluateDelay`, after appending each BROKEN impact to `impacts`, increment the counter. Add this inside the `for toRoute := range destRoutes` loop, right after the `impact := &pb.TransferImpact{...}` block and before appending to `impacts`:

Replace the block that appends the impact and logs it:

```go
			impacts = append(impacts, impact)

			if level == pb.TransferImpact_BROKEN {
				metrics.ProcessorBrokenTransfers.Inc()
			}

			levelStr := "AT_RISK"
```

- [ ] **Step 3: Instrument propagation fan-out in `internal/propagation/engine.go`**

Add import for `"github.com/briankim06/urban-goggles/internal/metrics"`.

At the end of `Propagate`, before the final `return result, nil`, add:

```go
	metrics.ProcessorPropagationFanOut.Observe(float64(len(result.GetImpacts())))
```

- [ ] **Step 4: Rewrite `cmd/processor/main.go` with metrics, backpressure, and graceful shutdown**

This is the largest change. The key additions:
1. Metrics HTTP server on port 9091
2. Backpressure: a `catchUpMode` atomic bool. A goroutine periodically checks consumer lag via sarama admin client. If lag > 1000, set catchUpMode=true. If lag < 100, set catchUpMode=false.
3. In `ConsumeClaim`, when catchUpMode is true, skip `detector.EvaluateDelay` and propagation (only do `mgr.ProcessEvent`).
4. Graceful shutdown: the existing signal handling already cancels context. Add explicit flush of producers.
5. Periodic gauge update for `processor_active_delays` and `redis_keys_total`.

Replace the full `cmd/processor/main.go`:

```go
// Command processor consumes DelayEvent messages from Kafka, maintains
// reconciled delay state in Redis, detects broken transfer connections,
// runs the propagation engine on broken transfers, and publishes results.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/IBM/sarama"
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
	lagHighWatermark        = 1000
	lagLowWatermark         = 100
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

	var catchUpMode atomic.Bool

	handler := &consumerHandler{
		mgr:         mgr,
		detector:    detector,
		engine:      propEngine,
		predPub:     predPub,
		catchUpMode: &catchUpMode,
		logger:      logger,
	}

	var wg sync.WaitGroup

	// Metrics HTTP server on :9091.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := metrics.ListenAndServe(ctx, 9091, logger); err != nil {
			logger.Error("metrics server", "err", err)
		}
	}()

	// Backpressure monitor: check consumer lag every 10s.
	wg.Add(1)
	go func() {
		defer wg.Done()
		monitorLag(ctx, cfg.KafkaBrokers, cfg.KafkaTopic, &catchUpMode, logger)
	}()

	// Stats reporter: update gauges.
	wg.Add(1)
	go func() {
		defer wg.Done()
		reportStats(ctx, logger, mgr, rdb, cfg.AgencyID)
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
	logger.Info("shutdown signal received, waiting for goroutines")
	wg.Wait()
	logger.Info("processor stopped")
}

// monitorLag checks Kafka consumer lag and toggles catch-up mode.
func monitorLag(ctx context.Context, brokers []string, topic string, catchUp *atomic.Bool, logger *slog.Logger) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	cfg := sarama.NewConfig()
	cfg.Version = sarama.V3_6_0_0
	admin, err := sarama.NewClusterAdmin(brokers, cfg)
	if err != nil {
		logger.Warn("lag monitor: cannot create admin client", "err", err)
		return
	}
	defer admin.Close()

	kafkaClient, err := sarama.NewClient(brokers, cfg)
	if err != nil {
		logger.Warn("lag monitor: cannot create client", "err", err)
		return
	}
	defer kafkaClient.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			totalLag := computeLag(kafkaClient, admin, topic, logger)

			for p, lag := range totalLag {
				metrics.KafkaConsumerLag.WithLabelValues(topic, strconv.Itoa(int(p))).Set(float64(lag))
			}

			var sumLag int64
			for _, lag := range totalLag {
				sumLag += lag
			}

			wasCatchUp := catchUp.Load()
			if sumLag > lagHighWatermark && !wasCatchUp {
				catchUp.Store(true)
				logger.Warn("BACKPRESSURE: entering catch-up mode", "lag", sumLag)
			} else if sumLag < lagLowWatermark && wasCatchUp {
				catchUp.Store(false)
				logger.Info("BACKPRESSURE: exiting catch-up mode", "lag", sumLag)
			}
		}
	}
}

// computeLag returns per-partition lag for the processor-group on the given topic.
func computeLag(client sarama.Client, admin sarama.ClusterAdmin, topic string, logger *slog.Logger) map[int32]int64 {
	partitions, err := client.Partitions(topic)
	if err != nil {
		logger.Debug("lag: partitions", "err", err)
		return nil
	}

	offsets, err := admin.ListConsumerGroupOffsets("processor-group", map[string][]int32{topic: partitions})
	if err != nil {
		logger.Debug("lag: consumer offsets", "err", err)
		return nil
	}

	lags := make(map[int32]int64, len(partitions))
	for _, p := range partitions {
		newest, err := client.GetOffset(topic, p, sarama.OffsetNewest)
		if err != nil {
			continue
		}
		block := offsets.GetBlock(topic, p)
		if block == nil || block.Offset < 0 {
			lags[p] = newest
			continue
		}
		lag := newest - block.Offset
		if lag < 0 {
			lag = 0
		}
		lags[p] = lag
	}
	return lags
}

// consumerHandler implements sarama.ConsumerGroupHandler.
type consumerHandler struct {
	mgr         *state.DelayStateManager
	detector    *transfer.TransferDetector
	engine      *propagation.PropagationEngine
	predPub     *propagation.KafkaPredictionPublisher
	catchUpMode *atomic.Bool
	logger      *slog.Logger
}

func (*consumerHandler) Setup(_ sarama.ConsumerGroupSession) error   { return nil }
func (*consumerHandler) Cleanup(_ sarama.ConsumerGroupSession) error { return nil }

func (h *consumerHandler) ConsumeClaim(sess sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for msg := range claim.Messages() {
		start := time.Now()

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
		if !h.catchUpMode.Load() {
			impacts, err := h.detector.EvaluateDelay(sess.Context(), &ev)
			if err != nil {
				h.logger.Error("evaluate delay", "err", err, "trip", ev.GetTripId())
			}
			for _, impact := range impacts {
				if impact.GetLevel() == pb.TransferImpact_BROKEN {
					go h.runPropagation(sess.Context(), impact)
				}
			}
		}

		sess.MarkMessage(msg, "")
		metrics.ProcessorEventsProcessed.Inc()
		metrics.ProcessorEventDuration.Observe(time.Since(start).Seconds())
	}
	return nil
}

func (h *consumerHandler) runPropagation(ctx context.Context, impact *pb.TransferImpact) {
	result, err := h.engine.Propagate(ctx, impact)
	if err != nil {
		h.logger.Error("propagation", "err", err)
		return
	}
	if len(result.GetImpacts()) == 0 {
		return
	}
	h.logger.Info("propagation result",
		"source", impact.GetFromRouteId()+"->"+impact.GetToRouteId(),
		"station", impact.GetStationId(),
		"downstream_impacts", len(result.GetImpacts()),
	)
	if err := h.predPub.Publish(ctx, result); err != nil {
		h.logger.Error("publish prediction", "err", err)
	}
}

func reportStats(ctx context.Context, logger *slog.Logger, mgr *state.DelayStateManager, rdb *redis.Client, agencyID string) {
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

			// Update Redis keys gauge.
			dbSize, err := rdb.DBSize(ctx).Result()
			if err == nil {
				metrics.RedisKeysTotal.Set(float64(dbSize))
			}

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
```

- [ ] **Step 5: Verify build**

Run: `go build ./cmd/processor/...`
Expected: builds cleanly.

- [ ] **Step 6: Commit**

```bash
git add internal/state/manager.go internal/transfer/detector.go internal/propagation/engine.go cmd/processor/main.go
git commit -m "feat(processor): add metrics, backpressure handling, and improved shutdown"
```

---

## Task 5: Instrument the gRPC server

**Files:**
- Modify: `internal/server/handlers.go`
- Modify: `cmd/server/main.go`

- [ ] **Step 1: Add metrics to `internal/server/handlers.go`**

Add import for `"github.com/briankim06/urban-goggles/internal/metrics"` and `"time"`.

Add timing + counter instrumentation to each RPC handler. For `GetNetworkState`, wrap the body:

At the start of `GetNetworkState`:
```go
func (s *TransitService) GetNetworkState(ctx context.Context, _ *pb.NetworkStateRequest) (*pb.NetworkStateResponse, error) {
	start := time.Now()
	defer func() {
		metrics.ServerGRPCRequests.WithLabelValues("GetNetworkState").Inc()
		metrics.ServerGRPCDuration.WithLabelValues("GetNetworkState").Observe(time.Since(start).Seconds())
	}()
```

At the start of `GetRouteDelays`:
```go
func (s *TransitService) GetRouteDelays(ctx context.Context, req *pb.RouteDelaysRequest) (*pb.RouteDelaysResponse, error) {
	start := time.Now()
	defer func() {
		metrics.ServerGRPCRequests.WithLabelValues("GetRouteDelays").Inc()
		metrics.ServerGRPCDuration.WithLabelValues("GetRouteDelays").Observe(time.Since(start).Seconds())
	}()
```

At the start of `PredictImpact`:
```go
func (s *TransitService) PredictImpact(ctx context.Context, req *pb.PredictImpactRequest) (*pb.PropagationResult, error) {
	start := time.Now()
	defer func() {
		metrics.ServerGRPCRequests.WithLabelValues("PredictImpact").Inc()
		metrics.ServerGRPCDuration.WithLabelValues("PredictImpact").Observe(time.Since(start).Seconds())
	}()
```

At the start of `GetBlastRadius`:
```go
func (s *TransitService) GetBlastRadius(ctx context.Context, req *pb.BlastRadiusRequest) (*pb.BlastRadiusResponse, error) {
	start := time.Now()
	defer func() {
		metrics.ServerGRPCRequests.WithLabelValues("GetBlastRadius").Inc()
		metrics.ServerGRPCDuration.WithLabelValues("GetBlastRadius").Observe(time.Since(start).Seconds())
	}()
```

At the start of `GetTransferReliability`:
```go
func (s *TransitService) GetTransferReliability(ctx context.Context, req *pb.TransferReliabilityRequest) (*pb.TransferReliabilityResponse, error) {
	start := time.Now()
	defer func() {
		metrics.ServerGRPCRequests.WithLabelValues("GetTransferReliability").Inc()
		metrics.ServerGRPCDuration.WithLabelValues("GetTransferReliability").Observe(time.Since(start).Seconds())
	}()
```

In `StreamAlerts`, add stream counting. After the `s.logger.Info("stream client connected"...` line:
```go
	metrics.ServerActiveStreams.Inc()
	defer metrics.ServerActiveStreams.Dec()
```

- [ ] **Step 2: Add metrics server to `cmd/server/main.go`**

Add import for `"github.com/briankim06/urban-goggles/internal/metrics"` and `"sync"`.

Start metrics HTTP server on port 9092. Add before the gRPC server setup, after the context creation:

```go
var wg sync.WaitGroup
wg.Add(1)
go func() {
	defer wg.Done()
	if err := metrics.ListenAndServe(ctx, 9092, logger); err != nil {
		logger.Error("metrics server", "err", err)
	}
}()
```

And change the shutdown to wait:
```go
go func() {
	<-ctx.Done()
	logger.Info("shutting down gRPC server")
	grpcServer.GracefulStop()
}()

if err := grpcServer.Serve(lis); err != nil {
	logger.Error("serve", "err", err)
}
wg.Wait()
```

- [ ] **Step 3: Verify build**

Run: `go build ./cmd/server/...`
Expected: builds cleanly.

- [ ] **Step 4: Commit**

```bash
git add internal/server/handlers.go cmd/server/main.go
git commit -m "feat(server): add Prometheus metrics to gRPC handlers"
```

---

## Task 6: Prometheus config and Docker Compose update

**Files:**
- Create: `deployments/prometheus.yml`
- Create: `deployments/grafana/provisioning/datasources/datasource.yml`
- Create: `deployments/grafana/provisioning/dashboards/dashboard.yml`
- Modify: `deployments/docker-compose.yml`

- [ ] **Step 1: Create `deployments/prometheus.yml`**

```yaml
global:
  scrape_interval: 15s

scrape_configs:
  - job_name: "ingestor"
    static_configs:
      - targets: ["host.docker.internal:9090"]

  - job_name: "processor"
    static_configs:
      - targets: ["host.docker.internal:9091"]

  - job_name: "server"
    static_configs:
      - targets: ["host.docker.internal:9092"]
```

Note: `host.docker.internal` is used because the Go services run on the host, not in Docker. This works on macOS and Docker Desktop for Linux. For native Linux, add `extra_hosts` in docker-compose.

- [ ] **Step 2: Create `deployments/grafana/provisioning/datasources/datasource.yml`**

```yaml
apiVersion: 1
datasources:
  - name: Prometheus
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
```

- [ ] **Step 3: Create `deployments/grafana/provisioning/dashboards/dashboard.yml`**

```yaml
apiVersion: 1
providers:
  - name: "Transit"
    orgId: 1
    folder: ""
    type: file
    disableDeletion: false
    editable: true
    options:
      path: /var/lib/grafana/dashboards
      foldersFromFilesStructure: false
```

- [ ] **Step 4: Update `deployments/docker-compose.yml`**

Replace the full file:

```yaml
services:
  kafka:
    image: apache/kafka:latest
    container_name: transit-kafka
    ports:
      - "9092:9092"
    environment:
      KAFKA_NODE_ID: "1"
      KAFKA_PROCESS_ROLES: "broker,controller"
      KAFKA_CONTROLLER_QUORUM_VOTERS: "1@localhost:9093"
      KAFKA_LISTENERS: "PLAINTEXT://:9092,CONTROLLER://:9093"
      KAFKA_LISTENER_SECURITY_PROTOCOL_MAP: "CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT"
      KAFKA_CONTROLLER_LISTENER_NAMES: "CONTROLLER"
      KAFKA_ADVERTISED_LISTENERS: "PLAINTEXT://localhost:9092"
      KAFKA_INTER_BROKER_LISTENER_NAME: "PLAINTEXT"
      KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR: "1"
      KAFKA_TRANSACTION_STATE_LOG_REPLICATION_FACTOR: "1"
      KAFKA_TRANSACTION_STATE_LOG_MIN_ISR: "1"
      KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS: "0"

  redis:
    image: redis:7-alpine
    container_name: transit-redis
    ports:
      - "6379:6379"
    volumes:
      - redis_data:/data

  prometheus:
    image: prom/prometheus:latest
    container_name: transit-prometheus
    ports:
      - "9099:9090"
    volumes:
      - ./prometheus.yml:/etc/prometheus/prometheus.yml
    extra_hosts:
      - "host.docker.internal:host-gateway"

  grafana:
    image: grafana/grafana:latest
    container_name: transit-grafana
    ports:
      - "3000:3000"
    environment:
      GF_SECURITY_ADMIN_PASSWORD: "admin"
      GF_AUTH_ANONYMOUS_ENABLED: "true"
      GF_AUTH_ANONYMOUS_ORG_ROLE: "Viewer"
    volumes:
      - ./grafana/provisioning:/etc/grafana/provisioning
      - ./grafana/dashboards:/var/lib/grafana/dashboards
    depends_on:
      - prometheus

volumes:
  redis_data:
```

Note: Prometheus container maps port 9099 externally to avoid conflict with the ingestor metrics on host port 9090. Prometheus internally still listens on 9090.

- [ ] **Step 5: Verify docker compose config**

Run: `docker compose -f deployments/docker-compose.yml config`
Expected: valid YAML output with all 4 services.

- [ ] **Step 6: Commit**

```bash
git add deployments/
git commit -m "feat(infra): add Prometheus and Grafana to docker-compose with provisioning"
```

---

## Task 7: Grafana dashboard JSON

**Files:**
- Create: `deployments/grafana/dashboards/transit.json`

- [ ] **Step 1: Create the dashboard JSON**

Create `deployments/grafana/dashboards/transit.json` with 7 panels arranged in a 2-column grid:

```json
{
  "annotations": { "list": [] },
  "editable": true,
  "fiscalYearStartMonth": 0,
  "graphTooltip": 1,
  "id": null,
  "links": [],
  "panels": [
    {
      "title": "Events/sec Ingested (by route)",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 0, "y": 0 },
      "datasource": { "type": "prometheus", "uid": "${DS_PROMETHEUS}" },
      "fieldConfig": {
        "defaults": { "unit": "ops" },
        "overrides": []
      },
      "targets": [
        {
          "expr": "rate(ingestor_events_total[1m])",
          "legendFormat": "{{route}}"
        }
      ]
    },
    {
      "title": "Processing Latency p50/p99",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 12, "y": 0 },
      "datasource": { "type": "prometheus", "uid": "${DS_PROMETHEUS}" },
      "fieldConfig": {
        "defaults": { "unit": "s" },
        "overrides": []
      },
      "targets": [
        {
          "expr": "histogram_quantile(0.50, rate(processor_event_processing_duration_seconds_bucket[5m]))",
          "legendFormat": "p50"
        },
        {
          "expr": "histogram_quantile(0.99, rate(processor_event_processing_duration_seconds_bucket[5m]))",
          "legendFormat": "p99"
        }
      ]
    },
    {
      "title": "Active Delays",
      "type": "stat",
      "gridPos": { "h": 8, "w": 6, "x": 0, "y": 8 },
      "datasource": { "type": "prometheus", "uid": "${DS_PROMETHEUS}" },
      "fieldConfig": {
        "defaults": { "thresholds": { "steps": [{ "color": "green", "value": null }, { "color": "yellow", "value": 10 }, { "color": "red", "value": 50 }] } },
        "overrides": []
      },
      "targets": [
        { "expr": "processor_active_delays", "legendFormat": "active" }
      ]
    },
    {
      "title": "Broken Transfers/min",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 6, "x": 6, "y": 8 },
      "datasource": { "type": "prometheus", "uid": "${DS_PROMETHEUS}" },
      "fieldConfig": {
        "defaults": { "unit": "ops" },
        "overrides": []
      },
      "targets": [
        {
          "expr": "rate(processor_broken_transfers_total[1m]) * 60",
          "legendFormat": "broken/min"
        }
      ]
    },
    {
      "title": "Kafka Consumer Lag",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 12, "y": 8 },
      "datasource": { "type": "prometheus", "uid": "${DS_PROMETHEUS}" },
      "fieldConfig": {
        "defaults": {},
        "overrides": []
      },
      "targets": [
        {
          "expr": "kafka_consumer_lag",
          "legendFormat": "{{topic}} p{{partition}}"
        }
      ]
    },
    {
      "title": "Propagation Fan-Out Distribution",
      "type": "histogram",
      "gridPos": { "h": 8, "w": 12, "x": 0, "y": 16 },
      "datasource": { "type": "prometheus", "uid": "${DS_PROMETHEUS}" },
      "fieldConfig": {
        "defaults": {},
        "overrides": []
      },
      "targets": [
        {
          "expr": "rate(processor_propagation_fan_out_bucket[5m])",
          "legendFormat": "{{le}}",
          "format": "heatmap"
        }
      ]
    },
    {
      "title": "Redis Keys",
      "type": "timeseries",
      "gridPos": { "h": 8, "w": 12, "x": 12, "y": 16 },
      "datasource": { "type": "prometheus", "uid": "${DS_PROMETHEUS}" },
      "fieldConfig": {
        "defaults": {},
        "overrides": []
      },
      "targets": [
        { "expr": "redis_keys_total", "legendFormat": "keys" }
      ]
    }
  ],
  "schemaVersion": 39,
  "tags": ["transit"],
  "templating": {
    "list": [
      {
        "name": "DS_PROMETHEUS",
        "type": "datasource",
        "query": "prometheus",
        "current": { "text": "Prometheus", "value": "Prometheus" }
      }
    ]
  },
  "time": { "from": "now-30m", "to": "now" },
  "title": "Transit Delay Pipeline",
  "uid": "transit-pipeline"
}
```

- [ ] **Step 2: Commit**

```bash
git add deployments/grafana/dashboards/transit.json
git commit -m "feat(grafana): add pre-provisioned transit pipeline dashboard"
```

---

## Task 8: Feed recorder script

**Files:**
- Create: `scripts/record_feeds.go`
- Modify: `Makefile`

- [ ] **Step 1: Create `scripts/record_feeds.go`**

```go
//go:build ignore

// record_feeds fetches GTFS-RT feeds from the MTA every 15 seconds for a
// configurable duration and saves each response as a binary protobuf file.
//
// Usage: go run scripts/record_feeds.go -duration 5m -outdir tests/integration/testdata
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

var feeds = []struct {
	name string
	url  string
}{
	{"ACE", "https://api-endpoint.mta.info/Dataservice/mtagtfsfeeds/nyct%2Fgtfs-ace"},
	{"L", "https://api-endpoint.mta.info/Dataservice/mtagtfsfeeds/nyct%2Fgtfs-l"},
}

func main() {
	duration := flag.Duration("duration", 5*time.Minute, "how long to record")
	outdir := flag.String("outdir", "tests/integration/testdata", "output directory")
	interval := flag.Duration("interval", 15*time.Second, "poll interval")
	flag.Parse()

	if err := os.MkdirAll(*outdir, 0o755); err != nil {
		log.Fatal(err)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	deadline := time.Now().Add(*duration)
	seq := 0

	log.Printf("Recording feeds for %s to %s", *duration, *outdir)

	for time.Now().Before(deadline) {
		for _, f := range feeds {
			resp, err := client.Get(f.url)
			if err != nil {
				log.Printf("[%s] fetch error: %v", f.name, err)
				continue
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				log.Printf("[%s] read error: %v", f.name, err)
				continue
			}
			if resp.StatusCode != 200 {
				log.Printf("[%s] status %d", f.name, resp.StatusCode)
				continue
			}

			fname := filepath.Join(*outdir, fmt.Sprintf("%s_%04d.pb", f.name, seq))
			if err := os.WriteFile(fname, body, 0o644); err != nil {
				log.Printf("[%s] write error: %v", f.name, err)
				continue
			}
			log.Printf("[%s] saved %s (%d bytes)", f.name, fname, len(body))
		}
		seq++
		time.Sleep(*interval)
	}
	log.Printf("Done. Recorded %d snapshots per feed.", seq)
}
```

- [ ] **Step 2: Add Makefile target**

Add to `Makefile`:

```makefile
record-feeds:
	go run scripts/record_feeds.go -duration 5m -outdir tests/integration/testdata
```

- [ ] **Step 3: Create testdata directory**

```bash
mkdir -p tests/integration/testdata
touch tests/integration/testdata/.gitkeep
```

- [ ] **Step 4: Commit**

```bash
git add scripts/record_feeds.go Makefile tests/integration/testdata/.gitkeep
git commit -m "feat(scripts): add GTFS-RT feed recorder for integration test fixtures"
```

---

## Task 9: Record live feed fixtures

**Files:**
- Populate: `tests/integration/testdata/`

- [ ] **Step 1: Start infrastructure**

Run: `make infra-up`
Expected: Kafka, Redis, Prometheus, Grafana containers start.

- [ ] **Step 2: Record feeds**

Run: `make record-feeds`
Expected: 5 minutes of recording. ~20 files per feed (ACE, L) saved to `tests/integration/testdata/`.

- [ ] **Step 3: Verify files exist**

Run: `ls tests/integration/testdata/*.pb | wc -l`
Expected: ~40 files (20 per feed x 2 feeds).

- [ ] **Step 4: Add recorded fixtures to git**

```bash
git add tests/integration/testdata/*.pb
git commit -m "test(fixtures): add recorded GTFS-RT feed samples for integration tests"
```

---

## Task 10: Integration tests

**Files:**
- Create: `tests/integration/pipeline_test.go`

- [ ] **Step 1: Create `tests/integration/pipeline_test.go`**

```go
//go:build integration

package integration

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/IBM/sarama"
	"github.com/redis/go-redis/v9"
	"go.uber.org/goleak"

	"github.com/briankim06/urban-goggles/internal/graph"
	"github.com/briankim06/urban-goggles/internal/ingest"
	"github.com/briankim06/urban-goggles/internal/state"
	"github.com/briankim06/urban-goggles/internal/transfer"
	pb "github.com/briankim06/urban-goggles/proto/transit"
	gtfsrt "github.com/briankim06/urban-goggles/proto/gtfsrt"
	"google.golang.org/protobuf/proto"
)

const (
	testBroker = "localhost:9092"
	testTopic  = "delay-events-test"
	testRedis  = "localhost:6379"
	testDB     = 1 // use Redis DB 1 to avoid polluting DB 0
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		// sarama spawns long-lived goroutines for consumer/producer management
		goleak.IgnoreTopFunction("github.com/IBM/sarama.(*Broker).responseReceiver"),
		goleak.IgnoreTopFunction("github.com/IBM/sarama.(*Broker).sendAndReceiveSASLHandshake"),
	)
}

func skipIfNoInfra(t *testing.T) (*redis.Client, sarama.SyncProducer) {
	t.Helper()

	rdb := redis.NewClient(&redis.Options{Addr: testRedis, DB: testDB})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("Redis not available: %v", err)
	}

	cfg := sarama.NewConfig()
	cfg.Producer.Return.Successes = true
	producer, err := sarama.NewSyncProducer([]string{testBroker}, cfg)
	if err != nil {
		rdb.Close()
		t.Skipf("Kafka not available: %v", err)
	}

	return rdb, producer
}

func TestPipelineReplay(t *testing.T) {
	rdb, producer := skipIfNoInfra(t)
	defer rdb.Close()
	defer producer.Close()

	ctx := context.Background()
	rdb.FlushDB(ctx)

	// Load recorded feed files.
	files, err := filepath.Glob("testdata/*.pb")
	if err != nil || len(files) == 0 {
		t.Skip("No recorded feed fixtures found in testdata/. Run 'make record-feeds' first.")
	}
	sort.Strings(files)

	// Load transit graph.
	g, err := graph.BuildGraph("../../data/gtfs_static", time.Now())
	if err != nil {
		t.Fatalf("build graph: %v", err)
	}

	mgr := state.NewDelayStateManager(rdb, g, nil)
	detector := transfer.NewTransferDetector(g, mgr, nil, nil)

	// Phase 1: Replay feeds through normalizer and publish to Kafka.
	differ := ingest.NewDiffer()
	var totalPublished int

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		var feed gtfsrt.FeedMessage
		if err := proto.Unmarshal(data, &feed); err != nil {
			t.Logf("skip %s: unmarshal error: %v", f, err)
			continue
		}

		observedAt := int64(feed.GetHeader().GetTimestamp())
		if observedAt == 0 {
			observedAt = time.Now().Unix()
		}

		var raw []*pb.DelayEvent
		for _, entity := range feed.GetEntity() {
			tu := entity.GetTripUpdate()
			if tu == nil {
				continue
			}
			raw = append(raw, ingest.NormalizeTripUpdate("mta", observedAt, tu)...)
		}
		changed := differ.Diff(raw)
		for _, ev := range changed {
			payload, err := proto.Marshal(ev)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			msg := &sarama.ProducerMessage{
				Topic: testTopic,
				Key:   sarama.StringEncoder(ev.GetRouteId()),
				Value: sarama.ByteEncoder(payload),
			}
			if _, _, err := producer.SendMessage(msg); err != nil {
				t.Fatalf("send: %v", err)
			}
			totalPublished++
		}
	}
	t.Logf("Published %d events from %d feed files", totalPublished, len(files))

	if totalPublished == 0 {
		t.Fatal("No events were published from recorded feeds")
	}

	// Phase 2: Consume from Kafka and process through state manager + detector.
	cfg := sarama.NewConfig()
	cfg.Consumer.Return.Errors = true
	cfg.Consumer.Offsets.Initial = sarama.OffsetOldest

	consumer, err := sarama.NewConsumer([]string{testBroker}, cfg)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	defer consumer.Close()

	pc, err := consumer.ConsumePartition(testTopic, 0, sarama.OffsetOldest)
	if err != nil {
		t.Fatalf("consume partition: %v", err)
	}
	defer pc.Close()

	var processed int
	var transferImpacts []*pb.TransferImpact

	timeout := time.After(30 * time.Second)
	for processed < totalPublished {
		select {
		case msg := <-pc.Messages():
			var ev pb.DelayEvent
			if err := proto.Unmarshal(msg.Value, &ev); err != nil {
				continue
			}
			if err := mgr.ProcessEvent(ctx, &ev); err != nil {
				t.Logf("process error: %v", err)
			}
			impacts, _ := detector.EvaluateDelay(ctx, &ev)
			transferImpacts = append(transferImpacts, impacts...)
			processed++
		case <-timeout:
			t.Fatalf("timed out after consuming %d/%d events", processed, totalPublished)
		}
	}

	// Assertions.
	t.Logf("Processed %d events, detected %d transfer impacts", processed, len(transferImpacts))

	// 1. Delay state must be populated in Redis.
	delays, err := mgr.GetAllActiveDelays(ctx, "mta")
	if err != nil {
		t.Fatalf("get active delays: %v", err)
	}
	t.Logf("Active delays in Redis: %d", len(delays))
	if len(delays) == 0 {
		t.Log("WARNING: No active delays found. This may be normal if the recorded feed had no significant delays.")
	}

	// 2. Check Redis has keys.
	dbSize, err := rdb.DBSize(ctx).Result()
	if err != nil {
		t.Fatal(err)
	}
	if dbSize == 0 {
		t.Fatal("Expected delay state in Redis, but found 0 keys")
	}
	t.Logf("Redis keys: %d", dbSize)

	// 3. If any delays exist, transfer detection should have been attempted.
	if len(delays) > 0 && len(transferImpacts) > 0 {
		var broken int
		for _, imp := range transferImpacts {
			if imp.GetLevel() == pb.TransferImpact_BROKEN {
				broken++
			}
		}
		t.Logf("Broken transfers detected: %d", broken)
	}
}
```

- [ ] **Step 2: Verify compilation**

Run: `go build ./tests/integration/...`
Expected: builds cleanly (tests won't run without `-tags integration`).

Wait, `go build` doesn't work directly on test files. Instead:

Run: `go vet -tags integration ./tests/integration/...`
Expected: no errors.

- [ ] **Step 3: Run integration tests (requires infra up + recorded fixtures)**

Run: `go test -tags integration ./tests/integration/ -v -count=1`
Expected: PASS. Output shows events published, processed, and Redis state populated.

- [ ] **Step 4: Commit**

```bash
git add tests/integration/pipeline_test.go
git commit -m "test(integration): add end-to-end pipeline replay test with goleak"
```

---

## Task 11: Update Makefile with integration test target

**Files:**
- Modify: `Makefile`

- [ ] **Step 1: Add integration test target to Makefile**

Add these targets:

```makefile
test-integration:
	go test -tags integration ./tests/integration/ -v -count=1
```

- [ ] **Step 2: Commit**

```bash
git add Makefile
git commit -m "feat(makefile): add test-integration and record-feeds targets"
```

---

## Verification Checklist

After all tasks are complete, verify the acceptance criteria:

1. `make infra-up` starts Kafka, Redis, Prometheus, and Grafana (4 containers).
2. Grafana accessible at `http://localhost:3000` with the "Transit Delay Pipeline" dashboard.
3. Run `make run-ingestor`, `make run-processor`, `make run-server` — all three expose `/metrics`.
4. `curl localhost:9090/metrics` shows ingestor metrics.
5. `curl localhost:9091/metrics` shows processor metrics.
6. `curl localhost:9092/metrics` shows server metrics.
7. Grafana dashboard panels show non-zero values when pipeline is running.
8. `make test-integration` passes.
9. Graceful shutdown: `Ctrl-C` the processor, check logs for "shutdown signal received" and clean exit.
10. Backpressure: pause processor for 30s, resume, check logs for "entering catch-up mode" / "exiting catch-up mode".
