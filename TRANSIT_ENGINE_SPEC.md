# Transit Delay Propagation Engine — Claude Code Implementation Spec

## How to Use This Document

This is a step-by-step implementation spec for building a real-time transit delay propagation engine in Go. **Do not build the entire project at once.** Work through each step sequentially. Each step has explicit acceptance criteria — do not move to the next step until the current step's criteria are met.

When I say "build step N," implement everything in that step, run the acceptance criteria to verify, and stop. Wait for me to confirm before moving on.

---

## Project Overview

A streaming pipeline that ingests live NYC subway GTFS-Realtime protobuf feeds, models the transit network as a temporal graph, detects broken transfer connections caused by delays, and predicts how delays cascade through the network.

### Tech Stack (do not deviate)

- **Language:** Go
- **Event streaming:** Apache Kafka (KRaft mode, no Zookeeper)
- **State store:** Redis
- **Serialization:** Protocol Buffers (protobuf)
- **API:** gRPC
- **Containerization:** Docker Compose for local dev
- **Observability:** Prometheus + Grafana

### Data Sources

- **GTFS-Realtime feeds:** MTA publishes protobuf-encoded HTTP endpoints for each subway division (ACE, BDFM, G, JZ, NQRW, L, 1234567, SIR). Each returns a `FeedMessage` containing `TripUpdate` entities with predicted arrival/departure times per stop. Feeds update every 15–30 seconds.
- **GTFS Static feed:** A zip of CSVs (`stops.txt`, `routes.txt`, `trips.txt`, `stop_times.txt`, `transfers.txt`, `calendar.txt`) defining the subway network topology, schedules, and valid transfer connections.
- **MTA API key:** Not required. All MTA GTFS-RT endpoints are publicly accessible with no authentication.

### Repository Structure (target)

```
transit-delay-engine/
├── cmd/
│   ├── ingestor/          # GTFS-RT poller binary
│   │   └── main.go
│   ├── processor/         # Stream processing binary
│   │   └── main.go
│   └── server/            # gRPC API binary
│       └── main.go
├── internal/
│   ├── ingest/            # Feed polling, diffing, normalization
│   ├── graph/             # Static schedule graph construction + querying
│   ├── state/             # Redis delay state management
│   ├── transfer/          # Transfer impact detection
│   ├── propagation/       # Multi-hop cascade prediction
│   └── reconcile/         # Noisy data reconciliation + smoothing
├── proto/
│   ├── gtfs-realtime.proto      # Google's GTFS-RT spec (vendored)
│   ├── delay_event.proto        # Internal normalized event schema
│   ├── propagation.proto        # Propagation result schema
│   └── service.proto            # gRPC service definition
├── data/
│   └── gtfs_static/       # Downloaded MTA GTFS Static CSVs
├── scripts/
│   ├── download_static.sh # Script to fetch + unzip GTFS Static data
│   └── seed_data.sh       # Optional: seed historical correlation data
├── deployments/
│   ├── docker-compose.yml
│   └── prometheus.yml
├── go.mod
├── go.sum
├── Makefile
└── README.md
```

---

## Step 1: Project Scaffolding + Docker Compose

### What to Build

Set up the Go module, directory structure, Docker Compose with Kafka and Redis, and a Makefile.

### Instructions

