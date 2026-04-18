# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Project Is

A benchmark comparing at-least-once delivery guarantees across RabbitMQ, NATS JetStream, Kafka (KRaft), and Temporal. The comparison axis is **where retry responsibility lives** (in-process, broker, or workflow engine), not middleware quality.

## Build & Run

Go 1.22+, Docker & Docker Compose required. No Makefile — everything runs via `go run` and Docker.

```bash
# Shared infra (Postgres, downstream sink, Prometheus, Grafana)
docker compose -f docker-compose.shared.yml up -d

# Single framework
docker compose -f frameworks/rabbitmq/docker-compose.yml up -d

# Single benchmark
go run ./bench/runner/main.go \
  --framework=rabbitmq --scenario=http-down \
  --duration=60s --rate=50 --warmup=10s --cooldown=30s \
  --report=results/rabbitmq-http-down.json

# All benchmarks
./scripts/run-all.sh --frameworks=rabbitmq,nats,kafka,temporal --scenarios=http-down,db-down,worker-crash

# Start/stop all
./scripts/start-all.sh
./scripts/stop-all.sh
```

No unit tests — validation is metrics-based via Prometheus.

## Architecture

```
bench/runner (Go) → publishes jobs at fixed rate
    ↓
Framework broker/engine
    ↓
frameworks/{name}/worker (Go) → processes jobs
    ├─→ HTTP POST → NGINX proxy → downstream:8080/sink
    ├─→ DB write  → NGINX proxy → PostgreSQL:5432
    └─→ Broker    → NGINX proxy → broker
```

Failure injection works by `docker stop`/`docker start` on NGINX proxy containers — no code changes needed. Scenarios: `http-down`, `db-down`, `worker-crash`.

## Module Structure

Module: `github.com/reivosar/at-least-once-bench`

- `shared/proto/job.go` — `Job` struct (ID, Payload, Attempt, SubmittedAt) used by all frameworks
- `shared/schema/init.sql` — `processed_jobs` table with `UNIQUE(job_id)` for idempotency
- `shared/downstream/` — HTTP sink with `/admin/fail` and `/admin/recover` endpoints
- `bench/runner/` — CLI that publishes jobs, injects failures, scrapes Prometheus
- `bench/scenarios/` — `Scenario` interface (`Inject`/`Recover`) implemented via Docker commands
- `frameworks/{rabbitmq,nats,kafka,temporal}/worker/` — one worker per framework

## Key Patterns

**Idempotency**: Every worker uses `INSERT ... ON CONFLICT (job_id) DO NOTHING`. If 0 rows inserted, the job was already processed — ACK without calling downstream again.

**Prometheus metrics** (all workers export the same set):
- `bench_processed_total`, `bench_retry_total`, `bench_lost_total`
- `bench_dup_http_calls_total` — downstream HTTP calls including retries
- `bench_latency_seconds` (histogram), `bench_inflight` (gauge)

**Worker concurrency**: All workers accept `--concurrency` flag (default 4). Each runs N goroutines consuming from the broker.

**Retry models differ by framework**:
- Kafka: in-process loop, 5 retries, exponential backoff (100ms base)
- RabbitMQ: broker Nack + requeue, maxRetries=5, no delay
- NATS: server AckWait=60s, maxDeliver=20
- Temporal: workflow RetryPolicy, MaximumAttempts=5, InitialInterval=1s, BackoffCoefficient=2.0

## Docker Composition

Each framework has its own `docker-compose.yml` that adds:
- The broker (RabbitMQ/NATS/Kafka/Temporal server)
- The worker container (multi-stage Go build)
- 3 NGINX proxies: `nginx-downstream`, `nginx-postgres`, `nginx-broker`

All attach to the shared `bench-net` network. Metrics ports: RabbitMQ worker 9091, NATS 9093, Kafka 9095, Temporal 9097.

## Adding a New Framework

1. Create `frameworks/<name>/docker-compose.yml` with broker + worker + 3 NGINX proxies
2. Create `frameworks/<name>/worker/main.go` following existing worker pattern (same Prometheus metrics, same idempotency SQL, same `--concurrency` flag)
3. Add a publisher in `bench/runner/main.go`
4. Add scrape target in `infra/prometheus/prometheus.yml`

## Adding a New Scenario

Implement the `Scenario` interface in `bench/scenarios/`:
```go
type Scenario interface {
    Name() string
    Inject(ctx context.Context) error
    Recover(ctx context.Context) error
}
```

Register it in `bench/runner/main.go`'s scenario selection logic.
