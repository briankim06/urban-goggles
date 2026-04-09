// Command client is a development tool that connects to the gRPC server,
// calls GetNetworkState, and streams alerts.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os/signal"
	"sort"
	"syscall"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/briankim06/urban-goggles/proto/transit"
)

func main() {
	addr := flag.String("addr", "localhost:50053", "gRPC server address")
	flag.Parse()

	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	client := pb.NewTransitPropagationClient(conn)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// 1. GetNetworkState
	fmt.Println("=== Network State ===")
	resp, err := client.GetNetworkState(ctx, &pb.NetworkStateRequest{})
	if err != nil {
		log.Fatalf("GetNetworkState: %v", err)
	}
	routes := make([]string, 0, len(resp.GetRoutes()))
	for r := range resp.GetRoutes() {
		routes = append(routes, r)
	}
	sort.Strings(routes)

	total := 0
	for _, r := range routes {
		rs := resp.GetRoutes()[r]
		fmt.Printf("\nRoute %s (%d delays):\n", r, len(rs.GetDelays()))
		for _, d := range rs.GetDelays() {
			fmt.Printf("  %-30s %+4ds  (conf=%.1f)\n",
				d.GetStopName(), d.GetDelaySeconds(), d.GetConfidence())
		}
		total += len(rs.GetDelays())
	}
	fmt.Printf("\nTotal active delays: %d (as of %d)\n", total, resp.GetAsOf())

	// 2. StreamAlerts
	fmt.Println("\n=== Streaming Alerts (Ctrl+C to stop) ===")
	stream, err := client.StreamAlerts(ctx, &pb.StreamAlertsRequest{})
	if err != nil {
		log.Fatalf("StreamAlerts: %v", err)
	}
	for {
		impact, err := stream.Recv()
		if err == io.EOF || ctx.Err() != nil {
			break
		}
		if err != nil {
			log.Printf("stream recv: %v", err)
			break
		}
		level := "AT_RISK"
		if impact.GetLevel() == pb.TransferImpact_BROKEN {
			level = "BROKEN"
		}
		fmt.Printf("[%s] %s→%s at %s | delay=%ds margin=%ds wait=%ds\n",
			level,
			impact.GetFromRouteId(), impact.GetToRouteId(),
			impact.GetStationId(),
			impact.GetSourceDelaySeconds(),
			impact.GetRemainingMarginSeconds(),
			impact.GetAdditionalWaitSeconds(),
		)
	}

	fmt.Println("\nDone.")
}
