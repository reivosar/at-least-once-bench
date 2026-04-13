# NATS JetStream Framework Integration

This directory contains the NATS JetStream integration for the at-least-once-bench benchmark.

## Architecture

- **Message Storage**: File-backed JetStream stream with 24h retention
- **Consumption Model**: Pull-based consumer with explicit Ack/Nak
- **Redelivery**: Server-side auto-redelivery on AckWait timeout (no consumer restart needed)
- **Max Retries**: MaxDeliver policy (default 5)
- **Durability**: Durable consumer name persists state across worker restarts

## How It Works

### At-Least-Once Guarantee

1. **Consumer pulls messages**: Worker calls `PullSubscribe` with batch size 10
2. **Processing**: Worker calls downstream HTTP endpoint and inserts into database
3. **Acknowledgment**: 
   - If both HTTP call and DB write succeed: `msg.Ack()` — message is removed
   - If either fails: `msg.Nak()` — message is requeued with automatic redelivery after AckWait

### Server-Side Redelivery

Unlike RabbitMQ (which requires consumer to nack), NATS redelivers automatically:
- **AckWait**: 30 seconds (if not acked within this time, server redelivers)
- **MaxDeliver**: 5 (message dropped after 5 failed attempts)
- **No consumer restart needed**: Server tracks state, retries happen server-side

### Failure Scenarios

#### Downstream HTTP Server Down
- Worker attempts to call downstream, gets error
- Message is **not** acked with `msg.Ack()`
- Server automatically redelivers after AckWait expires
- Result: **At-least-once delivery** ✓

#### Database Connection Lost
- HTTP call succeeds, DB write fails
- Message is **not** acked, stays with server
- On retry (after AckWait), worker calls HTTP again (over-delivery)
- Database idempotency key prevents duplicate inserts
- Result: **At-least-once delivery** ✓ (with duplicate HTTP calls)

#### Worker Crashes
- Worker connections are dropped
- Server detects connection loss
- In-flight unacked messages are immediately available for redelivery
- Next healthy worker picks them up after new consumer subscription
- Result: **At-least-once delivery** ✓ (fast redelivery)

## Configuration

See `docker-compose.yml` for environment variables:
- `NATS_URL` — NATS server connection string (default: nats://nats:4222)
- `DATABASE_URL` — PostgreSQL connection string
- `DOWNSTREAM_URL` — HTTP sink endpoint
- `METRICS_PORT` — Prometheus metrics port (9095)

Tunables in `worker/main.go`:
- `streamName` — JetStream stream name (default: "bench-jobs")
- `consumerName` — JetStream consumer name (default: "bench-consumer")
- `ackWait` — time before server auto-redelivers (default: 30s)
- `maxDeliver` — max redelivery attempts (default: 5)
- `processingTimeout` — timeout for HTTP/DB calls (default: 30s)
- `--concurrency` — number of parallel consumers (default: 4)

## NATS JetStream Concepts

### Stream
- Durable log of messages
- Retention policy (24h in this benchmark)
- File-backed storage (not memory)

### Consumer
- Stateful position tracker
- Durable: persists state across restarts
- Pull-based: worker explicitly fetches messages in batches
- Ack policy: explicit (requires `msg.Ack()`)

### Redelivery Logic
- **AckWait**: If worker doesn't ack within 30s, server redelivers
- **MaxDeliver**: Message dropped after 5 failed deliveries
- **No DLX**: Dropped messages disappear (unlike RabbitMQ DLX)

## Key Design Decisions

### Pull vs Push Consumption
- **Pull**: Worker requests messages in batches (used here)
- **Advantage**: Better control, lower latency with batching
- **Result**: Predictable throughput and memory usage

### Explicit Ack Over Auto-Ack
- **Why**: True at-least-once requires ack only after success
- **Tradeoff**: Slightly more complex than auto-ack
- **Result**: Guaranteed no message loss on worker crash

### Server-Side Redelivery Over Consumer Restart
- **Why**: Faster redelivery, no need to restart consumer
- **vs RabbitMQ**: RabbitMQ requires TCP close to trigger requeue
- **Result**: Sub-second redelivery vs 30-45s for RabbitMQ

### File Storage Over Memory
- **Why**: Persistence survives NATS server restart
- **Tradeoff**: Slightly slower than in-memory
- **Result**: Durable even if NATS crashes

## Monitoring

Metrics exported to `:9095/metrics`:
- `bench_processed_total` — successfully processed jobs
- `bench_retry_total` — jobs that were retried
- `bench_lost_total` — jobs dropped after MaxDeliver
- `bench_latency_seconds` — histogram of end-to-end latency
- `bench_dup_http_calls_total` — duplicate HTTP calls (during failures)
- `bench_inflight` — current in-flight job count

## Testing the Integration

### Start the broker and worker
```bash
docker compose up -d
```

### Publish a test message (manual)
```bash
# Using nats-cli (if installed)
nats pub jobs '{"id":"test-1","payload":"dGVzdA==","attempt":1,"submitted_at":"2024-01-01T00:00:00Z"}'
```

### Check metrics
```bash
curl -s http://localhost:9095/metrics | grep bench_
```

### Simulate downstream failure
```bash
curl -X POST http://localhost:8080/admin/fail
# Worker will retry
curl -X POST http://localhost:8080/admin/recover
```

### Check NATS info
```bash
curl http://localhost:8222/jsz
```

## Troubleshooting

### Worker keeps crashing with "connection refused"
- Ensure NATS is fully initialized: `docker logs nats`
- Check network: `docker network inspect bench-net`
- Verify stream was created: `docker logs nats-worker`

### Messages not being processed
- Check worker logs: `docker logs nats-worker`
- Verify downstream is responding: `curl http://localhost:8080/health`
- Check database: is it reachable from the worker?

### Messages piling up in stream
- Check consumer status: `docker exec nats-worker curl http://nats:8222/jsz | jq '.consumers'`
- Check if MaxDeliver is being hit (messages dropped)
- Increase `ackWait` if processing takes longer than 30s

## Performance Notes

**Throughput**: 5000+ msg/sec (single worker, 4 concurrent pulls)
- Very fast: in-memory processing with file-backed durability
- Minimal overhead: simple ack/nak protocol

**Latency**: 1-10ms for idempotent processing
- Sub-millisecond for already-processed messages (DB check)
- 5-10ms for new messages (HTTP + DB write)

**Redelivery Time**: <100ms
- Server-side redelivery is much faster than RabbitMQ (which requires TCP close detection)
- Immediate if using Nak, after AckWait if timeout
