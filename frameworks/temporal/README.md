# Temporal Workflow Framework Integration

This directory contains the Temporal integration for the at-least-once-bench benchmark.

## Architecture

- **Workflow Orchestrator**: Temporal server (separate from benchmark app)
- **Metadata Storage**: Dedicated PostgreSQL for Temporal (separate from bench DB)
- **Execution Model**: Workflow + Activity separation with deterministic replay
- **Durability**: Workflow history log as source of truth
- **Retry Strategy**: Built-in RetryPolicy with automatic backoff

## How It Works

### At-Least-Once Guarantee via Workflow History

Temporal uses a unique approach: workflow history is the source of truth.

1. **Workflow is started** with job parameters
2. **Activity is called** within the workflow
3. **Workflow history is persisted** to Temporal's database
4. **On failure**: Temporal re-schedules the activity by **replaying the workflow history**
5. **Idempotency**: Activity must be idempotent (UUID in job prevents duplicates)

### Deterministic Replay

The core mechanism:
- Workflow code is re-executed on failure using the persisted history
- Activities are executed once per unique activity ID + attempt
- No manual offset/ack management needed

### Failure Scenarios

#### Downstream HTTP Server Down
- Activity attempts to call downstream, gets error
- Temporal's RetryPolicy triggers automatic retry after backoff
- Activity is re-executed (deterministically replayed)
- Result: **At-least-once delivery** ✓

#### Database Connection Lost
- HTTP call succeeds, DB write fails
- Activity fails, Temporal retries
- On retry, HTTP is called again (over-delivery)
- Database UNIQUE constraint prevents duplicate inserts
- Result: **At-least-once delivery** ✓ (with duplicate HTTP calls)

#### Worker Crashes
- Worker process dies
- Workflow remains in Temporal's database
- Next healthy worker picks up the workflow
- Replays from persisted history
- Result: **At-least-once delivery** ✓ (automatic recovery)

#### Temporal Server Crashes
- If Temporal's database goes down, workflows pause
- This is a critical difference from brokers (which drop messages if unavailable)
- Once Temporal recovers, workflows resume automatically

## Configuration

See `docker-compose.yml` for setup:
- `temporal` — Temporal server (auto-setup with postgres backend)
- `temporal-db` — PostgreSQL for Temporal metadata (separate from bench DB)
- `temporal-ui` — Temporal Web UI (port 8080)
- `temporal-worker` — This worker

Environment variables:
- `TEMPORAL_HOST` — Temporal server hostname
- `TEMPORAL_PORT` — Temporal server port (7233)
- `DATABASE_URL` — PostgreSQL for benchmark data (not Temporal metadata)
- `DOWNSTREAM_URL` — HTTP sink endpoint
- `METRICS_PORT` — Prometheus metrics port (9091)

Tunables in `worker/main.go`:
- `TaskQueueName` — task queue name (default: "bench-task-queue")
- `WorkflowName` — workflow name (default: "ProcessJobWorkflow")
- Activity `RetryPolicy`:
  - `InitialInterval: 1s`
  - `BackoffCoefficient: 2.0`
  - `MaximumInterval: 1m`
  - `MaximumAttempts: 5`
  - `HeartbeatTimeout: 10s`

## Temporal Concepts

### Workflow
- Orchestration logic (what should happen)
- Deterministic and replayable
- Stores state in history
- Can call multiple activities

### Activity
- Work that actually happens (HTTP call, DB write)
- Must be idempotent
- Can fail and be retried
- Runs on workers

### Workflow History
- Immutable log of all workflow decisions and activity results
- Persisted in PostgreSQL
- Used for deterministic replay on recovery
- The source of truth (not the database)

### RetryPolicy
- Automatic retry configuration
- Exponential backoff
- Max attempts limit
- Heartbeat tracking for long-running activities

## Key Design Decisions

