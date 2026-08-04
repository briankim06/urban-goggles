package server

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/IBM/sarama"
	pb "github.com/briankim06/urban-goggles/proto/transit"
	"google.golang.org/protobuf/proto"
)

// AlertBroker consumes TransferImpact events from Kafka and fans them out to
// any number of connected gRPC StreamAlerts clients.
type AlertBroker struct {
	mu      sync.RWMutex
	subs    map[uint64]chan *pb.TransferImpact
	nextID  uint64
	logger  *slog.Logger
}

// NewAlertBroker creates a broker ready to accept subscribers.
func NewAlertBroker(logger *slog.Logger) *AlertBroker {
	if logger == nil {
		logger = slog.Default()
	}
	return &AlertBroker{
		subs:   make(map[uint64]chan *pb.TransferImpact),
		logger: logger,
	}
}

// Subscribe returns a channel that receives all future alerts and a function
// to call when the subscriber is done.
func (b *AlertBroker) Subscribe(bufSize int) (<-chan *pb.TransferImpact, func()) {
	ch := make(chan *pb.TransferImpact, bufSize)
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = ch
	b.mu.Unlock()

	unsub := func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}
	return ch, unsub
}

// Publish fans an impact out to all current subscribers. Slow subscribers that
// have a full buffer are skipped (non-blocking send).
func (b *AlertBroker) Publish(impact *pb.TransferImpact) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, ch := range b.subs {
		select {
		case ch <- impact:
		default:
			// subscriber is slow — drop rather than block
		}
	}
}

const (
	alertRetryBase = 5 * time.Second
	alertRetryMax  = 60 * time.Second
)

// RunKafkaConsumer reads from the transfer-impacts topic and publishes each
// event to all subscribers, reconnecting with backoff on failure so a
// transient Kafka outage (or Kafka not being up yet at boot) does not kill
// the alert stream for the process lifetime. Blocks until ctx is cancelled.
func (b *AlertBroker) RunKafkaConsumer(ctx context.Context, brokers []string, topic string) error {
	backoff := alertRetryBase
	for {
		err := b.consumeOnce(ctx, brokers, topic)
		if ctx.Err() != nil {
			return nil
		}
		b.logger.Warn("alert consumer disconnected; retrying", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > alertRetryMax {
			backoff = alertRetryMax
		}
	}
}

// consumeOnce runs one consumer session over all current partitions.
// Partitions added while the session is healthy are only picked up after the
// next reconnect — accepted for a fixed-partition dev topology.
func (b *AlertBroker) consumeOnce(ctx context.Context, brokers []string, topic string) error {
	cfg := sarama.NewConfig()
	cfg.Consumer.Return.Errors = true
	cfg.Consumer.Offsets.Initial = sarama.OffsetNewest

	client, err := sarama.NewConsumer(brokers, cfg)
	if err != nil {
		return err
	}
	defer client.Close()

	partitions, err := client.Partitions(topic)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	started := 0
	for _, p := range partitions {
		pc, err := client.ConsumePartition(topic, p, sarama.OffsetNewest)
		if err != nil {
			b.logger.Warn("consume partition", "partition", p, "err", err)
			continue
		}
		started++
		wg.Add(1)
		go func(pc sarama.PartitionConsumer) {
			defer wg.Done()
			defer pc.Close()
			for {
				select {
				case <-ctx.Done():
					return
				case err := <-pc.Errors():
					// Must be drained: sarama's send into this channel
					// blocks once its buffer fills, wedging the consumer.
					if err != nil {
						b.logger.Warn("partition consumer error", "err", err)
					}
				case msg := <-pc.Messages():
					if msg == nil {
						return
					}
					var impact pb.TransferImpact
					if err := proto.Unmarshal(msg.Value, &impact); err != nil {
						b.logger.Warn("unmarshal impact", "err", err)
						continue
					}
					b.Publish(&impact)
				}
			}
		}(pc)
	}
	if started == 0 {
		return fmt.Errorf("no partitions of %s could be consumed", topic)
	}

	<-ctx.Done()
	wg.Wait()
	return nil
}
