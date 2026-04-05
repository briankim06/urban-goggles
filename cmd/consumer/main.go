// Command consumer is a development tool that subscribes to the delay-events
// Kafka topic and prints each decoded DelayEvent to stdout. Used for manual
// verification of the ingestor pipeline.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/IBM/sarama"
	transit "github.com/briankim06/urban-goggles/proto/transit"
	"google.golang.org/protobuf/proto"
)

func main() {
	brokers := flag.String("brokers", "localhost:9092", "comma-separated Kafka brokers")
	topic := flag.String("topic", "delay-events", "topic to consume")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg := sarama.NewConfig()
	cfg.Consumer.Return.Errors = true
	cfg.Consumer.Offsets.Initial = sarama.OffsetNewest

	client, err := sarama.NewConsumer([]string{*brokers}, cfg)
	if err != nil {
		logger.Error("new consumer", "err", err)
		os.Exit(1)
	}
	defer client.Close()

	partitions, err := client.Partitions(*topic)
	if err != nil {
		logger.Error("list partitions", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	for _, p := range partitions {
		pc, err := client.ConsumePartition(*topic, p, sarama.OffsetNewest)
		if err != nil {
			logger.Error("consume partition", "partition", p, "err", err)
			continue
		}
		go func(pc sarama.PartitionConsumer, partition int32) {
			defer pc.Close()
			for {
				select {
				case <-ctx.Done():
					return
				case msg := <-pc.Messages():
					if msg == nil {
						return
					}
					var ev transit.DelayEvent
					if err := proto.Unmarshal(msg.Value, &ev); err != nil {
						logger.Warn("unmarshal", "err", err)
						continue
					}
					fmt.Printf("[p=%d off=%d key=%s] route=%s trip=%s stop=%s delay=%ds type=%s predicted=%d\n",
						partition, msg.Offset, string(msg.Key),
						ev.GetRouteId(), ev.GetTripId(), ev.GetStopId(),
						ev.GetDelaySeconds(), ev.GetType().String(), ev.GetPredictedArrival())
				}
			}
		}(pc, p)
	}

	<-ctx.Done()
}
