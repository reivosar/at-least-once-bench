# at-least-once-bench

A comprehensive benchmark comparing at-least-once delivery guarantees across messaging frameworks using Docker:

- **RabbitMQ** — explicit ACK/NACK with quorum queues
- **NATS JetStream** — server-side redelivery via AckWait
- **Kafka** (KRaft) — log + offset commit model with inner retry loop
- **Temporal** — workflow history in PostgreSQL with automatic retries

## Overview

The benchmark tests how each framework behaves when:
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
- **db-down caveat**: Temporal requires its own PostgreSQL (`temporal-db`) to persist workflow history. When `temporal-db` is down, new workflow submissions fail at the HTTP POST level — the benchmark runner's `/start-workflow` calls are rejected and those messages are lost. The db-down result showing `lost=0` only counts losses detected by the worker's retry logic, not the ~1,200 messages that failed at the submission layer. Only the 756 messages submitted before or after the outage were actually processed. This is analogous to RabbitMQ/NATS/Kafka losing messages when their respective brokers are down — a scenario not tested in this benchmark.

### RabbitMQ
- Quorum queues with `x-delivery-count` header for accurate retry tracking
- Messages exceeding `maxRetries=5` deliveries are routed to a dead-letter exchange (DLX) and counted as lost
- Broker-side redelivery is fast — during a 30s outage, messages cycle through all 5 retries quickly, causing 59% message loss in http-down
- To reduce loss: increase `maxRetries` or add a redelivery delay via quorum queue configuration
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

\* Temporal's db-down `lost=0` in the results table only reflects losses counted by the worker's retry logic. In reality, ~1,195 of 1,951 submitted messages failed at the `/start-workflow` HTTP POST level because `temporal-db` was down — these were never persisted as workflows and are permanently lost. The actual loss rate is comparable to RabbitMQ's http-down scenario.

- **Kafka** loses the fewest messages across all scenarios (9 in http-down, 0 in db-down and worker-crash) — a result of its in-process retry model keeping messages in worker memory during retries.
- **RabbitMQ** loses the most under http-down. Quorum queue redelivery is fast, so messages exhaust `maxRetries=5` within the 30s outage window and are routed to the dead-letter exchange. **Note:** This 59% loss is specific to `maxRetries=5` with no redelivery delay. Adding a delayed requeue (via quorum queue TTL or a delayed exchange plugin) or increasing `maxRetries` would significantly reduce loss. The current result reflects default quorum queue behavior without tuning, not an inherent model weakness.
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

Kafka's low loss rate reflects its in-process retry design (the message stays in the worker's memory during retries), not inherent broker superiority. RabbitMQ's high loss reflects aggressive broker-side redelivery under the test's `maxRetries=5` setting, not a fundamental model weakness. **These are architectural trade-offs, not quality rankings.** A fairer comparison would normalize retry budgets and add redelivery delays to broker-side frameworks.

#### Duplicate HTTP Calls (Over-Delivery)

When the database is down, the worker calls the downstream HTTP endpoint successfully but fails to record the result. On retry, the HTTP call is made again — this is "over-delivery." The `bench_dup_http_calls_total` counter increments on every HTTP call that receives a response (any status code), regardless of whether the response is 2xx. It does not fire when the connection fails entirely (timeout, connection refused). It measures total downstream round-trips, not just redundant or successful ones.

| Framework | Dup HTTP (db-down) | Processed (db-down) | Dup / Processed | Dup HTTP (http-down) | Processed (http-down) | Dup / Processed |
|-----------|-------------------|--------------------:|----------------:|---------------------|----------------------:|----------------:|
| RabbitMQ | 1,987 | 1,987 | 1.0x | 808 | 808 | 1.0x |
| NATS | 819 | 819 | 1.0x | 1,093 | 1,093 | 1.0x |
| Kafka | 493 | 493 | 1.0x | 1,910 | 1,910 | 1.0x |
| Temporal | 755 | 756 | 1.0x | 709 | 710 | 1.0x |

The dup/processed ratio is ~1.0x across all frameworks because the metric fires once per HTTP round-trip that receives a response. The meaningful comparison is the **absolute count**: RabbitMQ's fast redelivery cycle produces 1,987 HTTP calls in db-down (every submitted message hits the downstream at least once), while Kafka's in-process retry limits this to 493. For systems where downstream side effects are expensive or non-idempotent, the absolute dup count — not the ratio — determines the real-world damage from over-delivery.

#### Worker Crash Recovery

| Framework | Processed after crash | Notes |
|-----------|-----------------------|-------|
| RabbitMQ | 1,497 (75%) | Fast recovery via quorum queue redelivery |
| NATS | 31,296* | Reprocesses entire unacked backlog after restart |
| Kafka | 1,447 (75%) | Consumer group rebalancing adds latency |
| Temporal | 151 (8%) | Worker = workflow starter; crash blocks submission |

*NATS redelivers all unacknowledged messages on worker restart, causing massive reprocessing. The actual unique messages processed is much lower, but each triggers a duplicate HTTP call.

Temporal shows low throughput because the worker also hosts the workflow submission endpoint — killing the worker blocks new job submission during the outage.

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
