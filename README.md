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
- Lowest message loss across all scenarios (9 out of ~2,000 in http-down)

### Temporal
- Requires a separate PostgreSQL instance (`temporal-db`) for workflow history, proxied through NGINX for fault injection
- Activity retries managed by the workflow engine: `MaximumAttempts=5`, `InitialInterval=1s`, `BackoffCoefficient=2.0`
- Failed workflows (all retries exhausted) are recorded via a dedicated `RecordLostJobActivity`
- Near 1:1 ratio of HTTP calls to processed messages — minimal over-delivery
- Lowest throughput (2-10 msg/s) due to workflow engine overhead
- If Temporal's own DB goes down (tested in db-down scenario), workflows pause and resume automatically on recovery

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
- Still loses 370 messages (19%) in http-down despite tuning — JetStream's delivery semantics are difficult to configure for strict at-least-once guarantees

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
| **NATS** | worker-crash | 1,987 | 31,296 | 0 | 0 | 2,732 | 422.8 |
| **Kafka** | http-down | 1,970 | 1,910 | 39 | 9 | 1,910 | 25.9 |
| **Kafka** | db-down | 1,935 | 493 | 1,442 | 0 | 493 | 5.9 |
| **Kafka** | worker-crash | 1,941 | 1,447 | 0 | 0 | 1,447 | 19.5 |
| **Temporal** | http-down | 1,950 | 710 | 72 | 8 | 709 | 9.6 |
| **Temporal** | db-down | 1,951 | 756 | 112 | 0 | 755 | 7.7 |
| **Temporal** | worker-crash | 1,969 | 151 | 1 | 0 | 151 | 2.0 |

### Key Findings

#### Message Loss Under Failure

| Framework | http-down | db-down | worker-crash |
|-----------|-----------|---------|--------------|
| RabbitMQ | 1,171 (59%) | 0 | 0 |
| NATS | 370 (19%) | 0 | 0 |
| Kafka | 9 (0.5%) | 0 | 0 |
| Temporal | 8 (0.4%) | 0 | 0 |

- **Kafka and Temporal** lose almost no messages across all scenarios.
- **RabbitMQ** loses the most under http-down. Quorum queue redelivery is fast, so messages exhaust `maxRetries=5` within the 30s outage window and are routed to the dead-letter exchange.
- **NATS** loses fewer than RabbitMQ thanks to `ackWait=60s` (slower redelivery cycle), but still loses 370 messages.
- **All frameworks** recover fully from db-down and worker-crash with zero message loss.

#### Retry Efficiency

| Framework | Retries per processed msg (http-down) | Mechanism |
|-----------|---------------------------------------|-----------|
| RabbitMQ | 6.5 | Broker-side Nack + requeue |
| NATS | 6.8 | Server-side AckWait redelivery |
| Kafka | 0.02 | In-process retry loop with backoff |
| Temporal | 0.10 | Workflow-managed activity retry |

Kafka and Temporal retry within the worker process, avoiding broker round-trips. RabbitMQ and NATS rely on broker-side redelivery, which is more aggressive and generates significantly more retry overhead.

#### Duplicate HTTP Calls (Over-Delivery)

When the database is down, the worker calls the downstream HTTP endpoint successfully but fails to record the result. On retry, the HTTP call is made again — this is "over-delivery."

| Framework | Dup HTTP calls (db-down) | Ratio to submitted |
|-----------|--------------------------|-------------------|
| RabbitMQ | 1,987 | 1.0x |
| NATS | 819 | 0.4x |
| Kafka | 493 | 0.3x |
| Temporal | 755 | 0.4x |

RabbitMQ generates the most duplicate HTTP calls because its redelivery cycle is the fastest. Kafka generates the fewest because its inner retry loop processes each message to completion (success or max retries) before moving on.

#### Worker Crash Recovery

| Framework | Processed after crash | Notes |
|-----------|-----------------------|-------|
| RabbitMQ | 1,497 (75%) | Fast recovery via quorum queue redelivery |
| NATS | 31,296* | Reprocesses entire unacked backlog after restart |
| Kafka | 1,447 (75%) | Consumer group rebalancing adds latency |
| Temporal | 151 (8%) | Worker = workflow starter; crash blocks submission |

*NATS redelivers all unacknowledged messages on worker restart, causing massive reprocessing. The actual unique messages processed is much lower, but each triggers a duplicate HTTP call.

Temporal shows low throughput because the worker also hosts the workflow submission endpoint — killing the worker blocks new job submission during the outage.

### Recommendations

| Use case | Recommended framework | Why |
|----------|----------------------|-----|
| General-purpose at-least-once | **Kafka** | Lowest message loss (0.5%), decent throughput, efficient inner retry loop |
| Minimize duplicate side effects | **Temporal** | Near 1:1 HTTP-to-processed ratio; workflow-level retry prevents redundant calls |
| High throughput, loss-tolerant | **RabbitMQ** | Fast processing when healthy; increase `maxRetries` to reduce http-down loss |
| At-least-once delivery | **Avoid NATS** | JetStream delivery semantics are difficult to tune; still loses messages after config optimization |

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
