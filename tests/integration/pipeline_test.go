package integration

import (
	"context"
	"testing"
	"time"

	"github.com/IBM/sarama"
	"github.com/redis/go-redis/v9"
	"go.uber.org/goleak"
	"google.golang.org/protobuf/proto"

	"github.com/briankim06/urban-goggles/internal/state"
	pb "github.com/briankim06/urban-goggles/proto/transit"
)

const (
	testBroker = "localhost:9092"
	testTopic  = "delay-events-integration"
	testGroup  = "integration-test-group"
)

func skipIfNoInfra(t *testing.T) (*redis.Client, sarama.SyncProducer, sarama.ConsumerGroup) {
	t.Helper()

	// Redis
	rdb := redis.NewClient(&redis.Options{Addr: "localhost:6379"})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Skipf("Redis not available: %v", err)
	}

	// Kafka — create topic
	adminCfg := sarama.NewConfig()
	adminCfg.Version = sarama.V3_6_0_0
	admin, err := sarama.NewClusterAdmin([]string{testBroker}, adminCfg)
	if err != nil {
		t.Skipf("Kafka not available: %v", err)
	}
	topics, _ := admin.ListTopics()
	if _, ok := topics[testTopic]; !ok {
		_ = admin.CreateTopic(testTopic, &sarama.TopicDetail{
			NumPartitions:     1,
			ReplicationFactor: 1,
		}, false)
	}
	admin.Close()

	// Producer
	prodCfg := sarama.NewConfig()
	prodCfg.Producer.Return.Successes = true
	prodCfg.Producer.RequiredAcks = sarama.WaitForLocal
	producer, err := sarama.NewSyncProducer([]string{testBroker}, prodCfg)
	if err != nil {
		t.Skipf("Kafka producer: %v", err)
	}

	// Consumer
	consCfg := sarama.NewConfig()
	consCfg.Consumer.Return.Errors = true
	consCfg.Consumer.Offsets.Initial = sarama.OffsetOldest
	consumer, err := sarama.NewConsumerGroup([]string{testBroker}, testGroup, consCfg)
	if err != nil {
		producer.Close()
		t.Skipf("Kafka consumer: %v", err)
	}

	return rdb, producer, consumer
}

// TestPipelineEndToEnd publishes synthetic DelayEvents to Kafka, consumes them
// through the processor path (state manager), and verifies delay state in Redis.
func TestPipelineEndToEnd(t *testing.T) {
	rdb, producer, consumer := skipIfNoInfra(t)

	// Only check for goroutine leaks if infra is available (test wasn't skipped).
	defer goleak.VerifyNone(t,
		goleak.IgnoreTopFunction("github.com/IBM/sarama.(*Broker).responseReceiver"),
		goleak.IgnoreTopFunction("github.com/IBM/sarama.(*Broker).sendAndReceive.func1"),
		goleak.IgnoreTopFunction("github.com/IBM/sarama.(*client).backgroundMetadataUpdater"),
		goleak.IgnoreTopFunction("github.com/IBM/sarama.(*consumerGroup).loopCheckPartitionNumbers"),
		goleak.IgnoreTopFunction("internal/poll.runtime_pollWait"),
		goleak.IgnoreAnyFunction("github.com/redis/go-redis/v9/maintnotifications.(*CircuitBreakerManager).cleanupLoop"),
	)
	defer rdb.Close()
	defer producer.Close()
	defer consumer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Clear test state.
	rdb.FlushDB(ctx)

	// Produce synthetic delay events.
	events := []*pb.DelayEvent{
		{
			AgencyId:     "test",
			TripId:       "trip_A_001",
			RouteId:      "A",
			StopId:       "A15",
			DelaySeconds: 120,
			ObservedAt:   time.Now().Unix(),
		},
		{
			AgencyId:     "test",
			TripId:       "trip_B_001",
			RouteId:      "B",
			StopId:       "D14",
			DelaySeconds: 180,
			ObservedAt:   time.Now().Unix(),
		},
		{
			AgencyId:     "test",
			TripId:       "trip_A_002",
			RouteId:      "A",
			StopId:       "A24",
			DelaySeconds: 90,
			ObservedAt:   time.Now().Unix(),
		},
	}

	for _, ev := range events {
		payload, err := proto.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		_, _, err = producer.SendMessage(&sarama.ProducerMessage{
			Topic: testTopic,
			Key:   sarama.StringEncoder(ev.GetRouteId()),
			Value: sarama.ByteEncoder(payload),
		})
		if err != nil {
			t.Fatalf("send: %v", err)
		}
	}

	// Consume and process through state manager.
	mgr := state.NewDelayStateManager(rdb, nil, nil)

	handler := &testConsumerHandler{mgr: mgr, processed: make(chan struct{}, len(events))}
	go func() {
		_ = consumer.Consume(ctx, []string{testTopic}, handler)
	}()

	// Wait for all events to be processed.
	for i := 0; i < len(events); i++ {
		select {
		case <-handler.processed:
		case <-ctx.Done():
			t.Fatal("timeout waiting for events to be processed")
		}
	}
	cancel()

	// Verify delay state in Redis.
	verifyCtx := context.Background()
	for _, ev := range events {
		ds, err := mgr.GetDelay(verifyCtx, ev.AgencyId, ev.TripId, ev.StopId)
		if err != nil {
			t.Errorf("get delay %s/%s: %v", ev.TripId, ev.StopId, err)
			continue
		}
		if ds == nil {
			t.Errorf("delay not found for %s/%s", ev.TripId, ev.StopId)
			continue
		}
		if ds.DelaySeconds != ev.DelaySeconds {
			t.Errorf("delay mismatch for %s: got %d, want %d", ev.TripId, ds.DelaySeconds, ev.DelaySeconds)
		}
		if ds.RouteID != ev.RouteId {
			t.Errorf("route mismatch for %s: got %s, want %s", ev.TripId, ds.RouteID, ev.RouteId)
		}
	}

	// Verify active delays.
	delays, err := mgr.GetAllActiveDelays(verifyCtx, "test")
	if err != nil {
		t.Fatalf("get all active delays: %v", err)
	}
	if len(delays) != 3 {
		t.Errorf("expected 3 active delays, got %d", len(delays))
	}
}

// testConsumerHandler processes messages through the state manager.
type testConsumerHandler struct {
	mgr       *state.DelayStateManager
	processed chan struct{}
}

func (*testConsumerHandler) Setup(_ sarama.ConsumerGroupSession) error   { return nil }
func (*testConsumerHandler) Cleanup(_ sarama.ConsumerGroupSession) error { return nil }

func (h *testConsumerHandler) ConsumeClaim(sess sarama.ConsumerGroupSession, claim sarama.ConsumerGroupClaim) error {
	for msg := range claim.Messages() {
		var ev pb.DelayEvent
		if err := proto.Unmarshal(msg.Value, &ev); err != nil {
			sess.MarkMessage(msg, "")
			continue
		}
		_ = h.mgr.ProcessEvent(sess.Context(), &ev)
		sess.MarkMessage(msg, "")
		select {
		case h.processed <- struct{}{}:
		default:
		}
	}
	return nil
}
