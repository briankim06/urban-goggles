package server

import (
	"context"
	"log/slog"
	"sync"

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

// RunKafkaConsumer reads from the transfer-impacts topic and publishes each
// event to all subscribers. Blocks until ctx is cancelled.
func (b *AlertBroker) RunKafkaConsumer(ctx context.Context, brokers []string, topic string) error {
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
	for _, p := range partitions {
		pc, err := client.ConsumePartition(topic, p, sarama.OffsetNewest)
		if err != nil {
			b.logger.Warn("consume partition", "partition", p, "err", err)
			continue
		}
		wg.Add(1)
		go func(pc sarama.PartitionConsumer) {
			defer wg.Done()
			defer pc.Close()
			for {
				select {
				case <-ctx.Done():
					return
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

	<-ctx.Done()
	wg.Wait()
	return nil
}
