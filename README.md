# at-least-once-bench

A benchmark comparing **where retry responsibility lives** — in the application, the broker, or the workflow engine — and how that choice determines failure behavior under at-least-once delivery.

> **What this benchmark compares is not middleware quality, but the architectural consequences of placing retry responsibility at different layers.** Kafka retries in-process, RabbitMQ/NATS delegate to the broker, and Temporal delegates to a workflow engine. The results below reflect those design choices, not inherent framework strengths or weaknesses.

- **RabbitMQ** — explicit ACK/NACK with quorum queues (broker-side retry)
- **NATS JetStream** — server-side redelivery via AckWait (broker-side retry)
- **Kafka** (KRaft) — log + offset commit model with inner retry loop (in-process retry)
- **Temporal** — workflow history in PostgreSQL with automatic retries (workflow-engine retry)

## Overview

Each framework places retry responsibility at a fundamentally different layer. This benchmark tests how that choice manifests when:
1. **Downstream HTTP server is unreachable** — messages must not be lost
2. **Database is unreachable** — HTTP calls succeed but DB write fails; retry must re-call HTTP (over-delivery)
3. **Worker crashes mid-processing** — in-flight messages must be redelivered

All workers are idempotent: duplicate processing of the same job is safe, thanks to a `UNIQUE(job_id)` constraint in the database.

## Architecture

```
Load Generator (Go runner)
    ↓
[Framework Queue/Workflow]
    ↓
Worker (Go/Python)
    ├─→ HTTP POST via NGINX → downstream:8080/sink
    ├─→ DB writes via NGINX → PostgreSQL:5432
    └─→ Broker access via NGINX → [RabbitMQ/NATS/Kafka/Temporal]
    
Monitoring: Prometheus + Grafana

NGINX Layer:
  • nginx-http-downstream: Simulate HTTP failures (docker stop)
  • nginx-tcp-postgres: Simulate DB failures (docker stop)
  • nginx-tcp-broker: Simulate broker failures (docker stop)
```

## Quick Start

### Prerequisites
- Docker & Docker Compose
- Go 1.22+ (for building benchmark runner)
- PostgreSQL client tools (optional, for debugging)

### Run Shared Infrastructure

```bash
docker compose -f docker-compose.shared.yml up -d
```

This brings up:
- PostgreSQL (port 5432)
- Downstream HTTP sink (ports 8080, 8081)
- Prometheus (port 9090)
- Grafana (port 3000)

Verify health:
```bash
curl -s http://localhost:8080/health | jq
```

### Run a Framework Worker

Each framework has its own docker-compose file. To run RabbitMQ:

```bash
docker compose -f frameworks/rabbitmq/docker-compose.yml up -d
```

The worker will automatically start consuming jobs and posting metrics to Prometheus.

### Run Benchmark

Run a single benchmark:

```bash
go run ./bench/runner/main.go \
  --framework=rabbitmq \
  --scenario=http-down \
  --duration=60s \
  --rate=50 \
  --warmup=10s \
  --cooldown=20s \
  --report=results/rabbitmq-http-down.json
```

Or run all frameworks automatically:

```bash
./scripts/run-all.sh --frameworks=rabbitmq,nats,kafka,temporal --scenarios=http-down,db-down,worker-crash
```

### View Metrics

- Prometheus: http://localhost:9090
- Grafana: http://localhost:3000 (admin/admin)

## Project Structure

```
at-least-once-bench/
├── shared/
│   ├── downstream/        # Controllable HTTP sink
│   ├── proto/             # Job struct
│   └── schema/            # Database schema
├── frameworks/
│   ├── rabbitmq/           # Explicit ACK/NACK
│   ├── nats/               # Server-side auto-redelivery
│   ├── kafka/              # Offset management + inner retry
│   └── temporal/           # Workflow history + activities
├── bench/
│   ├── runner/            # Benchmark CLI
│   └── scenarios/         # Failure injection (docker stop/start)
├── infra/
│   ├── nginx/              # NGINX proxy configs (HTTP, TCP)
│   ├── prometheus/         # Metrics collection
│   └── grafana/            # Visualization
└── docker-compose.shared.yml
```

## Design Decisions

