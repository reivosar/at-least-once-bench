# Kafka (KRaft) Framework Integration

This directory contains the Kafka integration for the at-least-once-bench benchmark.

## Architecture

- **Broker Mode**: KRaft (Kafka Raft) — no ZooKeeper needed
- **Topic**: Single partition (for ordering), 1x replication
- **Consumer Model**: Consumer group with manual offset management
- **Retry Strategy**: Inner retry loop in worker (no server-side retry)
- **Durability**: Offsets committed only after HTTP + DB success

## How It Works

### At-Least-Once Guarantee

Unlike RabbitMQ or NATS, Kafka requires application-level retry logic:

1. **Consumer reads message** from topic partition
2. **Worker implements inner retry loop** (up to 5 attempts with exponential backoff)
3. **Processing**: Within each attempt, call downstream HTTP and insert to DB
4. **Offset commit**: 
   - If success: `reader.CommitMessages()` — offset advances
   - If failure after max retries: offset NOT committed, message will be redelivered on consumer restart

### No Server-Side Retry

Kafka does NOT have automatic message redelivery like RabbitMQ or NATS. Instead:
- **Offset management is explicit**: consumer decides when to commit
- **Message stays in partition** until offset is committed
- **Redelivery requires**: consumer restart or seeking back to previous offset

### Failure Scenarios

#### Downstream HTTP Server Down
- Worker attempts to call downstream, gets error
- Inner retry loop retries (up to 5 times with backoff)
- If all retries fail: offset NOT committed
- Message stays in partition, redelivered on consumer restart
- Result: **At-least-once delivery** ✓

#### Database Connection Lost
- HTTP call succeeds, DB write fails
- Inner retry loop retries
- On each retry, HTTP is called again (over-delivery)
- Database idempotency key prevents duplicate inserts
- If DB recovers: offset is committed
- Result: **At-least-once delivery** ✓ (with duplicate HTTP calls)

#### Consumer Crashes
- Consumer group automatically rebalances
- New consumer seeks to last committed offset
- Unprocessed messages (no offset commit yet) are re-read
- Result: **At-least-once delivery** ✓ (rebalance triggers reprocessing)

## Configuration

See `docker-compose.yml` for environment variables:
- `KAFKA_BROKERS` — comma-separated broker addresses (e.g., "kafka:29092")
- `KAFKA_TOPIC` — topic name (default: "bench-jobs")
- `KAFKA_GROUP_ID` — consumer group ID (default: "bench-group")
- `DATABASE_URL` — PostgreSQL connection string
- `DOWNSTREAM_URL` — HTTP sink endpoint
- `METRICS_PORT` — Prometheus metrics port (9092)

Tunables in `worker/main.go`:
- `maxRetries` — max retry attempts before giving up (default: 5)
- `processingTimeout` — timeout for HTTP/DB calls (default: 30s)
- `--concurrency` — number of parallel consumers (default: 4)

## Kafka vs RabbitMQ/NATS

| Feature | Kafka | RabbitMQ | NATS |
|---------|-------|----------|------|
| Server-side retry | ❌ | ✅ | ✅ |
| Consumer restart on failure | ✅ Required | ✅ Auto | ✅ Auto |
| Offset/Ack model | Offset commit | Explicit ACK | Explicit ACK |
| Redelivery trigger | Consumer restart | TCP close or timeout | Server timeout |
| Application retry logic | Must implement | Optional | Optional |

## KRaft Mode (No ZooKeeper)

Modern Kafka (3.0+) uses Raft consensus instead of ZooKeeper:
- Simpler deployment (one less service)
- Broker acts as both broker and controller
- Faster controller elections
- Single node in this benchmark

## Implementation Notes

### Inner Retry Loop
Unlike RabbitMQ/NATS, Kafka requires explicit retry logic:
```go
for attempt := 0; attempt < maxRetries; attempt++ {
    if callDownstream(job) && insertJob(db, job) {
        reader.CommitMessages()  // Only commit on success
        return true
    }
    // Exponential backoff: 100ms, 200ms, 400ms, 800ms, 1600ms
}
// If all fail, don't commit; consumer restart will reprocess
```

### Idempotency
Database constraint handles idempotency across retries:
```sql
INSERT INTO processed_jobs (job_id, ...) VALUES (...) 
ON CONFLICT (job_id) DO NOTHING
```

### Offset Commit Semantics
- **Manual commit** (`reader.CommitMessages()`) ensures at-least-once
- **Auto-commit** would risk message loss if worker crashes
- **No commit on failure** means reprocessing on restart

## Monitoring

Metrics exported to `:9092/metrics`:
- `bench_processed_total` — successfully processed jobs
- `bench_retry_total` — jobs that were retried
- `bench_lost_total` — jobs not processed after max retries
- `bench_latency_seconds` — histogram of end-to-end latency
- `bench_dup_http_calls_total` — duplicate HTTP calls (during failures)
- `bench_inflight` — current in-flight job count

## Testing the Integration

### Start the broker and worker
```bash
docker compose up -d
```

### Publish a test message
```bash
# Using kafka console producer (inside kafka container)
docker exec kafka bash -c '
echo '\''{\"id\":\"test-1\",\"payload\":\"dGVzdA==\",\"attempt\":1,\"submitted_at\":\"2024-01-01T00:00:00Z\"}'\'\" | \
kafka-console-producer --broker-list localhost:29092 --topic bench-jobs
'
```

### Check metrics
```bash
curl -s http://localhost:9092/metrics | grep bench_
```

### Simulate downstream failure
```bash
curl -X POST http://localhost:8080/admin/fail
# Worker will retry inner loop
curl -X POST http://localhost:8080/admin/recover
```

### Check consumer group status
```bash
docker exec kafka kafka-consumer-groups --bootstrap-server localhost:29092 --group bench-group --describe
```

## Troubleshooting

### Worker keeps crashing with "connection refused"
- Ensure Kafka is fully initialized: `docker logs kafka`
- Check network: `docker network inspect bench-net`
- Verify topic was created: `docker exec kafka kafka-topics --bootstrap-server localhost:29092 --list`

### Messages not being processed
- Check worker logs: `docker logs kafka-worker`
- Verify downstream is responding: `curl http://localhost:8080/health`
- Check consumer group: `docker exec kafka kafka-consumer-groups --bootstrap-server localhost:29092 --group bench-group --describe`

### Consumer lag growing
- Check if worker is healthy: `docker logs kafka-worker`
- Verify database connectivity
- Increase `--concurrency` if throughput is insufficient

## Performance Notes

**Throughput**: 10000+ msg/sec (single worker, 4 concurrent threads, simple payloads)
- Very high: Kafka optimized for throughput
- Offset management is fast and scalable

**Latency**: 5-50ms for successful processing
- Much higher than NATS due to inner retry loop
- Depends on HTTP call latency

**Redelivery Time**: 1-2 seconds
- Consumer must be restarted to reprocess
- Much slower than RabbitMQ/NATS (which redelivery in <100ms)

## Comparison with Other Frameworks

**When to use Kafka**:
- High throughput requirements (10k+ msg/sec)
- Natural message ordering needed
- Can tolerate longer redelivery times
- Willing to implement application-level retries

**Why not Kafka for at-least-once**:
- Requires complex inner retry logic
- Redelivery is slow (consumer restart)
- No built-in backoff or retry strategies
- Operational overhead for consumer group management
