// Command delays is a development tool that connects to Redis and prints a
// live table of current delays grouped by route, refreshing every 5 seconds.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/briankim06/urban-goggles/internal/state"
)

func main() {
	agency := flag.String("agency", "mta", "agency ID to display")
	flag.Parse()

	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	defer rdb.Close()
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		fmt.Fprintf(os.Stderr, "redis: %v\n", err)
		os.Exit(1)
	}

	mgr := state.NewDelayStateManager(rdb, nil, nil)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	t := time.NewTicker(5 * time.Second)
	defer t.Stop()

	render(ctx, mgr, *agency)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			render(ctx, mgr, *agency)
		}
	}
}

func render(ctx context.Context, mgr *state.DelayStateManager, agency string) {
	delays, err := mgr.GetAllActiveDelays(ctx, agency)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return
	}

	// Group by route.
	byRoute := make(map[string][]*state.DelayState)
	for _, d := range delays {
		byRoute[d.RouteID] = append(byRoute[d.RouteID], d)
	}
	routes := make([]string, 0, len(byRoute))
	for r := range byRoute {
		routes = append(routes, r)
	}
	sort.Strings(routes)

	// Clear screen.
	fmt.Print("\033[2J\033[H")
	fmt.Printf("=== Delay Dashboard (agency=%s)  %s ===\n\n", agency, time.Now().Format("15:04:05"))
	fmt.Printf("%-8s %-12s %-10s %-8s %s\n", "Route", "Trip", "Stop", "Delay", "Conf")
	fmt.Println("------   ----------   --------   ------  ----")

	total := 0
	for _, r := range routes {
		ds := byRoute[r]
		sort.Slice(ds, func(i, j int) bool {
			return ds[i].DelaySeconds > ds[j].DelaySeconds
		})
		// Show top 5 per route to keep it readable.
		limit := 5
		if len(ds) < limit {
			limit = len(ds)
		}
		for _, d := range ds[:limit] {
			fmt.Printf("%-8s %-12s %-10s %+4ds    %.1f\n",
				d.RouteID, truncate(d.TripID, 12), truncate(d.StopID, 10),
				d.DelaySeconds, d.Confidence)
		}
		if len(ds) > limit {
			fmt.Printf("%-8s ... and %d more\n", "", len(ds)-limit)
		}
		total += len(ds)
	}
	fmt.Printf("\nTotal active delays (>60s): %d\n", total)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