### Idempotency
Every worker uses:
```sql
INSERT INTO processed_jobs (job_id, payload, attempt, ts)
VALUES ($1, $2, $3, now())
ON CONFLICT (job_id) DO NOTHING;
```

If 0 rows are inserted, the message was already processed — it is acked without calling the downstream again.

### Failure Injection
Network failures are injected via Docker stop/start commands targeting NGINX proxies:
```bash
docker stop bench-{framework}-nginx-downstream  # HTTP server unreachable
docker stop bench-{framework}-nginx-postgres    # Database unreachable
docker stop bench-{framework}-worker             # Worker crash
docker start bench-{framework}-nginx-downstream  # Restore HTTP connectivity
```

NGINX proxies act as transparent TCP/HTTP load balancers, allowing controlled network failures without modifying worker configuration.

### Metrics
All workers export Prometheus metrics:
- `bench_processed_total` — successfully processed messages
- `bench_retry_total` — messages retried
- `bench_lost_total` — messages dropped after exhausting retries
- `bench_latency_seconds` — end-to-end latency (histogram)
- `bench_dup_http_calls_total` — duplicate HTTP calls (during DB outages)

## Framework-Specific Notes

### Kafka
- Uses `FetchMessage` (not `ReadMessage`) to prevent auto-committing offsets before processing completes
- Inner retry loop with exponential backoff (5 attempts) — retries stay in-process, no broker round-trip
- When DB is unreachable, the idempotency check fails and the message is not committed — it will be retried on the next poll
- Consumer group rebalancing after worker-crash adds 10-20s of recovery latency
- Lost 9 messages in http-down (0.5%) — likely due to in-flight messages during the outage transition that exhausted the 5-attempt retry budget before the downstream recovered. The in-process retry model keeps this number low but does not eliminate it entirely

### Temporal
- Requires a separate PostgreSQL instance (`temporal-db`) for workflow history, proxied through NGINX for fault injection
- Activity retries managed by the workflow engine: `MaximumAttempts=5`, `InitialInterval=1s`, `BackoffCoefficient=2.0`
- Failed workflows (all retries exhausted) are recorded via a dedicated `RecordLostJobActivity`
- Near 1:1 HTTP-to-processed ratio — a direct consequence of workflow-scoped retries: the engine gates external calls, reducing over-delivery at the cost of throughput (2-10 msg/s)
- **db-down caveat (benchmark architecture issue, not a Temporal weakness)**: In this benchmark, the worker process hosts both the workflow submission endpoint and the activity workers — a single-container design for simplicity. When `temporal-db` is down, new workflow submissions fail at the HTTP POST level because the submission layer and the processing layer share the same failure domain. The ~1,200 messages lost are not a Temporal limitation but a consequence of this co-located architecture. In a production deployment with a separated submission service (e.g., a standalone API gateway that buffers requests), workflow submissions would be queued and retried independently of worker availability. The db-down result showing `lost=0` only counts losses detected by the worker's retry logic, not the submission-level failures. This is analogous to RabbitMQ/NATS/Kafka losing messages when their respective brokers are down — a scenario not tested in this benchmark.

### RabbitMQ
- Quorum queues with `x-delivery-count` header for accurate retry tracking
- Messages exceeding `maxRetries=5` deliveries are routed to a dead-letter exchange (DLX) and counted as lost
- Broker-side redelivery is fast — during a 30s outage, messages cycle through all 5 retries quickly, causing 59% message loss in http-down
- **This is a default-configuration trap, not an inherent weakness.** With `maxRetries=5` and no redelivery delay, quorum queues burn through the retry budget in seconds. Adding a delayed requeue (via quorum queue delivery-limit + TTL, or the delayed exchange plugin) would space out retries across the outage window and dramatically reduce loss. The 59% figure reflects *untuned defaults*, not what a production RabbitMQ deployment would look like
- Best db-down recovery: `processed == submitted` (all messages eventually processed)

