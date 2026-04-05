package ingest

import (
	"context"
	"fmt"

	"github.com/IBM/sarama"
	transit "github.com/briankim06/urban-goggles/proto/transit"
	"google.golang.org/protobuf/proto"
)

// KafkaPublisher implements Publisher by sending protobuf-serialized
// DelayEvent records to a Kafka topic, keyed by route_id so all events for a
// given route land on the same partition (preserving per-route ordering).
type KafkaPublisher struct {
	producer sarama.SyncProducer
	topic    string
}

// NewKafkaPublisher connects a sync producer to the given brokers and, if the
// topic does not already exist, creates it with the configured partition and
// replication counts.
func NewKafkaPublisher(brokers []string, topic string, partitions int32) (*KafkaPublisher, error) {
	if err := ensureTopic(brokers, topic, partitions); err != nil {
		return nil, fmt.Errorf("ensure topic: %w", err)
	}

	cfg := sarama.NewConfig()
	cfg.Producer.Return.Successes = true
	cfg.Producer.RequiredAcks = sarama.WaitForLocal
	cfg.Producer.Partitioner = sarama.NewHashPartitioner

	producer, err := sarama.NewSyncProducer(brokers, cfg)
	if err != nil {
		return nil, fmt.Errorf("new producer: %w", err)
	}
	return &KafkaPublisher{producer: producer, topic: topic}, nil
}

// Publish serializes the event and sends it to Kafka. Messages are keyed by
// route_id so sarama's hash partitioner keeps a given route on one partition.
func (k *KafkaPublisher) Publish(ctx context.Context, ev *transit.DelayEvent) error {
	payload, err := proto.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	msg := &sarama.ProducerMessage{
		Topic: k.topic,
		Key:   sarama.StringEncoder(ev.GetRouteId()),
		Value: sarama.ByteEncoder(payload),
	}
	_, _, err = k.producer.SendMessage(msg)
	return err
}

// Close flushes and shuts down the underlying producer. Safe to call once.
func (k *KafkaPublisher) Close() error {
	return k.producer.Close()
}

// ensureTopic creates the delay-events topic with the requested partition
// count if it does not already exist. Replication factor is 1 since the local
// dev stack runs a single-broker KRaft cluster.
func ensureTopic(brokers []string, topic string, partitions int32) error {
	cfg := sarama.NewConfig()
	cfg.Version = sarama.V3_6_0_0
	admin, err := sarama.NewClusterAdmin(brokers, cfg)
	if err != nil {
		return fmt.Errorf("cluster admin: %w", err)
	}
	defer admin.Close()

	topics, err := admin.ListTopics()
	if err != nil {
		return fmt.Errorf("list topics: %w", err)
	}
	if _, exists := topics[topic]; exists {
		return nil
	}
	err = admin.CreateTopic(topic, &sarama.TopicDetail{
		NumPartitions:     partitions,
		ReplicationFactor: 1,
	}, false)
	// Treat "already exists" as success in case of a race with another ingestor.
	if err != nil && err != sarama.ErrTopicAlreadyExists {
		return fmt.Errorf("create topic: %w", err)
	}
	return nil
}
