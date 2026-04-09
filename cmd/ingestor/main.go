// Command ingestor polls MTA GTFS-Realtime feeds, normalizes the protobuf
// payloads into DelayEvent records, and publishes them to Kafka.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/briankim06/urban-goggles/internal/ingest"
	_ "github.com/briankim06/urban-goggles/internal/metrics" // register metrics
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to ingestor config file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := ingest.LoadConfig(*configPath)
	if err != nil {
		logger.Error("load config", "err", err)
		os.Exit(1)
	}
	if len(cfg.Feeds) == 0 {
		logger.Error("no feeds configured")
		os.Exit(1)
	}

	pub, err := ingest.NewKafkaPublisher(cfg.KafkaBrokers, cfg.KafkaTopic, 10)
	if err != nil {
		logger.Error("kafka publisher", "err", err)
		os.Exit(1)
	}
	defer func() {
		if err := pub.Close(); err != nil {
			logger.Warn("kafka close", "err", err)
		}
	}()

	// Prometheus metrics endpoint.
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		logger.Info("metrics server starting", "addr", ":9090")
		if err := http.ListenAndServe(":9090", mux); err != nil {
			logger.Error("metrics server", "err", err)
		}
	}()

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var wg sync.WaitGroup
	pollers := make([]*ingest.Poller, 0, len(cfg.Feeds))
	for _, feed := range cfg.Feeds {
		p := ingest.NewPoller(feed.Name, feed.URL, cfg.AgencyID, feed.PollInterval, pub, logger)
		pollers = append(pollers, p)
		wg.Add(1)
		go func(poller *ingest.Poller) {
			defer wg.Done()
			poller.Start(ctx)
		}(p)
	}

	// Throughput reporter: every 30s, log total and per-feed event counts.
	wg.Add(1)
	go func() {
		defer wg.Done()
		reportThroughput(ctx, logger, pollers)
	}()

	<-ctx.Done()
	logger.Info("shutdown signal received")
	wg.Wait()
}

func reportThroughput(ctx context.Context, logger *slog.Logger, pollers []*ingest.Poller) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	var prevTotal uint64
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			var total uint64
			for _, p := range pollers {
				total += p.Published()
			}
			delta := total - prevTotal
			elapsed := now.Sub(start).Seconds()
			logger.Info("throughput",
				"total_events", total,
				"events_last_30s", delta,
				"events_per_sec", float64(total)/elapsed,
			)
			prevTotal = total
		}
	}
}