### Workflow/Activity Separation
- **Why**: Clear separation of concerns (orchestration vs execution)
- **Tradeoff**: More complex than simple queue processing
- **Result**: Automatic recovery without manual offset/ack logic

### History-Based Durability
- **Why**: Deterministic replay ensures at-least-once without explicit ack
- **vs Brokers**: Don't need to track message acknowledgment
- **Risk**: If Temporal DB goes down, workflows are stuck (can't proceed or fail)

### Separate Temporal DB
- **Why**: Temporal needs its own PostgreSQL for metadata
- **vs Shared DB**: Cleaner separation, dedicated for Temporal
- **Operational**: One more database to manage

### Automatic Heartbeat
- **Why**: Temporal detects stuck activities
- **Timeout**: 10 seconds if no heartbeat
- **Result**: Stuck activities are restarted automatically

## Monitoring

Metrics exported to `:9091/metrics`:
- `bench_processed_total` — successfully completed workflows
- `bench_retry_total` — activities that were retried
- `bench_lost_total` — workflows that exceeded max retries
- `bench_latency_seconds` — histogram of end-to-end latency
- `bench_dup_http_calls_total` — duplicate HTTP calls (during retries)
- `bench_inflight` — current in-flight workflows

Additional monitoring via Temporal UI:
- Workflow executions and history
- Activity retries and failures
- Metrics and statistics

## Testing the Integration

### Start the server, database, and worker
```bash
docker compose up -d
```

### Check Temporal UI
```
http://localhost:8080
```

### Submit a workflow (via Temporal CLI or SDK)
```bash
# Using tctl (if installed)
tctl workflow start --workflow-type ProcessJobWorkflow --input '{"id":"test-1","payload":"dGVzdA==","attempt":1,"submitted_at":0}'
```

### Check metrics
```bash
curl -s http://localhost:9091/metrics | grep bench_
```

### Monitor workflow execution
- Use Temporal UI to see workflow history
- Check activity retries and backoff
- Monitor heartbeat status

## Troubleshooting

### Worker fails to connect to Temporal
- Ensure Temporal server is initialized: `docker logs temporal`
- Check if Temporal DB is ready: `docker logs temporal-db`
- Verify network: `docker network inspect bench-net`

### Workflows not progressing
- Check worker logs: `docker logs temporal-worker`
- Verify worker is registered on task queue
- Check for activity errors in Temporal UI

### Stuck workflows
- If Temporal DB is down, workflows can't progress
- Restart Temporal and it will resume automatically
- Check metrics for in-flight count

## Performance Notes

**Throughput**: 50-300 msg/sec (single worker)
- Slowest framework due to history persistence
- Overhead from deterministic replay and activity scheduling

**Latency**: 100-1000ms per job
- High overhead from Temporal infrastructure
- Designed for long-running workflows, not high-throughput queues

**Redelivery Time**: 1-5 seconds (based on RetryPolicy backoff)
- Exponential backoff: 1s → 2s → 4s → 8s → 16s
- Configurable but not instant

## Comparison with Other Frameworks

**When to use Temporal**:
- Complex workflows with multiple steps
- Long-running operations (hours/days)
- Need for audit trail and history
- Can tolerate lower throughput

**Why not Temporal for simple queue processing**:
- Overkill for single-step operations
- Slower than RabbitMQ/NATS/Kafka
- Requires separate infrastructure (server + DB)
- More operational complexity

**vs Other Frameworks**:
- **vs RabbitMQ/NATS**: Much slower, but handles more complex scenarios
- **vs Kafka**: Different use case (workflows vs streaming)
- **vs Celery**: Similar use case, but Temporal has stronger durability

## Advanced Features (Not Used in Benchmark)

- **Signals**: Send data to running workflows
- **Queries**: Query workflow state
- **Continue-as-New**: Restart workflow with new parameters (for very long runs)
- **Child Workflows**: Nested workflow orchestration
- **Timers**: Schedule delayed execution
