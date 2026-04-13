package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	amqp "github.com/rabbitmq/amqp091-go"
	_ "github.com/lib/pq"

	"github.com/reivosar/at-least-once-bench/shared/proto"
)

var (
	rabbitmqHost     = os.Getenv("RABBITMQ_HOST")
	rabbitmqPort     = os.Getenv("RABBITMQ_PORT")
	rabbitmqUser     = os.Getenv("RABBITMQ_USER")
	rabbitmqPassword = os.Getenv("RABBITMQ_PASSWORD")
	databaseURL      = os.Getenv("DATABASE_URL")
	downstreamURL    = os.Getenv("DOWNSTREAM_URL")
	metricsPort      = os.Getenv("METRICS_PORT")
	concurrency      = flag.Int("concurrency", 4, "Number of concurrent workers")

	// RabbitMQ configuration
	queueName         = "bench-jobs"
	dlxExchangeName   = "bench-dlx"
	dlqName           = "bench-jobs-dlq"
	processingTimeout = 30 * time.Second
	maxRetries        = 5

	// Prometheus metrics
	benchProcessedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "bench_processed_total",
			Help: "Total number of successfully processed jobs",
		},
		[]string{},
	)
	benchRetryTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "bench_retry_total",
			Help: "Total number of retried jobs",
		},
		[]string{},
	)
	benchLostTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "bench_lost_total",
			Help: "Total number of lost jobs (exceeded max retries)",
		},
		[]string{},
	)
	benchDupHTTPCallsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "bench_dup_http_calls_total",
			Help: "Total number of duplicate HTTP calls (from retries)",
		},
		[]string{},
	)
	benchLatencySeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "bench_latency_seconds",
			Help:    "Latency from job submission to completion",
			Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 5, 10, 30, 60},
		},
		[]string{},
	)
	benchInflight = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "bench_inflight",
			Help: "Number of jobs currently being processed",
		},
		[]string{},
	)

	inflightCount int64
)

func init() {
	prometheus.MustRegister(benchProcessedTotal)
	prometheus.MustRegister(benchRetryTotal)
	prometheus.MustRegister(benchLostTotal)
	prometheus.MustRegister(benchDupHTTPCallsTotal)
	prometheus.MustRegister(benchLatencySeconds)
	prometheus.MustRegister(benchInflight)
}

// SinkRequest mirrors the structure expected by the downstream sink
type SinkRequest struct {
	ID        string `json:"id"`
	Payload   []byte `json:"payload"`
	Attempt   int    `json:"attempt"`
	Timestamp int64  `json:"timestamp"`
}

func main() {
	flag.Parse()

	if rabbitmqHost == "" || databaseURL == "" || downstreamURL == "" || metricsPort == "" {
		log.Fatalf("Missing required environment variables")
	}

	// Connect to PostgreSQL
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// Test database connection with retries
	for i := 0; i < 30; i++ {
		err = db.Ping()
		if err == nil {
			break
		}
		log.Printf("Database connection attempt %d failed, retrying...", i+1)
		time.Sleep(time.Second)
	}
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}
	log.Println("Connected to PostgreSQL")

	// Connect to RabbitMQ with retries
	var conn *amqp.Connection
	for i := 0; i < 30; i++ {
		dsn := fmt.Sprintf("amqp://%s:%s@%s:%s/", rabbitmqUser, rabbitmqPassword, rabbitmqHost, rabbitmqPort)
		conn, err = amqp.Dial(dsn)
		if err == nil {
			break
		}
		log.Printf("RabbitMQ connection attempt %d failed, retrying...", i+1)
		time.Sleep(time.Second)
	}
	if err != nil {
		log.Fatalf("Failed to connect to RabbitMQ: %v", err)
	}
	defer conn.Close()
	log.Println("Connected to RabbitMQ")

	// Set up RabbitMQ
	ch, err := conn.Channel()
	if err != nil {
		log.Fatalf("Failed to open channel: %v", err)
	}
	defer ch.Close()

	// Declare DLX (dead-letter exchange)
	err = ch.ExchangeDeclare(dlxExchangeName, "direct", true, false, false, false, nil)
	if err != nil {
		log.Fatalf("Failed to declare DLX: %v", err)
	}

	// Declare DLQ (dead-letter queue)
	_, err = ch.QueueDeclare(dlqName, true, false, false, false, nil)
	if err != nil {
		log.Fatalf("Failed to declare DLQ: %v", err)
	}

	// Bind DLQ to DLX
	err = ch.QueueBind(dlqName, "", dlxExchangeName, false, nil)
	if err != nil {
		log.Fatalf("Failed to bind DLQ: %v", err)
	}

	// Declare main queue with DLX configured
	args := amqp.Table{
		"x-queue-type":             "quorum",
		"x-dead-letter-exchange":   dlxExchangeName,
		"x-max-length":             1000000, // Prevent unbounded queue growth during testing
	}
	_, err = ch.QueueDeclare(queueName, true, false, false, false, args)
	if err != nil {
		log.Fatalf("Failed to declare queue: %v", err)
	}
	log.Printf("Declared quorum queue: %s", queueName)

	// Start metrics server
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		if err := http.ListenAndServe(fmt.Sprintf(":%s", metricsPort), mux); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Metrics server error: %v", err)
		}
	}()
	log.Printf("Metrics server started on port %s", metricsPort)

	// Start consumer workers
	var wg sync.WaitGroup
	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			consumeLoop(db, workerID)
		}(i)
	}

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	log.Println("Shutting down...")
	wg.Wait()
}

