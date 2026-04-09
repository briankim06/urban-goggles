// Command server runs the gRPC API for the transit delay propagation engine.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"

	"github.com/briankim06/urban-goggles/internal/graph"
	_ "github.com/briankim06/urban-goggles/internal/metrics" // register metrics
	"github.com/briankim06/urban-goggles/internal/propagation"
	"github.com/briankim06/urban-goggles/internal/server"
	"github.com/briankim06/urban-goggles/internal/state"
	pb "github.com/briankim06/urban-goggles/proto/transit"
)

const transferImpactsTopic = "transfer-impacts"

func main() {
	dataDir := flag.String("data", "data/gtfs_static", "path to GTFS static data")
	port := flag.Int("port", 50053, "gRPC listen port")
	agencyID := flag.String("agency", "mta", "agency ID")
	kafkaBroker := flag.String("kafka", "localhost:9092", "Kafka broker address")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// Load transit graph.
	g, err := graph.BuildGraph(*dataDir, time.Now())
	if err != nil {
		logger.Error("build graph", "err", err)
		os.Exit(1)
	}

	// Redis.
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	defer rdb.Close()
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		logger.Error("redis ping", "err", err)
		os.Exit(1)
	}
	logger.Info("redis connected")

	mgr := state.NewDelayStateManager(rdb, g, logger)

	// Propagation engine + historical store.
	histStore := propagation.NewHistoricalStore(rdb)
	propEngine := propagation.NewPropagationEngine(g, mgr, histStore, logger)

	// Alert broker: consumes transfer-impacts from Kafka and fans out to
	// StreamAlerts clients.
	alerts := server.NewAlertBroker(logger)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go func() {
		if err := alerts.RunKafkaConsumer(ctx, []string{*kafkaBroker}, transferImpactsTopic); err != nil {
			logger.Error("alert consumer", "err", err)
		}
	}()

	// Prometheus metrics endpoint.
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		logger.Info("metrics server starting", "addr", ":9092")
		if err := http.ListenAndServe(":9092", mux); err != nil {
			logger.Error("metrics server", "err", err)
		}
	}()

	// gRPC server.
	svc := server.NewTransitService(g, mgr, alerts, propEngine, histStore, *agencyID, logger)
	grpcServer := grpc.NewServer()
	pb.RegisterTransitPropagationServer(grpcServer, svc)

	addr := fmt.Sprintf(":%d", *port)
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error("listen", "err", err)
		os.Exit(1)
	}
	logger.Info("gRPC server starting", "addr", addr)

	// Graceful shutdown.
	go func() {
		<-ctx.Done()
		logger.Info("shutting down gRPC server")
		grpcServer.GracefulStop()
	}()

	if err := grpcServer.Serve(lis); err != nil {
		logger.Error("serve", "err", err)
	}
}