1. Initialize Go module: `go mod init github.com/brian/transit-delay-engine` (or whatever module path I specify).
2. Create the full directory structure shown above (empty files/dirs are fine, we'll fill them in).
3. Create `deployments/docker-compose.yml` with:
   - **Kafka** (use `bitnami/kafka:latest`, KRaft mode — no Zookeeper). Expose port 9092. Set `KAFKA_CFG_NODE_ID=1`, `KAFKA_CFG_PROCESS_ROLES=broker,controller`, `KAFKA_CFG_CONTROLLER_QUORUM_VOTERS=1@kafka:9093`, `KAFKA_CFG_LISTENERS=PLAINTEXT://:9092,CONTROLLER://:9093`, `KAFKA_CFG_LISTENER_SECURITY_PROTOCOL_MAP=CONTROLLER:PLAINTEXT,PLAINTEXT:PLAINTEXT`, `KAFKA_CFG_CONTROLLER_LISTENER_NAMES=CONTROLLER`, `KAFKA_CFG_ADVERTISED_LISTENERS=PLAINTEXT://localhost:9092`.
   - **Redis** (`redis:7-alpine`). Expose port 6379.
4. Create a `Makefile` with targets:
   - `infra-up`: `docker compose -f deployments/docker-compose.yml up -d`
   - `infra-down`: `docker compose -f deployments/docker-compose.yml down`
   - `proto`: compile all `.proto` files in `proto/` to Go (using `protoc` with `protoc-gen-go` and `protoc-gen-go-grpc`)
   - `build`: `go build ./...`
   - `run-ingestor`: `go run ./cmd/ingestor`
   - `run-processor`: `go run ./cmd/processor`
   - `run-server`: `go run ./cmd/server`
   - `test`: `go test ./... -v`

### Acceptance Criteria

- `make infra-up` starts Kafka and Redis containers successfully.
- `docker exec` into the Kafka container and run `kafka-topics.sh --list --bootstrap-server localhost:9092` without error.
- `redis-cli ping` returns `PONG`.
- `make build` succeeds (even though binaries are stubs).
- `make infra-down` cleans up.

---

## Step 2: Protobuf Schemas + Code Generation

### What to Build

Vendor the GTFS-Realtime proto, define our internal event schemas, and set up code generation.

### Instructions

1. Download `gtfs-realtime.proto` from `https://github.com/google/transit/blob/master/gtfs-realtime/proto/gtfs-realtime.proto` and place it in `proto/`. Set its `option go_package = "github.com/brian/transit-delay-engine/proto/gtfsrt";`.

2. Create `proto/delay_event.proto`:

```protobuf
syntax = "proto3";
package transit;
option go_package = "github.com/brian/transit-delay-engine/proto/transit";

message DelayEvent {
  string agency_id = 1;
  string trip_id = 2;
  string route_id = 3;
  string stop_id = 4;
  int32 delay_seconds = 5;
  int64 observed_at = 6;
  int64 scheduled_arrival = 7;
  int64 predicted_arrival = 8;
  EventType type = 9;

  enum EventType {
    ARRIVAL_DELAY = 0;
    DEPARTURE_DELAY = 1;
    TRIP_CANCELLED = 2;
    SKIP_STOP = 3;
  }
}

message DelayEventBatch {
  repeated DelayEvent events = 1;
}
```

3. Create `proto/propagation.proto`:

```protobuf
syntax = "proto3";
package transit;
option go_package = "github.com/brian/transit-delay-engine/proto/transit";

message TransferImpact {
  string from_trip_id = 1;
  string from_route_id = 2;
  string to_route_id = 3;
  string station_id = 4;
  int32 source_delay_seconds = 5;
  int32 original_transfer_margin_seconds = 6;
  int32 remaining_margin_seconds = 7;
  ImpactLevel level = 8;
  int64 detected_at = 9;
  string next_viable_trip_id = 10;
  int32 additional_wait_seconds = 11;

  enum ImpactLevel {
    AT_RISK = 0;
    BROKEN = 1;
  }
}

message PropagationResult {
  string source_trip_id = 1;
  string source_stop_id = 2;
  int32 source_delay_seconds = 3;
  repeated DownstreamImpact impacts = 4;
  int64 computed_at = 5;
}

message DownstreamImpact {
  string route_id = 1;
  string stop_id = 2;
  int32 predicted_additional_delay = 3;
  float confidence = 4;
  int32 hops_from_source = 5;
  string impact_type = 6;
}
```

4. Create `proto/service.proto`:

```protobuf
syntax = "proto3";
package transit;
option go_package = "github.com/brian/transit-delay-engine/proto/transit";

service TransitPropagation {
  rpc GetNetworkState(NetworkStateRequest) returns (NetworkStateResponse);
  rpc GetRouteDelays(RouteDelaysRequest) returns (RouteDelaysResponse);
  rpc PredictImpact(PredictImpactRequest) returns (PropagationResult);
  rpc GetBlastRadius(BlastRadiusRequest) returns (BlastRadiusResponse);
  rpc GetTransferReliability(TransferReliabilityRequest) returns (TransferReliabilityResponse);
  rpc StreamAlerts(StreamAlertsRequest) returns (stream TransferImpact);
}

message NetworkStateRequest {}
message NetworkStateResponse {
  map<string, RouteState> routes = 1;
  int64 as_of = 2;
}
message RouteState {
  string route_id = 1;
  repeated StopDelay delays = 2;
}
message StopDelay {
  string stop_id = 1;
  string stop_name = 2;
  int32 delay_seconds = 3;
  float confidence = 4;
}

message RouteDelaysRequest {
  string route_id = 1;
}
message RouteDelaysResponse {
  repeated StopDelay delays = 1;
}

message PredictImpactRequest {
  string trip_id = 1;
  string stop_id = 2;
}

message BlastRadiusRequest {
  string station_id = 1;
}
message BlastRadiusResponse {
  repeated PropagationResult cascades = 1;
}

message TransferReliabilityRequest {
  string station_id = 1;
  int32 hour_of_day = 2;
}
message TransferReliabilityResponse {
  repeated TransferStats transfers = 1;
}
message TransferStats {
  string from_route_id = 1;
  string to_route_id = 2;
  string station_id = 3;
  float reliability_pct = 4;
  float avg_delay_when_broken = 5;
  int32 sample_count = 6;
}

message StreamAlertsRequest {
  repeated string route_ids = 1;
  repeated string station_ids = 2;
}
```

5. Update the `Makefile` `proto` target to compile all three proto files. Generated Go code should go into `proto/gtfsrt/` and `proto/transit/` respectively.

6. Install protobuf Go plugins as Go dependencies: `google.golang.org/protobuf`, `google.golang.org/grpc`, `google.golang.org/grpc/cmd/protoc-gen-go-grpc`.

### Acceptance Criteria

- `make proto` generates Go files under `proto/gtfsrt/` and `proto/transit/` with no errors.
- `make build` compiles successfully with the generated protobuf code.
- The generated types are importable from other packages in the module.

---

## Step 3: GTFS-RT Feed Ingestion

### What to Build

A poller that fetches live MTA GTFS-Realtime feeds, deserializes protobuf, diffs against previous snapshots to extract changes, normalizes into our `DelayEvent` schema, and publishes to Kafka.

### Instructions

1. Implement `internal/ingest/poller.go`:
   - `Poller` struct that takes a feed URL, agency ID, polling interval, and Kafka producer.
   - `Start()` method that runs a ticker loop: fetch feed → deserialize `FeedMessage` → diff → normalize → publish.
   - Use `http.Client` with a 10-second timeout.
   - Deserialize the HTTP response body directly into `gtfs_realtime.FeedMessage` using `proto.Unmarshal`.

2. Implement `internal/ingest/differ.go`:
   - `Differ` struct that stores the previous `FeedMessage`.
   - `Diff(current *FeedMessage) []DelayEvent` method that compares current vs. previous feed, extracting only trips/stops where the delay has changed by more than 15 seconds (to filter noise).
   - On first poll (no previous snapshot), treat everything as new.

3. Implement `internal/ingest/normalizer.go`:
   - Convert GTFS-RT `TripUpdate.StopTimeUpdate` entries into our internal `DelayEvent` proto.
   - Compute `delay_seconds` as `actual_arrival - scheduled_arrival` from the `StopTimeEvent` fields.
   - Set `observed_at` to the feed's `header.timestamp`.
   - Extract `route_id` from the `TripDescriptor`. MTA encodes route in the trip ID (e.g., trip ID `130200_A..N` → route `A`). Parse this.

4. Implement `cmd/ingestor/main.go`:
   - Load `config.yaml` to get feed URLs and Kafka config.
   - Create Kafka producer (use `github.com/IBM/sarama` or `confluent-kafka-go` — pick one and stay consistent).
   - Create Kafka topic `delay-events` with 10 partitions on startup if it doesn't exist.
   - Launch one `Poller` goroutine per feed defined in config. Start with just ACE and L for development (the others can be enabled by uncommenting in config).
   - Partition Kafka messages by `route_id`.
   - Log events/second throughput to stdout.
   - Handle graceful shutdown on SIGINT/SIGTERM.

5. Create a simple `cmd/ingestor/consumer_test.go` or standalone script that reads from the `delay-events` topic and prints deserialized `DelayEvent` messages to stdout for manual verification.

### MTA Feed URLs

All MTA GTFS-RT endpoints are public with no authentication. The ingestor should accept feed URLs via config (YAML file) so they can be updated without code changes. Provide a `config.yaml` with all subway feeds:

```yaml
agency_id: "mta"
feeds:
  - name: "1234567S"
    url: "https://api-endpoint.mta.info/Dataservice/mtagtfsfeeds/nyct%2Fgtfs"
    poll_interval: 15s
    routes: ["1", "2", "3", "4", "5", "6", "7", "GS"]
  - name: "ACE"
    url: "https://api-endpoint.mta.info/Dataservice/mtagtfsfeeds/nyct%2Fgtfs-ace"
    poll_interval: 15s
    routes: ["A", "C", "E", "SR"]
  - name: "BDFM"
    url: "https://api-endpoint.mta.info/Dataservice/mtagtfsfeeds/nyct%2Fgtfs-bdfm"
    poll_interval: 15s
    routes: ["B", "D", "F", "M", "SF"]
  - name: "G"
    url: "https://api-endpoint.mta.info/Dataservice/mtagtfsfeeds/nyct%2Fgtfs-g"
    poll_interval: 15s
    routes: ["G"]
  - name: "JZ"
    url: "https://api-endpoint.mta.info/Dataservice/mtagtfsfeeds/nyct%2Fgtfs-jz"
    poll_interval: 15s
    routes: ["J", "Z"]
  - name: "NQRW"
    url: "https://api-endpoint.mta.info/Dataservice/mtagtfsfeeds/nyct%2Fgtfs-nqrw"
    poll_interval: 15s
    routes: ["N", "Q", "R", "W"]
  - name: "L"
    url: "https://api-endpoint.mta.info/Dataservice/mtagtfsfeeds/nyct%2Fgtfs-l"
    poll_interval: 15s
    routes: ["L"]
  - name: "SIR"
    url: "https://api-endpoint.mta.info/Dataservice/mtagtfsfeeds/nyct%2Fgtfs-si"
    poll_interval: 15s
    routes: ["SI"]
polling_default_interval: 15s
kafka_brokers:
  - "localhost:9092"
kafka_topic: "delay-events"
```

### Acceptance Criteria

- `make infra-up && make run-ingestor` connects to MTA feeds and begins polling.
- The consumer script shows `DelayEvent` messages arriving on the `delay-events` topic with valid `route_id`, `stop_id`, `trip_id`, and `delay_seconds` values.
- Events are partitioned by `route_id` in Kafka (verify with `kafka-console-consumer.sh` with `--property print.key=true`).
- Incremental diffing works: after the first full snapshot, subsequent polls emit only changed delays (verify by watching event count drop after the first poll cycle).
- The ingestor logs throughput (events/sec) to stdout.
- Graceful shutdown works — Ctrl+C cleanly closes the Kafka producer and exits.

---

## Step 4: GTFS Static Graph Builder

### What to Build

A parser that reads MTA's GTFS Static CSV files and builds an in-memory temporal graph representing the subway network, its schedules, and transfer connections.

### Instructions

1. Create `scripts/download_static.sh`:
   - Download the MTA GTFS Static feed zip from `http://web.mta.info/developers/data/nyct/subway/google_transit.zip`.
   - Unzip into `data/gtfs_static/`.
   - This script should be idempotent (skip download if files exist and are recent).

2. Implement `internal/graph/parser.go`:
   - Parse `stops.txt` into a map of `stop_id → Stop{ID, Name, Lat, Lon, ParentStation}`. MTA uses parent stations — stop IDs like `A32N` and `A32S` are child stops (northbound/southbound platforms) of parent station `A32`. Normalize to parent station IDs for transfer purposes.
   - Parse `routes.txt` into a map of `route_id → Route{ID, ShortName, LongName, Color}`.
   - Parse `stop_times.txt` into a list of `StopTime{TripID, StopID, ArrivalTime, DepartureTime, StopSequence}`. Note: GTFS times can exceed 24:00:00 for trips crossing midnight (e.g., `25:30:00` = 1:30 AM). Handle this.
   - Parse `trips.txt` to map `trip_id → Trip{RouteID, ServiceID, DirectionID}`.
   - Parse `transfers.txt` into a list of `Transfer{FromStopID, ToStopID, TransferType, MinTransferTime}`. `TransferType=2` means timed transfer with `MinTransferTime` seconds.
   - Parse `calendar.txt` and `calendar_dates.txt` to determine which `service_id` values are active on a given date.

3. Implement `internal/graph/graph.go`:
   - `TransitGraph` struct holding:
     - `Stops map[string]*Stop`
     - `Routes map[string]*Route`
     - `StopTimesByStop map[string][]*ScheduledStopTime` — for a given stop, all scheduled arrivals/departures sorted by time.
     - `TransfersByStop map[string][]*Transfer` — for a given stop (parent station), all outbound transfers.
     - `TripRoute map[string]string` — trip_id → route_id lookup.
   - `BuildGraph(dataDir string, date time.Time) (*TransitGraph, error)` — loads CSVs, filters to active services for the given date, builds indices.
   - `GetTransfersFrom(stopID string) []*Transfer` — returns all outbound transfers from a station.
   - `GetNextDepartures(stopID string, routeID string, afterTime int) []*ScheduledStopTime` — returns the next N scheduled departures on a given route from a given stop after a given time-of-day (in seconds since midnight). This is critical for determining "if you miss this connection, when's the next one?"
   - `GetDownstreamStops(routeID string, stopID string, directionID int) []*ScheduledStopTime` — returns all stops downstream of a given stop on a given route and direction.

4. Write unit tests in `internal/graph/graph_test.go`:
   - Test that the parser correctly handles 24+ hour times.
   - Test that parent station normalization works.
   - Test that `GetTransfersFrom` returns correct transfers for a known station (e.g., 14th St–Union Square has transfers between 4/5/6, L, N/Q/R/W).
   - Test that `GetNextDepartures` returns correctly ordered results.

### Acceptance Criteria

- `make download-static` (add this to Makefile) downloads and unzips the GTFS Static data.
- Unit tests pass.
- A simple main program can load the graph and print stats: total stops, total routes, total transfers, average transfers per station. For the MTA subway, expect ~470 stops, ~26 routes, and ~600+ transfer pairs.
- `GetTransfersFrom("631")` (14th St–Union Square) returns transfers to the L, N, Q, R, W, 4, 5, 6 routes.

---

## Step 5: Delay State Manager (Redis)

### What to Build

A Kafka consumer that reads `DelayEvent` messages from the `delay-events` topic and maintains a real-time, reconciled delay state in Redis.

### Instructions

1. Implement `internal/state/manager.go`:
   - `DelayStateManager` struct with a Redis client and reconciliation config.
   - `ProcessEvent(event *DelayEvent) error` — the main entry point:
     - Redis key format: `delay:{agency_id}:{trip_id}:{stop_id}`
     - Redis value: JSON with `{delay_seconds, observed_at, confidence, update_count}`
     - Set TTL of 2 hours on each key (trips don't last longer than that).
     - **Out-of-order rejection:** If the stored `observed_at` is newer than the incoming event's, discard silently.
     - **Smoothing:** If the new delay differs from the stored delay by more than 120 seconds and `update_count < 3`, apply exponential smoothing: `new_delay = 0.7 * incoming + 0.3 * stored`. This handles the MTA's habit of publishing wildly wrong predictions that self-correct.
     - **Confidence scoring:** Confidence starts at 0.5 on first observation. Each subsequent observation within ±30 seconds of the current value increases confidence by 0.1 (cap at 1.0). Observations that diverge significantly reset confidence to 0.5.
   - `GetDelay(agencyID, tripID, stopID string) (*DelayState, error)` — read current delay from Redis.
   - `GetRouteDelays(agencyID, routeID string) ([]*DelayState, error)` — scan for all delays on a route. Use a Redis key pattern scan: `delay:{agency_id}:*` and filter by route using the graph's `TripRoute` mapping.
   - `GetAllActiveDelays(agencyID string) ([]*DelayState, error)` — return all current delays above a threshold (e.g., > 60 seconds).

2. Implement `cmd/processor/main.go` (initial version):
   - Create Kafka consumer subscribed to `delay-events` topic.
   - Create Redis client.
   - Load the transit graph (from Step 4).
   - For each message: deserialize `DelayEvent`, pass to `DelayStateManager.ProcessEvent()`.
   - Log summary stats every 30 seconds: total active delays, average delay, max delay.

3. Add a CLI command or script `cmd/delays/main.go` that connects to Redis and prints a formatted table of current delays by route, refreshing every 5 seconds. This is your development debugging tool.

### Acceptance Criteria

- With the ingestor and processor both running (`make run-ingestor` in one terminal, `make run-processor` in another), Redis populates with delay state.
- `redis-cli keys "delay:*" | wc -l` shows active delay keys.
- The debug CLI shows delays updating in real time.
- Out-of-order events are silently dropped (verify with logging at DEBUG level).
- Stale keys auto-expire after 2 hours (verify by setting a short TTL for testing).

---

## Step 6: Transfer Impact Detection

### What to Build

Logic that cross-references live delay state against the static schedule graph to detect broken and at-risk transfer connections in real time.

### Instructions

1. Implement `internal/transfer/detector.go`:
   - `TransferDetector` struct taking a `TransitGraph`, `DelayStateManager`, and Kafka producer.
   - `EvaluateDelay(event *DelayEvent) ([]*TransferImpact, error)`:
     1. Look up the stop's parent station.
     2. Get all outbound transfers from that station via `graph.GetTransfersFrom()`.
     3. For each transfer to `(to_route, to_stop)`:
        - Get the current time of day from the event's `observed_at`.
        - Find the next scheduled departure on `to_route` from `to_stop` after the original scheduled arrival time of the delayed trip (use `graph.GetNextDepartures()`).
        - Compute original margin: `next_departure_on_to_route - scheduled_arrival_of_delayed_trip`.
        - Compute remaining margin: `original_margin - delay_seconds`.
        - If `remaining_margin < min_transfer_time` → `BROKEN`.
        - If `remaining_margin < min_transfer_time + 60` → `AT_RISK`.
        - For broken transfers, find the *next viable* departure: the first departure on `to_route` after `predicted_arrival + min_transfer_time`. Compute `additional_wait_seconds`.
     4. Check if the connecting route is also delayed (query `DelayStateManager`). If the connecting route is *also* late, the transfer might still be viable — adjust the margin.
     5. Return the list of `TransferImpact` protos.
   - Publish detected impacts to a Kafka topic `transfer-impacts`.

2. Update `cmd/processor/main.go`:
   - After processing a `DelayEvent` through the state manager, pass it to `TransferDetector.EvaluateDelay()`.
   - Log detected broken/at-risk transfers to stdout with a clear format: `[BROKEN] A→L at 14th St-Union Sq | A train 8min late | next L in 12min (+7min wait)`.
   - Publish `TransferImpact` messages to the `transfer-impacts` Kafka topic.

3. Create Kafka topic `transfer-impacts` with 10 partitions in the processor startup.

### Edge Cases to Handle

- **No scheduled service on connecting route:** Late at night, some routes stop running. If there's no next departure, skip the transfer.
- **Multiple transfer options:** A station might have transfers to 5 different routes. Evaluate all of them.
- **Both routes delayed:** If the A is 5 min late and the L is 3 min late, the effective margin loss is only 2 minutes (5 - 3). But only apply this if the L's delay is at the *same station* or upstream of it.
- **High-frequency routes:** If the L runs every 3 minutes at rush hour, a broken transfer adds at most 3 minutes of wait. Weight the impact accordingly — include `additional_wait_seconds` in the output.

### Acceptance Criteria

- With the full pipeline running (ingestor → processor), broken transfer alerts appear in the processor logs.
- Transfer impact events are published to the `transfer-impacts` Kafka topic (verify with a console consumer).
- A broken transfer on a high-frequency route shows a small `additional_wait_seconds` (3–5 min). A broken transfer on a low-frequency route (late night) shows a large `additional_wait_seconds` (15–30 min).
- If you manually delay both sides of a transfer (or wait for it to happen naturally), the detector accounts for the connecting route's delay.

---

## Step 7: gRPC API Server

### What to Build

A gRPC server exposing the system's real-time state and predictions to consumers.

### Instructions

1. Implement `cmd/server/main.go`:
   - Load the transit graph.
   - Connect to Redis.
   - Subscribe to `transfer-impacts` Kafka topic for streaming alerts.
   - Start gRPC server on port 50051.

2. Implement service handlers in `internal/server/handlers.go`:
   - `GetNetworkState` — Query `DelayStateManager.GetAllActiveDelays()`, group by route, return a map of route → delays.
   - `GetRouteDelays` — Query delays for a specific route.
   - `StreamAlerts` — Server-side streaming. Maintain a channel of `TransferImpact` events fed by the Kafka consumer. When a client subscribes (optionally filtered by route or station IDs), stream matching events to them. Handle client disconnection gracefully.

3. Write a `cmd/client/main.go` test client:
   - Connect to the gRPC server.
   - Call `GetNetworkState` and print the current delay map.
   - Call `StreamAlerts` with no filter and print incoming alerts.
   - This is a development/demo tool.

### Acceptance Criteria

- `make run-server` starts the gRPC server on port 50051.
- The test client can call `GetNetworkState` and receive current delays.
- The test client can stream alerts and receive transfer impact events in real time.
- Multiple clients can connect and stream simultaneously.
- Client disconnection doesn't crash the server.

---

## Step 8: Propagation Engine

### What to Build

The core prediction engine: when a transfer breaks, walk the temporal graph forward to predict cascading downstream delays.

### Instructions

1. Implement `internal/propagation/engine.go`:
   - `PropagationEngine` struct with `TransitGraph`, `DelayStateManager`, and `HistoricalStore`.
   - `Propagate(impact *TransferImpact) (*PropagationResult, error)`:
     1. Start from the broken transfer's connecting route and station.
     2. Estimate the passenger load increase on the next viable train (simple model: assume N% of riders at that station were waiting for the broken connection).
     3. For each downstream stop on the connecting route:
        - Estimate additional dwell time from the load increase (linear model: `extra_dwell = base_dwell * load_increase_pct * dwell_elasticity`). Start with `dwell_elasticity = 0.3`.
        - Sum accumulated dwell increases as `predicted_additional_delay`.
        - At each stop, check if this accumulated delay breaks any *further* transfers from that stop. If yes, recursively propagate (with depth limit).
     4. Apply a **decay function** at each hop: `confidence = initial_confidence * (0.7 ^ hops_from_source)`. Stop propagating when confidence drops below 0.2 or predicted delay drops below 15 seconds.
     5. Use a **priority queue** (Go's `container/heap`): process the highest-impact nodes first. If the system is under load, this ensures the most important predictions are computed.
     6. Build and return a `PropagationResult` with all `DownstreamImpact` entries.

2. Implement `internal/propagation/history.go` (Historical Correlation Store):
   - `HistoricalStore` struct backed by Redis sorted sets.
   - `RecordObservation(fromRoute, toRoute, stationID string, hourOfDay int, sourceDelay int, downstreamImpact int)` — add an observation to the sorted set keyed by `history:{from}:{to}:{station}:{hour}`, scored by timestamp.
   - `GetAverageImpact(fromRoute, toRoute, stationID string, hourOfDay int) (float64, int, error)` — return the average downstream impact and sample count from the last 30 days of observations.
   - If historical data exists for a transfer pair, use it to weight the propagation prediction instead of the simple linear model.
   - Trim sorted sets to 30 days of data on each write.

3. Integrate into `cmd/processor/main.go`:
   - After detecting a broken transfer, run the propagation engine.
   - Publish `PropagationResult` to a `network-predictions` Kafka topic.
   - Feed actual observed downstream delays back into the `HistoricalStore` (compare predicted vs. actual after a time window).

4. Add gRPC endpoints to the server:
   - `PredictImpact(trip_id, stop_id)` — run propagation for a specific delayed trip.
   - `GetBlastRadius(station_id)` — find all current delays at a station and run propagation for each.
   - `GetTransferReliability(station_id, hour_of_day)` — return historical reliability stats for transfers at a station.

### Acceptance Criteria

- When a transfer breaks, the propagation engine produces a `PropagationResult` with at least 1 downstream impact.
- Confidence decays with each hop (verify in output).
- Propagation stops when confidence drops below 0.2 or delay drops below 15 seconds (no infinite fan-out).
- Historical observations accumulate in Redis (`redis-cli zcard "history:A:L:631:8"` shows a count).
- The gRPC `PredictImpact` endpoint returns propagation results.
- The system doesn't block or slow down during propagation (async/non-blocking design).

---

## Step 9: Observability + Hardening

### What to Build

Prometheus metrics, Grafana dashboards, backpressure handling, graceful shutdown with state recovery, and integration tests.

### Instructions

1. **Prometheus instrumentation** — add metrics to all components using `prometheus/client_golang`:
   - Ingestor: `ingestor_events_total` (counter by route), `ingestor_poll_duration_seconds` (histogram), `ingestor_poll_errors_total` (counter).
   - Processor: `processor_events_processed_total`, `processor_event_processing_duration_seconds`, `processor_active_delays` (gauge), `processor_broken_transfers_total` (counter), `processor_propagation_fan_out` (histogram).
   - Server: `server_grpc_requests_total`, `server_grpc_request_duration_seconds`, `server_active_streams` (gauge).
   - Redis: `redis_operation_duration_seconds` (histogram by operation), `redis_keys_total` (gauge).
   - Kafka: `kafka_consumer_lag` (gauge by topic/partition).

2. **Prometheus config** — create `deployments/prometheus.yml` scraping all three services (ingestor on :9090/metrics, processor on :9091/metrics, server on :9092/metrics — pick non-conflicting ports).

3. **Docker Compose update** — add Prometheus and Grafana containers. Grafana should provision a dashboard JSON automatically (create `deployments/grafana/dashboards/transit.json` and a provisioning config).

4. **Grafana dashboard** — create a dashboard with panels for:
   - Events/sec ingested (by route)
   - Processing latency p50/p99
   - Active delays count
   - Broken transfers/min
   - Kafka consumer lag
   - Propagation fan-out distribution
   - Redis memory usage

5. **Backpressure handling** in the processor:
   - Monitor Kafka consumer lag. If lag exceeds a threshold (e.g., 1000 messages), switch to "catch-up mode": skip propagation (expensive) and only update delay state. Resume full processing when lag drops below 100.
   - Log when entering/exiting catch-up mode.

6. **Graceful shutdown**:
   - On SIGINT/SIGTERM, flush Kafka producers, commit consumer offsets, close Redis connections.
   - On restart, the consumer picks up from the last committed offset — no data loss.

7. **Integration tests** in `tests/integration/`:
   - Record a 5-minute sample of real GTFS-RT feed responses (save as binary protobuf files).
   - Write a test that replays these recorded feeds through the full pipeline (ingestor → Kafka → processor → Redis) and asserts:
     - Delay state is populated in Redis.
     - At least one transfer impact is detected (if the sample contains delays).
     - No goroutine leaks (use `goleak`).

### Acceptance Criteria

- `make infra-up` now starts Kafka, Redis, Prometheus, and Grafana.
- Grafana is accessible at `http://localhost:3000` with a pre-provisioned dashboard showing live metrics.
- The dashboard shows non-zero values for all panels when the pipeline is running.
- Consumer lag panel works and reflects actual lag.
- Integration tests pass: `go test ./tests/integration/ -v`.
- Graceful shutdown works without losing consumer offsets.
- Backpressure mode activates when manually induced (e.g., pause the processor for 30 seconds, resume, observe catch-up mode in logs).

---

## Step 10 (Stretch): Multi-Agency + Second Feed

### What to Build

Add a second transit agency (Chicago CTA) to validate that the architecture generalizes beyond the MTA.

### Instructions

1. Add CTA feed URLs to `config.yaml`. CTA publishes GTFS-RT feeds at `https://www.transitchicago.com/developers/ttdocs/` (train tracker API, different format — verify the protobuf endpoint).
2. Download CTA GTFS Static data and add a second entry to `download_static.sh`.
3. Ensure the graph builder can load multiple agencies. The graph should be keyed by `agency_id` at the top level.
4. Ensure all Redis keys, Kafka partitioning, and gRPC responses correctly scope by agency.
5. Verify that transfer detection and propagation work for CTA independently.

### Acceptance Criteria

- The ingestor polls both MTA and CTA feeds simultaneously.
- `GetNetworkState` returns delays for both agencies, scoped by `agency_id`.
- Transfer detection works for CTA stations.
- No cross-contamination between agencies in Redis state.

---

## General Guidelines for All Steps

- **Error handling:** Never panic. Return errors up the call stack. Log errors with context (which feed, which trip, which stop). Use `fmt.Errorf` with `%w` for wrapping.
- **Logging:** Use `log/slog` (Go 1.21+ structured logging). Log at INFO level for operational events (polling, connection established, topic created). Log at DEBUG level for per-event processing. Log at WARN for recoverable issues (feed timeout, stale data). Log at ERROR for failures.
- **Testing:** Write unit tests for all non-trivial logic. Use table-driven tests. Mock Redis and Kafka in unit tests; use real instances only in integration tests.
- **Configuration:** Use environment variables for secrets (API keys). Use a YAML config file for everything else. Use `gopkg.in/yaml.v3` for parsing.
- **No premature optimization.** Get correctness first, then optimize if profiling shows bottlenecks. The exception is Kafka partitioning — get that right from the start.
- **Git:** Commit after each step with a meaningful message. Tag each step completion (e.g., `v0.1-ingestion`, `v0.2-graph`).