func consumeLoop(db *sql.DB, workerID int) {
	for {
		// Reconnect if channel is closed
		conn, err := amqp.Dial(fmt.Sprintf("amqp://%s:%s@%s:%s/", rabbitmqUser, rabbitmqPassword, rabbitmqHost, rabbitmqPort))
		if err != nil {
			log.Printf("Worker %d: Failed to reconnect to RabbitMQ: %v, retrying in 5s...", workerID, err)
			time.Sleep(5 * time.Second)
			continue
		}

		ch, err := conn.Channel()
		if err != nil {
			conn.Close()
			log.Printf("Worker %d: Failed to open channel: %v, retrying in 5s...", workerID, err)
			time.Sleep(5 * time.Second)
			continue
		}

		// Set QoS
		ch.Qos(1, 0, false)

		// Consume messages
		msgs, err := ch.Consume(queueName, fmt.Sprintf("worker-%d", workerID), false, false, false, false, nil)
		if err != nil {
			ch.Close()
			conn.Close()
			log.Printf("Worker %d: Failed to consume: %v, retrying in 5s...", workerID, err)
			time.Sleep(5 * time.Second)
			continue
		}

		log.Printf("Worker %d: Started consuming from %s", workerID, queueName)
		processMessages(ch, msgs, db, workerID)

		ch.Close()
		conn.Close()
		time.Sleep(time.Second)
	}
}

func processMessages(ch *amqp.Channel, msgs <-chan amqp.Delivery, db *sql.DB, workerID int) {
	for delivery := range msgs {
		atomic.AddInt64(&inflightCount, 1)
		benchInflight.WithLabelValues().Set(float64(atomic.LoadInt64(&inflightCount)))

		var job proto.Job
		err := json.Unmarshal(delivery.Body, &job)
		if err != nil {
			log.Printf("Worker %d: Failed to unmarshal job: %v", workerID, err)
			delivery.Nack(false, false) // Send to DLX
			atomic.AddInt64(&inflightCount, -1)
			continue
		}

		// Check if job was already processed (idempotency)
		var exists bool
		err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM processed_jobs WHERE job_id = $1)", job.ID).Scan(&exists)
		if err != nil {
			log.Printf("Worker %d: Failed to check job existence: %v", workerID, err)
			delivery.Nack(false, true) // Requeue
			atomic.AddInt64(&inflightCount, -1)
			continue
		}

		if exists {
			// Job already processed
			delivery.Ack(false)
			atomic.AddInt64(&inflightCount, -1)
			benchProcessedTotal.WithLabelValues().Inc()
			continue
		}

		// Process the job: call downstream and write to DB
		processed := false
		if callDownstream(job) {
			if insertJob(db, job) {
				processed = true
			}
		}

		if processed {
			delivery.Ack(false)
			benchProcessedTotal.WithLabelValues().Inc()

			// Calculate latency
			latency := float64(time.Now().UnixMilli()-job.SubmittedAt.UnixMilli()) / 1000.0
			benchLatencySeconds.WithLabelValues().Observe(latency)
		} else {
			// Determine if this is a retry
			if job.Attempt > 1 {
				benchRetryTotal.WithLabelValues().Inc()
			}

			// Check if we've exceeded max retries
			if job.Attempt >= maxRetries {
				log.Printf("Worker %d: Job %s exceeded max retries (%d), sending to DLX", workerID, job.ID, maxRetries)
				delivery.Nack(false, false) // Send to DLX
				benchLostTotal.WithLabelValues().Inc()
			} else {
				// Increment attempt and requeue
				job.Attempt++
				delivery.Nack(false, true) // Requeue for retry
			}
		}

		atomic.AddInt64(&inflightCount, -1)
		benchInflight.WithLabelValues().Set(float64(atomic.LoadInt64(&inflightCount)))
	}
}

func callDownstream(job proto.Job) bool {
	req := SinkRequest{
		ID:        job.ID,
		Payload:   job.Payload,
		Attempt:   job.Attempt,
		Timestamp: job.SubmittedAt.UnixMilli(),
	}

	body, err := json.Marshal(req)
	if err != nil {
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), processingTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/sink", downstreamURL), bytes.NewReader(body))
	if err != nil {
		return false
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: processingTimeout}
	resp, err := client.Do(httpReq)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	benchDupHTTPCallsTotal.WithLabelValues().Inc()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func insertJob(db *sql.DB, job proto.Job) bool {
	// INSERT ... ON CONFLICT DO NOTHING for idempotency
	var inserted bool
	err := db.QueryRow(`
		INSERT INTO processed_jobs (job_id, payload, attempt, ts)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (job_id) DO NOTHING
		RETURNING true
	`, job.ID, job.Payload, job.Attempt).Scan(&inserted)

	if err != nil && err != sql.ErrNoRows {
		log.Printf("Failed to insert job: %v", err)
		return false
	}

	return inserted || err == sql.ErrNoRows // If no rows inserted, job was already there
}
