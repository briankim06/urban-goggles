# Transit Delay Propagation Engine

A real-time streaming pipeline that ingests live NYC subway GTFS-Realtime feeds, models the transit network as a temporal graph, detects broken transfer connections caused by delays, and predicts how delays cascade through the network.

## Architecture

```text
MTA GTFS-RT Feeds
      │
      ▼
┌─────────────┐    Kafka     ┌─────────────┐    Redis     ┌─────────────┐
│   Ingestor  │──────────────▶  Processor   │──────────────▶  gRPC API   │
│  (poller +  │ delay-events │ (state mgr + │  delay state │  (queries + │
│   differ)   │              │  transfer +  │              │  streaming) │
└─────────────┘              │ propagation) │              └─────────────┘
                             └──────┬───────┘
                                    │ transfer-impacts
                                    │ network-predictions
                                    ▼
                              Kafka Topics
```

**Ingestor** — polls MTA protobuf feeds every 15s, diffs against previous snapshots, normalises into `DelayEvent` messages, and publishes to Kafka partitioned by route.

**Processor** — consumes delay events, reconciles noisy data with exponential smoothing and confidence scoring in Redis, detects broken/at-risk transfer connections against the static schedule graph, and runs a multi-hop propagation engine that predicts cascading downstream delays.

**gRPC Server** — exposes network state, per-route delays, blast-radius predictions, transfer reliability stats, and a server-streaming alerts endpoint.

## Tech Stack

| Layer | Technology |
|---|---|
| Language | Go 1.25 |
| Event Streaming | Apache Kafka (KRaft mode, no Zookeeper) |
| State Store | Redis 7 |
| Serialization | Protocol Buffers |
| API | gRPC |
| Containerization | Docker Compose |
| Observability | Prometheus + Grafana |

## Data Sources

- **GTFS-Realtime** — MTA publishes protobuf-encoded HTTP endpoints for each subway division (ACE, BDFM, G, JZ, NQRW, L, 1234567, SIR). Feeds update every 15–30 s. No API key required.
- **GTFS Static** — A zip of CSVs (`stops.txt`, `routes.txt`, `trips.txt`, `stop_times.txt`, `transfers.txt`, `calendar.txt`) defining the subway network topology, schedules, and valid transfer connections.

## Prerequisites

