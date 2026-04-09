package metrics

import "github.com/prometheus/client_golang/prometheus"

// Ingestor metrics
var (
	IngestorEventsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "ingestor_events_total",
			Help: "Total number of events ingested, by route.",
		},
		[]string{"route"},
	)

	IngestorPollDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "ingestor_poll_duration_seconds",
			Help:    "Duration of ingestor poll operations in seconds.",
			Buckets: prometheus.DefBuckets,
		},
	)

	IngestorPollErrors = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "ingestor_poll_errors_total",
			Help: "Total number of ingestor poll errors.",
		},
	)
)

// Processor metrics
var (
	ProcessorEventsProcessed = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "processor_events_processed_total",
			Help: "Total number of events processed.",
		},
	)

	ProcessorEventDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "processor_event_processing_duration_seconds",
			Help:    "Duration of event processing in seconds.",
			Buckets: prometheus.DefBuckets,
		},
	)

	ProcessorActiveDelays = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "processor_active_delays",
			Help: "Number of currently active delays.",
		},
	)

	ProcessorBrokenTransfers = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "processor_broken_transfers_total",
			Help: "Total number of broken transfers detected.",
		},
	)

	ProcessorPropagationFanOut = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "processor_propagation_fan_out",
			Help:    "Distribution of propagation fan-out counts.",
			Buckets: []float64{0, 1, 2, 5, 10, 20, 50, 100},
		},
	)
)

// Server metrics
var (
	ServerGRPCRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "server_grpc_requests_total",
			Help: "Total number of gRPC requests, by method.",
		},
		[]string{"method"},
	)

	ServerGRPCDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "server_grpc_request_duration_seconds",
			Help:    "Duration of gRPC requests in seconds, by method.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"method"},
	)

	ServerActiveStreams = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "server_active_streams",
			Help: "Number of currently active gRPC streams.",
		},
	)
)

// Redis metrics
var (
	RedisOperationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "redis_operation_duration_seconds",
			Help:    "Duration of Redis operations in seconds, by operation.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"operation"},
	)

	RedisKeysTotal = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "redis_keys_total",
			Help: "Total number of keys in Redis.",
		},
	)
)

// Kafka metrics
var (
	KafkaConsumerLag = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "kafka_consumer_lag",
			Help: "Kafka consumer lag by topic and partition.",
		},
		[]string{"topic", "partition"},
	)
)

func init() {
	prometheus.MustRegister(
		// Ingestor
		IngestorEventsTotal,
		IngestorPollDuration,
		IngestorPollErrors,
		// Processor
		ProcessorEventsProcessed,
		ProcessorEventDuration,
		ProcessorActiveDelays,
		ProcessorBrokenTransfers,
		ProcessorPropagationFanOut,
		// Server
		ServerGRPCRequests,
		ServerGRPCDuration,
		ServerActiveStreams,
		// Redis
		RedisOperationDuration,
		RedisKeysTotal,
		// Kafka
		KafkaConsumerLag,
	)
}
