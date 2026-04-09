package propagation

import (
	"context"
	"fmt"

	"github.com/IBM/sarama"
	pb "github.com/briankim06/urban-goggles/proto/transit"
	"google.golang.org/protobuf/proto"
)

// KafkaPredictionPublisher publishes PropagationResult events to Kafka.
type KafkaPredictionPublisher struct {
	producer sarama.SyncProducer
	topic    string
}

// NewKafkaPredictionPublisher creates the topic and returns a publisher.
func NewKafkaPredictionPublisher(brokers []string, topic string, partitions int32) (*KafkaPredictionPublisher, error) {
	if err := ensureTopic(brokers, topic, partitions); err != nil {
		return nil, fmt.Errorf("ensure topic: %w", err)
	}
	cfg := sarama.NewConfig()
	cfg.Producer.Return.Successes = true
	cfg.Producer.RequiredAcks = sarama.WaitForLocal

	producer, err := sarama.NewSyncProducer(brokers, cfg)
	if err != nil {
		return nil, fmt.Errorf("new producer: %w", err)
	}
	return &KafkaPredictionPublisher{producer: producer, topic: topic}, nil
}

// Publish sends a PropagationResult to Kafka, keyed by source stop ID.
func (k *KafkaPredictionPublisher) Publish(_ context.Context, result *pb.PropagationResult) error {
	payload, err := proto.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	msg := &sarama.ProducerMessage{
		Topic: k.topic,
		Key:   sarama.StringEncoder(result.GetSourceStopId()),
		Value: sarama.ByteEncoder(payload),
	}
	_, _, err = k.producer.SendMessage(msg)
	return err
}

// Close shuts down the producer.
func (k *KafkaPredictionPublisher) Close() error {
	return k.producer.Close()
}

func ensureTopic(brokers []string, topic string, partitions int32) error {
	cfg := sarama.NewConfig()
	cfg.Version = sarama.V3_6_0_0
	admin, err := sarama.NewClusterAdmin(brokers, cfg)
	if err != nil {
		return err
	}
	defer admin.Close()

	topics, err := admin.ListTopics()
	if err != nil {
		return err
	}
	if _, exists := topics[topic]; exists {
		return nil
	}
	err = admin.CreateTopic(topic, &sarama.TopicDetail{
		NumPartitions:     partitions,
		ReplicationFactor: 1,
	}, false)
	if err != nil && err != sarama.ErrTopicAlreadyExists {
		return err
	}
	return nil
}