- [Go 1.25+](https://go.dev/dl/)
- [Docker](https://docs.docker.com/get-docker/) and Docker Compose
- [protoc](https://grpc.io/docs/protoc-installation/) with `protoc-gen-go` and `protoc-gen-go-grpc`
- `curl` and `unzip` (for downloading GTFS Static data)

## Quick Start

```bash
# 1. Clone the repo
git clone https://github.com/briankim06/urban-goggles.git
cd urban-goggles

# 2. Start infrastructure (Kafka, Redis, Prometheus, Grafana)
make infra-up

# 3. Download MTA GTFS Static schedule data
make download-static

# 4. Generate protobuf Go code
make proto

# 5. Build all binaries
make build

# 6. Run each component (in separate terminals)
make run-ingestor    # Polls MTA feeds → Kafka
make run-processor   # Kafka → state/transfer/propagation → Redis
make run-server      # gRPC API on :50051
```

## Makefile Targets

| Target | Description |
|---|---|
| `make infra-up` | Start Kafka, Redis, Prometheus, and Grafana via Docker Compose |
| `make infra-down` | Tear down all infrastructure containers |
| `make proto` | Compile `.proto` files to generated Go code |
| `make build` | Build all Go packages |
| `make run-ingestor` | Run the GTFS-RT feed ingestor |
| `make run-processor` | Run the stream processor (state + transfer + propagation) |
| `make run-server` | Run the gRPC API server on port 50051 |
| `make test` | Run all unit tests |
| `make download-static` | Download and unzip MTA GTFS Static data into `data/gtfs_static/` |

## Project Structure

```
transit-delay-engine/
├── cmd/
│   ├── ingestor/       # GTFS-RT poller binary
│   ├── processor/      # Stream processing binary
│   ├── server/         # gRPC API binary
│   ├── delays/         # CLI tool: live delay table (dev debugging)
│   ├── consumer/       # CLI tool: raw Kafka event viewer
│   └── client/         # CLI tool: gRPC test client
├── internal/
│   ├── ingest/         # Feed polling, diffing, normalisation
│   ├── graph/          # GTFS Static graph construction + querying
│   ├── state/          # Redis delay state management + reconciliation
│   ├── transfer/       # Transfer impact detection
│   ├── propagation/    # Multi-hop cascade prediction + history
│   ├── server/         # gRPC handlers + alert streaming
│   └── metrics/        # Prometheus instrumentation
├── proto/
│   ├── gtfs-realtime.proto   # Vendored Google GTFS-RT spec
│   ├── delay_event.proto     # Internal normalised event schema
│   ├── propagation.proto     # Propagation result schema
│   └── service.proto         # gRPC service definition
├── data/gtfs_static/         # Downloaded MTA GTFS Static CSVs (gitignored)
├── scripts/
│   └── download_static.sh    # Idempotent GTFS Static downloader
├── deployments/
│   ├── docker-compose.yml    # Kafka, Redis, Prometheus, Grafana
│   ├── prometheus.yml
│   └── grafana/              # Dashboard + datasource provisioning
├── tests/integration/        # Full-pipeline integration tests
├── config.yaml               # Feed URLs, Kafka brokers, polling config
├── Makefile
├── go.mod
└── go.sum
```

## Configuration

Feed URLs and infrastructure addresses are configured in `config.yaml`. By default, only ACE and L feeds are enabled; uncomment the rest for full subway coverage.

```yaml
agency_id: "mta"
polling_default_interval: 15s
kafka_brokers:
  - "localhost:9092"
kafka_topic: "delay-events"

feeds:
  - name: "ACE"
    url: "https://api-endpoint.mta.info/Dataservice/mtagtfsfeeds/nyct%2Fgtfs-ace"
    poll_interval: 15s
    routes: ["A", "C", "E", "SR"]
  - name: "L"
    url: "https://api-endpoint.mta.info/Dataservice/mtagtfsfeeds/nyct%2Fgtfs-l"
    poll_interval: 15s
    routes: ["L"]
  # ... additional feeds commented out in config.yaml
```

## gRPC API

The server listens on port **50051** and exposes the `TransitPropagation` service:

| RPC | Description |
|---|---|
| `GetNetworkState()` | Current delays across all routes |
| `GetRouteDelays(route_id)` | Delays for a specific route |
| `PredictImpact(trip_id, stop_id)` | Run propagation prediction for a delayed trip |
| `GetBlastRadius(station_id)` | Cascading impact from all delays at a station |
| `GetTransferReliability(station_id, hour)` | Historical transfer reliability stats |
| `StreamAlerts(route_ids?, station_ids?)` | Server-streaming broken/at-risk transfer alerts |

Use the included test client for quick exploration:

```bash
go run ./cmd/client
```

## Observability

With `make infra-up`, Prometheus and Grafana start automatically.

- **Grafana** — [http://localhost:3000](http://localhost:3000) (admin / admin). A pre-provisioned dashboard shows events/sec, processing latency, active delays, broken transfers/min, Kafka consumer lag, propagation fan-out, and Redis memory.
- **Prometheus** — [http://localhost:9093](http://localhost:9093)

Metrics endpoints:
- Ingestor: `:9090/metrics`
- Processor: `:9091/metrics`
- Server: `:9094/metrics`

## Key Design Decisions

- **Incremental diffing** — Only events where delay changed by >15 s are emitted, filtering MTA feed noise.
- **Exponential smoothing** — Wild initial predictions are dampened (70/30 weighted blend) until confidence stabilises.
- **Confidence scoring** — Starts at 0.5; corroborating observations raise it, divergent ones reset it.
- **Backpressure handling** — When Kafka consumer lag exceeds a threshold, the processor drops expensive propagation work and catches up on state updates only.
- **Propagation decay** — Cascade predictions decay confidence by 0.7x per hop and stop when confidence < 0.2 or predicted delay < 15 s.
- **Historical correlation** — Redis sorted sets store observed transfer impacts, used to weight predictions against empirical data.

## Testing

```bash
# Unit tests
make test

# Integration tests (requires infrastructure running)
go test ./tests/integration/ -v
```

## License

Private — not currently published under an open-source license.
