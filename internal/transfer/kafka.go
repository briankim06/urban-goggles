package transfer

import (
	"context"
	"fmt"

	"github.com/IBM/sarama"
	pb "github.com/briankim06/urban-goggles/proto/transit"
	"google.golang.org/protobuf/proto"
)

// KafkaImpactPublisher publishes TransferImpact events to a Kafka topic.
type KafkaImpactPublisher struct {
	producer sarama.SyncProducer
	topic    string
}

// NewKafkaImpactPublisher creates the transfer-impacts topic (if needed) and
// returns a publisher backed by a sync Kafka producer.
func NewKafkaImpactPublisher(brokers []string, topic string, partitions int32) (*KafkaImpactPublisher, error) {
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
	return &KafkaImpactPublisher{producer: producer, topic: topic}, nil
}

// PublishImpact serializes and sends the impact event, keyed by station ID.
func (k *KafkaImpactPublisher) PublishImpact(_ context.Context, impact *pb.TransferImpact) error {
	payload, err := proto.Marshal(impact)
	if err != nil {
		return fmt.Errorf("marshal impact: %w", err)
	}
	msg := &sarama.ProducerMessage{
		Topic: k.topic,
		Key:   sarama.StringEncoder(impact.GetStationId()),
		Value: sarama.ByteEncoder(payload),
	}
	_, _, err = k.producer.SendMessage(msg)
	return err
}

// Close shuts down the producer.
func (k *KafkaImpactPublisher) Close() error {
	return k.producer.Close()
}

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
	if err != nil && err != sarama.ErrTopicAlreadyExists {
		return fmt.Errorf("create topic: %w", err)
	}
	return nil
}