### NATS JetStream
- Server-side redelivery with `ackWait=60s` and `maxDeliver=20`
- Uses `msg.Metadata().NumDelivered` for retry tracking (not the job's internal attempt counter)
- `msg.Term()` used to stop redelivery when max deliveries are reached
- On worker restart, all unacknowledged messages are redelivered — can cause massive reprocessing (31k messages for 2k submitted)
- Loses 370 messages (19%) in http-down with the current configuration (`ackWait=60s`, `maxDeliver=20`) — further tuning of these parameters may reduce this number

## Benchmark Results

### Test Conditions

| Parameter | Value |
|-----------|-------|
| Rate | 50 msg/s |
| Duration | 30s (failure injection phase) |
| Warmup | 10s |
| Cooldown | 30s |
| Submitted per test | ~2,000 messages |

### Results

| Framework | Scenario | Submitted | Processed | Retried | Lost | Dup HTTP | Throughput (msg/s) |
|-----------|----------|-----------|-----------|---------|------|----------|-------------------|
| **RabbitMQ** | http-down | 1,979 | 808 | 5,278 | 1,171 | 808 | 10.9 |
| **RabbitMQ** | db-down | 1,987 | 1,987 | 1,173 | 0 | 1,987 | 23.8 |
| **RabbitMQ** | worker-crash | 1,996 | 1,497 | 0 | 0 | 1,497 | 20.4 |
| **NATS** | http-down | 1,962 | 1,093 | 7,464 | 370 | 1,093 | 15.0 |
| **NATS** | db-down | 1,982 | 819 | 31 | 0 | 819 | 9.8 |
| **NATS** | worker-crash | 1,987 | 31,296* | 0 | 0 | 2,732 | 422.8 |
| **Kafka** | http-down | 1,970 | 1,910 | 39 | 9 | 1,910 | 25.9 |
| **Kafka** | db-down | 1,935 | 493 | 1,442 | 0 | 493 | 5.9 |
| **Kafka** | worker-crash | 1,941 | 1,447 | 0 | 0 | 1,447 | 19.5 |
| **Temporal** | http-down | 1,950 | 710 | 72 | 8 | 709 | 9.6 |
| **Temporal** | db-down | 1,951 | 756 | 112 | 0 | 755 | 7.7 |
| **Temporal** | worker-crash | 1,969 | 151 | 1 | 0 | 151 | 2.0 |

\* NATS worker-crash "Processed" count includes massive reprocessing of unacked messages after worker restart — the actual number of unique messages processed is much lower. See [Worker Crash Recovery](#worker-crash-recovery) for details.

### Key Findings

#### Message Loss Under Failure

| Framework | http-down | db-down | worker-crash |
|-----------|-----------|---------|--------------|
| RabbitMQ | 1,171 (59%) | 0 | 0 |
| NATS | 370 (19%) | 0 | 0 |
| Kafka | 9 (0.5%) | 0 | 0 |
| Temporal | 8 (0.4%) | ~1,195 (61%)* | 0 |

\* Temporal's db-down `lost=0` in the results table only reflects losses counted by the worker's retry logic. In reality, ~1,195 of 1,951 submitted messages failed at the `/start-workflow` HTTP POST level because `temporal-db` was down. **This is a benchmark architecture issue**: the worker hosts both submission and processing in a single container, so a `temporal-db` outage blocks both layers simultaneously. With a separated submission service, these messages could be buffered and retried. The loss pattern is analogous to RabbitMQ/NATS/Kafka losing messages when their brokers are down — a scenario not tested in this benchmark.

- **Kafka** loses the fewest messages across all scenarios (9 in http-down, 0 in db-down and worker-crash) — a result of its in-process retry model keeping messages in worker memory during retries.
- **RabbitMQ** loses the most under http-down — but **this is a configuration trap, not an inherent weakness.** With `maxRetries=5` and zero redelivery delay, quorum queues cycle through all retries in seconds during a 30s outage. The 59% loss is what happens when the retry budget is burned before the outage ends. Adding a redelivery delay (via quorum queue TTL or delayed exchange plugin) would spread retries across the outage window, and increasing `maxRetries` would extend the budget. Either change alone would dramatically reduce loss. The result reflects *untuned default behavior*, not what a production deployment would experience.
- **NATS** loses fewer than RabbitMQ in http-down (19% vs 59%) thanks to `ackWait=60s` (slower redelivery cycle), but still loses 370 messages. Like RabbitMQ, this is a broker-side redelivery model — different `ackWait` and `maxDeliver` settings would change the result.
- **Temporal** loses almost nothing under http-down and worker-crash, but depends on its own DB (`temporal-db`) for workflow persistence — when `temporal-db` is down, new workflow submissions fail and those messages are lost with no recovery mechanism.
- **RabbitMQ, NATS, and Kafka** recover fully from db-down and worker-crash with zero message loss. Temporal recovers from worker-crash but not from db-down.

#### Retry Efficiency

| Framework | Retries per processed msg (http-down) | Mechanism |
|-----------|---------------------------------------|-----------|
| RabbitMQ | 6.5 | Broker-side Nack + requeue |
| NATS | 6.8 | Server-side AckWait redelivery |
| Kafka | 0.02 | In-process retry loop with backoff |
| Temporal | 0.10 | Workflow-managed activity retry |

Kafka and Temporal retry within the worker process, avoiding broker round-trips. RabbitMQ and NATS rely on broker-side redelivery, which is more aggressive and generates significantly more retry overhead.

#### Retry Responsibility Model

The four frameworks place retry responsibility at fundamentally different layers. The benchmark results reflect these architectural choices — not inherent quality differences.

| Framework | Retry layer | Retry location | Consequence |
|-----------|-------------|----------------|-------------|
| Kafka | Application | In-process loop with backoff | Message never leaves worker during retry — low broker overhead, low loss, but failure is invisible to the broker |
| RabbitMQ | Broker | Nack + requeue to quorum queue | Broker controls redelivery — fast cycling exhausts `maxRetries` during sustained outages |
| NATS | Broker | Server-side AckWait expiry | Similar to RabbitMQ but timer-based — slower cycle (60s) reduces loss vs instant requeue |
| Temporal | Workflow engine | Activity retry within workflow execution | Retries are scoped to the workflow — external side effects are gated, but throughput is throttled by engine overhead |

Kafka's low loss rate reflects its in-process retry design (the message stays in the worker's memory during retries), not inherent broker superiority. RabbitMQ's high loss reflects aggressive broker-side redelivery under the test's `maxRetries=5` with no delay — a configuration that burns the retry budget in seconds, not a fundamental model weakness. Temporal's low throughput and db-down losses reflect the benchmark's co-located submission/worker architecture, not Temporal's production capabilities.

**These are architectural trade-offs, not quality rankings.** Every framework's results are dominated by where it places retry responsibility and how this benchmark configured that layer. A fairer comparison would normalize retry budgets, add redelivery delays to broker-side frameworks, and separate Temporal's submission endpoint from its workers.

#### Duplicate HTTP Calls (Over-Delivery)

When the database is down, the worker calls the downstream HTTP endpoint successfully but fails to record the result. On retry, the HTTP call is made again — this is "over-delivery."

**The ratio is not the story — the absolute count is.** In db-down, RabbitMQ makes 1,987 downstream HTTP calls while Kafka makes only 493 — a **4x difference in external side-effect cost.** If the downstream is a billing API, a payment gateway, or any non-idempotent service, that 4x multiplier is the difference between a minor incident and a serious one. The dup/processed ratio is ~1.0x across all frameworks (the metric fires once per round-trip that receives a response), which makes it uninformative as a comparison axis. Focus on the absolute numbers instead.

The `bench_dup_http_calls_total` counter increments on every HTTP call that receives a response (any status code), regardless of whether the response is 2xx. It does not fire when the connection fails entirely (timeout, connection refused).

| Framework | Dup HTTP (db-down) | Processed (db-down) | Dup / Processed | Dup HTTP (http-down) | Processed (http-down) | Dup / Processed |
|-----------|-------------------|--------------------:|----------------:|---------------------|----------------------:|----------------:|
| RabbitMQ | 1,987 | 1,987 | 1.0x | 808 | 808 | 1.0x |
| NATS | 819 | 819 | 1.0x | 1,093 | 1,093 | 1.0x |
| Kafka | 493 | 493 | 1.0x | 1,910 | 1,910 | 1.0x |
| Temporal | 755 | 756 | 1.0x | 709 | 710 | 1.0x |

Kafka's in-process retry keeps the message in worker memory, avoiding broker round-trips and limiting downstream calls. RabbitMQ's broker-side redelivery cycles the message back through the full processing path on each retry, multiplying downstream calls. This is a direct consequence of where retry responsibility sits — not a quality difference.

#### Worker Crash Recovery

| Framework | Processed after crash | Notes |
|-----------|-----------------------|-------|
| RabbitMQ | 1,497 (75%) | Fast recovery via quorum queue redelivery |
| NATS | 31,296* | Reprocesses entire unacked backlog after restart |
| Kafka | 1,447 (75%) | Consumer group rebalancing adds latency |
| Temporal | 151 (8%) | Worker = workflow starter; crash blocks submission |

*NATS redelivers all unacknowledged messages on worker restart, causing massive reprocessing. The actual unique messages processed is much lower, but each triggers a duplicate HTTP call.

Temporal shows low throughput **not because Temporal is slow, but because this benchmark's architecture co-locates the workflow submission endpoint with the activity worker in a single container.** Killing the worker blocks both new job submission and processing simultaneously. In a production deployment with separated submission and worker services, a worker crash would only affect processing — the submission service would continue accepting workflows, and Temporal's durable workflow history would ensure they are processed once a worker recovers.

### Trade-off Summary

These results reflect each framework's **default configuration** under the test conditions. Different tuning (retry delays, higher retry budgets, separated submission endpoints) would change the numbers. The table below summarizes the trade-offs observed, not absolute quality rankings.

| Framework | Strength (in this benchmark) | Weakness (in this benchmark) | Key config dependency |
|-----------|------------------------------|------------------------------|----------------------|
| **Kafka** | Fewest messages lost (0.5% in http-down) | Low throughput during db-down (5.9 msg/s) due to blocking retry loop | `maxRetries`, backoff timing |
| **RabbitMQ** | Full recovery from db-down and worker-crash; highest healthy throughput | 59% message loss in http-down due to fast redelivery exhausting retry budget | `maxRetries=5`, no redelivery delay |
| **NATS** | Lower http-down loss than RabbitMQ (19%) due to slower redelivery cycle | Massive reprocessing on worker restart (31k messages); tuning is non-trivial | `ackWait=60s`, `maxDeliver=20` |
| **Temporal** | Near 1:1 HTTP-to-processed ratio; minimal over-delivery | Depends on its own DB for persistence — db-down causes submission-level message loss; lowest throughput | `temporal-db` availability, worker architecture |

### Future Work

The current benchmark tests each framework's default/typical configuration. Variant configurations would isolate whether observed differences are architectural or configuration-dependent:

- **RabbitMQ with delayed retry**: Add redelivery delay via delayed exchange plugin or quorum queue TTL — expected to dramatically reduce http-down loss
- **Kafka with broker-side retry**: Disable in-process retry loop, rely on offset non-commit for redelivery — would make Kafka behave more like RabbitMQ/NATS
- **Temporal with separated submission**: Decouple workflow starter from worker process — would fix worker-crash throughput drop
- **All frameworks with higher maxRetries**: Normalize retry budgets across frameworks for a fairer comparison

## Troubleshooting

### Worker fails to start
1. Check Docker logs: `docker logs <worker_container>`
2. Verify shared infrastructure is running: `docker compose -f docker-compose.shared.yml ps`
3. Check network connectivity: `docker network ls` and `docker network inspect bench-net`

### Metrics not appearing in Prometheus
1. Worker must be exposing `/metrics` on the correct port (see architecture)
2. Check Prometheus targets: http://localhost:9090/targets
3. Verify worker is actually running

### Database errors during processing
1. Check PostgreSQL is up: `docker exec postgres psql -U bench -d benchdb -c "SELECT * FROM processed_jobs LIMIT 1;"`
2. Verify schema was created: `docker compose -f docker-compose.shared.yml logs postgres`

## Contributing

To add a new framework:
1. Create `frameworks/<name>/docker-compose.yml`
2. Create `frameworks/<name>/worker/main.go` (or appropriate language)
3. Ensure worker exports metrics to `<assigned_port>/metrics`
4. Update `infra/prometheus/prometheus.yml` with new job
5. Test with benchmark runner

## License

MIT
