# at-least-once-bench

A comprehensive benchmark comparing at-least-once delivery guarantees across five messaging frameworks using Docker:

- **Temporal** — workflow history in PostgreSQL, automatic retries
- **Kafka** (KRaft) — log + offset commit model
- **Celery + Redis** — ACK-late task queue with Redis broker
- **RabbitMQ** — explicit ACK/NACK with quorum queues
- **NATS JetStream** — server-side redelivery via AckWait

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
    ├─→ HTTP POST to downstream:8080/sink
    └─→ INSERT into processed_jobs (PostgreSQL)
    
Monitoring: Prometheus + Grafana
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

The benchmark runner is in `bench/runner/main.go`. Currently under construction.

```bash
go run ./bench/runner/main.go \
  --framework=rabbitmq \
  --scenario=db-down \
  --duration=120s \
  --rate=50 \
  --report=results/rabbitmq-db-down.json
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
│   ├── temporal/
│   ├── kafka/
│   ├── celery/
│   ├── rabbitmq/
│   └── nats/
├── bench/
│   ├── runner/            # Benchmark CLI
│   └── scenarios/         # Failure injection
├── infra/
│   ├── prometheus/
│   └── grafana/
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
Network failures are injected via Docker API:
```bash
docker network disconnect bench-net downstream  # HTTP server unreachable
docker network disconnect bench-net postgres    # Database unreachable
docker kill --signal=SIGKILL <worker>           # Worker crash
```

### Metrics
All workers export Prometheus metrics:
- `bench_processed_total` — successfully processed messages
- `bench_retry_total` — messages retried
- `bench_lost_total` — messages dropped after exhausting retries
- `bench_latency_seconds` — end-to-end latency (histogram)
- `bench_dup_http_calls_total` — duplicate HTTP calls (during DB outages)

## Framework-Specific Notes

### Temporal
- Requires separate PostgreSQL instance for workflow history
- Automatic retries via Activity timeout
- Idempotency key: `workflowId + activityId`
- Risk: if Temporal's DB goes down, workflows pause

### Kafka
- Consumer must manually commit offsets after processing
- No automatic retry — requires inner retry loop in worker
- Idempotency key: embedded UUID in message
- Risk: uncommitted offsets replay on consumer restart

### Celery + Redis
- **Critical**: Redis must have AOF with `appendfsync always`
- `acks_late=True` ensures task is not acked until after execution
- Visibility timeout can be up to 1 hour — tune based on max task duration
- Idempotency key: Celery task ID

### RabbitMQ
- Explicit ACK/NACK model
- Quorum queues recommended (Raft-based, survive broker restart)
- Dead-letter exchange needed for poison messages
- Idempotency key: embedded UUID in message

### NATS JetStream
- Server-side redelivery (no consumer restart needed)
- File-backed stream persistence
- `AckWait` controls redelivery timeout (default 30s, tune as needed)
- Idempotency key: `Nats-Msg-Id` header (with dedup window)

## Performance Expectations

Rough throughput estimates (100% at-least-once delivery):
- RabbitMQ: 1000+ msg/sec
- NATS: 5000+ msg/sec
- Kafka: 10000+ msg/sec
- Celery + Redis: 500+ msg/sec (Python overhead)
- Temporal: 100-300 msg/sec (workflow history disk I/O)

(These vary based on payload size, worker concurrency, and hardware)

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
