# RabbitMQ Framework Integration

This directory contains the RabbitMQ integration for the at-least-once-bench benchmark.

## Architecture

- **Queue Type**: Quorum queues (Raft-based, durable)
- **Acknowledgment**: Explicit ACK/NACK per message
- **Retry Mechanism**: Automatic requeue via NACK with requeue=true
- **Dead-Letter Strategy**: Messages exceeding max retries are sent to a dead-letter queue (DLQ)

## How It Works

### At-Least-Once Guarantee

1. **Consumer receives message**: RabbitMQ holds the message in "unacked" state
2. **Processing**: Worker calls downstream HTTP endpoint and inserts into database
3. **Idempotency check**: Database `UNIQUE(job_id)` constraint prevents duplicates
4. **Acknowledgment**: 
   - If both HTTP call and DB write succeed: `ch.Ack(false)` — message is removed from queue
   - If either fails: `ch.Nack(false, true)` — message is requeued and will be redelivered

### Max Retries

When a message has been redelivered more than `maxRetries` (currently 5), it is sent to the dead-letter queue (DLX) instead of being requeued again.

### Failure Scenarios

#### Downstream HTTP Server Down
- Worker attempts to call downstream, gets error
- Message is **not** acked, stays in queue
- On recovery, worker retries the same message
- Result: **At-least-once delivery** ✓

#### Database Connection Lost
- HTTP call succeeds, DB write fails
- Message is **not** acked, stays in queue
- On retry, worker calls HTTP again (over-delivery)
- Database idempotency key prevents duplicate inserts
- Result: **At-least-once delivery** ✓ (with duplicate HTTP calls)

#### Worker Crashes
- TCP connection is dropped
- RabbitMQ immediately requeues all in-flight unacked messages
- Next healthy worker picks up the message
- Result: **At-least-once delivery** ✓ (fast redelivery)

## Configuration

See `docker-compose.yml` for environment variables:
- `RABBITMQ_HOST`, `RABBITMQ_PORT`, `RABBITMQ_USER`, `RABBITMQ_PASSWORD` — broker connection
- `DATABASE_URL` — PostgreSQL connection string
- `DOWNSTREAM_URL` — HTTP sink endpoint
- `METRICS_PORT` — Prometheus metrics port (9094)

Tunables in `worker/main.go`:
- `queueName` — queue name (default: "bench-jobs")
- `maxRetries` — max redelivery attempts (default: 5)
- `processingTimeout` — timeout for HTTP/DB calls (default: 30s)
- `--concurrency` — number of parallel consumers (default: 4)

## Quorum Queues

RabbitMQ quorum queues use Raft consensus for durability:
- Messages are replicated across nodes (on a cluster)
- On single-node Docker setup, a quorum of 1 is used (defaults to safe mode)
- Survive broker restarts without message loss
- Slower than classic queues but more reliable

## Monitoring

Metrics exported to `:9094/metrics`:
- `bench_processed_total` — successfully processed jobs
- `bench_retry_total` — jobs that were retried
- `bench_lost_total` — jobs sent to DLQ (max retries exceeded)
- `bench_latency_seconds` — histogram of end-to-end latency
- `bench_dup_http_calls_total` — duplicate HTTP calls (during failures)
- `bench_inflight` — current in-flight job count

## Key Design Decisions

### Quorum Queues Over Classic
- **Why**: Quorum queues survive broker restarts; classic queues may lose messages in edge cases
- **Tradeoff**: Slightly higher latency and memory overhead
- **For single-node Docker**: Quorum of 1, no replication overhead

### Explicit ACK/NACK Over Auto-Ack
- **Why**: Guarantees no message loss on worker crash
- **Tradeoff**: Slightly more complex application code
- **Result**: True at-least-once delivery

### Dead-Letter Queue for Poison Messages
- **Why**: Prevent infinite retry loops on permanently broken messages
- **Result**: Unprocessable messages are isolated in DLQ instead of jamming the main queue

### QoS = 1 Per Consumer
- **Why**: Limits in-flight messages to 1 per worker, prevents overload
- **Tradeoff**: Slightly lower throughput than QoS > 1
- **Result**: Fairer distribution and predictable memory usage

## Testing the Integration

### Start the broker and worker
```bash
docker compose up -d
```

### Publish a test message
```bash
docker exec rabbitmq-worker curl -X POST http://downstream:8080/sink \
  -H "Content-Type: application/json" \
  -d '{"id":"test-1","payload":"dGVzdA==","attempt":1,"timestamp":0}'
```

### Check metrics
```bash
curl http://localhost:9094/metrics | grep bench_
```

### Simulate downstream failure
```bash
curl -X POST http://localhost:8080/admin/fail
# Worker will retry
curl -X POST http://localhost:8080/admin/recover
```

### Check RabbitMQ management UI
```
http://localhost:15672
Username: bench
Password: benchpass
```

## Troubleshooting

### Worker keeps crashing with "connection refused"
- Ensure RabbitMQ is fully initialized: `docker logs rabbitmq`
- Check network: `docker network inspect bench-net`

### Messages piling up in queue
- Check worker logs: `docker logs rabbitmq-worker`
- Check if downstream is responding: `curl http://localhost:8080/health`
- Check database: is it reachable from the worker?

### Messages in dead-letter queue
- Check worker logs for processing errors
- Verify downstream is behaving correctly
- Increase `processingTimeout` if operations are slow
