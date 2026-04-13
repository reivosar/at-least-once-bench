# Development Guide

## Current Status

The foundation of the at-least-once-bench project has been implemented:

### ✅ Completed

1. **Shared Infrastructure**
   - `shared/downstream/main.go` — controllable HTTP sink with `/sink`, `/admin/fail`, `/admin/recover`
   - `shared/proto/job.go` — Job struct for cross-framework consistency
   - `shared/schema/init.sql` — PostgreSQL schema with idempotency table
   - `docker-compose.shared.yml` — postgres, downstream, prometheus, grafana
   - `infra/prometheus/prometheus.yml` — metric scraping config
   - `infra/grafana/provisioning/` — Grafana datasources and dashboard

2. **RabbitMQ Framework**
   - `frameworks/rabbitmq/docker-compose.yml` — RabbitMQ setup
   - `frameworks/rabbitmq/worker/main.go` — full worker implementation with:
     - Quorum queue declaration
     - Explicit ACK/NACK semantics
     - Dead-letter exchange for poison messages
     - Idempotent database writes
     - Prometheus metrics export
   - `frameworks/rabbitmq/worker/Dockerfile` — containerized worker
   - `frameworks/rabbitmq/README.md` — detailed architecture and design docs

3. **Benchmark Runner**
   - `bench/runner/main.go` — CLI for orchestrating benchmarks
   - `bench/runner/metrics.go` — Prometheus scraping and metric extraction
   - Supports: `--framework`, `--scenario`, `--duration`, `--rate`, `--warmup`, `--cooldown`, `--report`
   - Failure injection for http-down scenario
   - JSON + Markdown report generation

### 🚧 Next Steps (Implementation Order)

#### Phase 2: Additional Frameworks (in priority order)

