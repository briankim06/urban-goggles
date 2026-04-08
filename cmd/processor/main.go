// Command processor consumes DelayEvent messages from Kafka, maintains
// reconciled delay state in Redis, and logs summary statistics.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/IBM/sarama"
	"github.com/redis/go-redis/v9"

	"github.com/briankim06/urban-goggles/internal/graph"
	"github.com/briankim06/urban-goggles/internal/ingest"
	"github.com/briankim06/urban-goggles/internal/state"
	transit "github.com/briankim06/urban-goggles/proto/transit"
	"google.golang.org/protobuf/proto"
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

	handler := &consumerHandler{
		mgr:    mgr,
		logger: logger,
	}

	// Stats reporter.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		reportStats(ctx, logger, mgr, cfg.AgencyID)
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
	mgr    *state.DelayStateManager
	logger *slog.Logger
}

func (*consumerHandler) Setup(_ sarama.ConsumerGroupSession) error   { return nil }
func (*consumerHandler) Cleanup(_ sarama.ConsumerGroupSession) error { return nil }

func (h *consumerHandler) ConsumeClaim(sess sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for msg := range claim.Messages() {
		var ev transit.DelayEvent
		if err := proto.Unmarshal(msg.Value, &ev); err != nil {
			h.logger.Warn("unmarshal", "err", err, "offset", msg.Offset)
			sess.MarkMessage(msg, "")
			continue
		}
		if err := h.mgr.ProcessEvent(sess.Context(), &ev); err != nil {
			h.logger.Error("process event", "err", err, "trip", ev.GetTripId())
		}
		sess.MarkMessage(msg, "")
	}
	return nil
}

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