1. **NATS JetStream** (highest priority for contrast)
   - Server-side auto-redelivery (vs RabbitMQ's explicit NACK)
   - Lightweight footprint
   - `frameworks/nats/` — docker-compose, worker, tests

2. **Kafka (KRaft mode)**
   - Consumer group offset management
   - Inner retry loop (no server-side retry)
   - `frameworks/kafka/` — docker-compose, worker, tests

3. **Temporal** (most complex)
   - Workflow history + activity pattern
   - Separate PostgreSQL for Temporal metadata
   - `frameworks/temporal/` — docker-compose, worker, tests

4. **Celery + Redis**
   - Python worker (breaking from all-Go)
   - `acks_late` semantics
   - Redis AOF persistence tuning
   - `frameworks/celery/` — docker-compose, worker, tests

#### Phase 3: Testing & Scenarios

1. **Additional Failure Scenarios**
   - Implement `db-down` scenario (network disconnect on postgres)
   - Implement `worker-crash` scenario (docker kill + restart)
   - `bench/scenarios/` — scenario implementations

2. **Load Generator Enhancements**
   - Parallel submission (goroutines vs sequential)
   - Payload size variations
   - Long-running job support

3. **Comprehensive Test Coverage**
   - Unit tests for each worker
   - Integration tests with failure injection
   - End-to-end benchmark validation

#### Phase 4: Automation & Reporting

1. **Batch Benchmark Script**
   - `scripts/run-all.sh` — orchestrate all frameworks × scenarios
   - Parameterized execution (different rates, durations)
   - Aggregate results

2. **Advanced Metrics**
   - Prometheus histogram quantile extraction
   - Comparison tables (framework × scenario × metric)
   - Statistical significance testing

3. **Documentation**
   - Per-framework design decision rationale
   - Performance characteristics and tradeoffs
   - Operational guidelines

## Quick Development Commands

### Build Everything
```bash
go mod tidy
cd shared/downstream && go build
cd ../../frameworks/rabbitmq/worker && go build
cd ../../bench/runner && go build
```

### Test Shared Infrastructure
```bash
docker compose -f docker-compose.shared.yml up -d
curl -s http://localhost:8080/health | jq
```

### Test RabbitMQ Worker
```bash
docker compose -f frameworks/rabbitmq/docker-compose.yml up -d
# Wait for worker to connect
docker logs rabbitmq-worker
curl http://localhost:9094/metrics | grep bench_
```

### Run a Quick Benchmark
```bash
go run ./bench/runner/main.go \
  --framework=rabbitmq \
  --scenario=http-down \
  --duration=30s \
  --rate=50 \
  --report=results/test.json
cat results/test.md
```

### View Metrics
- Prometheus: http://localhost:9090
- Grafana: http://localhost:3000
- RabbitMQ Management: http://localhost:15672 (bench/benchpass)

## Code Structure Patterns

### Framework Worker Template
Each worker should:
1. Parse environment variables for broker/DB connection
2. Implement graceful startup with retries (30s timeout)
3. Export Prometheus metrics on a unique port
4. Use idempotent DB write: `INSERT ... ON CONFLICT DO NOTHING`
5. Implement job struct unmarshaling from the framework's native format
6. Calculate and export latency metrics (timestamp in job payload)

### Metric Export Pattern
```go
benchProcessedTotal.WithLabelValues().Inc()       // On successful processing
benchRetryTotal.WithLabelValues().Inc()           // On each retry
benchLostTotal.WithLabelValues().Inc()            // On max retries exceeded
benchDupHTTPCallsTotal.WithLabelValues().Inc()    // Each downstream HTTP call
benchLatencySeconds.WithLabelValues().Observe(latency)  // Latency histogram
benchInflight.WithLabelValues().Set(count)        // In-flight gauge
```

### Docker Compose Pattern
Each framework should:
1. Use `networks: { bench-net: { external: true } }`
2. Depend on: `postgres` (via health check), `downstream`
3. Set `DATABASE_URL`, `DOWNSTREAM_URL`, `METRICS_PORT` env vars
4. Expose metrics port (9091-9095 depending on framework)

## Testing Checklist

- [ ] RabbitMQ worker processes messages with correct latency
- [ ] RabbitMQ worker retries on downstream failure
- [ ] RabbitMQ worker sends to DLQ after max retries
- [ ] NATS worker implementation complete
- [ ] Kafka worker implementation complete
- [ ] Temporal worker implementation complete
- [ ] Celery worker implementation complete
- [ ] Benchmark runner collects accurate metrics from Prometheus
- [ ] Failure injection (http-down, db-down, worker-crash) works
- [ ] All workers handle idempotency correctly
- [ ] Comparison table generation working

## Key Files Reference

| File | Purpose |
|------|---------|
| `shared/downstream/main.go` | HTTP sink with failure injection |
| `shared/proto/job.go` | Job struct (framework-independent) |
| `shared/schema/init.sql` | Idempotency table definition |
| `frameworks/rabbitmq/worker/main.go` | RabbitMQ worker implementation |
| `bench/runner/main.go` | Benchmark CLI orchestrator |
| `bench/runner/metrics.go` | Prometheus querying |
| `go.mod` | Dependencies |
| `docker-compose.shared.yml` | Shared services (postgres, prometheus, grafana) |

## Performance Notes

### Expected Throughput (single worker, 4 concurrent consumers)
- RabbitMQ: 500-1000 msg/sec (depending on downstream latency)
- NATS: 2000+ msg/sec
- Kafka: 5000+ msg/sec
- Temporal: 50-100 msg/sec (workflow history I/O)
- Celery: 200-500 msg/sec (Python overhead)

### Latency Measurement
Latency is end-to-end: from job submission (timestamp in payload) to DB write completion. Includes:
- Broker queueing time
- Worker processing time
- Downstream HTTP call roundtrip
- Database write latency

## Debugging Tips

### RabbitMQ Queue Inspection
```bash
docker exec rabbitmq rabbitmq-diagnostics list_queues
```

### Check Processed Jobs
```bash
docker exec postgres psql -U bench -d benchdb -c "SELECT COUNT(*) FROM processed_jobs"
```

### View Worker Metrics Live
```bash
curl -s http://localhost:9094/metrics | grep bench_ | head -20
```

### Trace a Job Through the System
1. Submit a job: note the UUID
2. Check RabbitMQ: `docker logs rabbitmq | grep <uuid>`
3. Check database: `SELECT * FROM processed_jobs WHERE job_id = '<uuid>'`
4. Check metrics: `curl -s http://localhost:9094/metrics | grep bench_processed`
