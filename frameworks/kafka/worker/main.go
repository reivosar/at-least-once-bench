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
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	_ "github.com/lib/pq"

	"github.com/reivosar/at-least-once-bench/shared/proto"
)

var (
	kafkaBrokers  = os.Getenv("KAFKA_BROKERS")
	kafkaTopic    = os.Getenv("KAFKA_TOPIC")
	kafkaGroupID  = os.Getenv("KAFKA_GROUP_ID")
	databaseURL   = os.Getenv("DATABASE_URL")
	downstreamURL = os.Getenv("DOWNSTREAM_URL")
	metricsPort   = os.Getenv("METRICS_PORT")
	concurrency   = flag.Int("concurrency", 4, "Number of concurrent workers")

	// Configuration
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

type SinkRequest struct {
	ID        string `json:"id"`
	Payload   []byte `json:"payload"`
	Attempt   int    `json:"attempt"`
	Timestamp int64  `json:"timestamp"`
}

func main() {
	flag.Parse()

	if kafkaBrokers == "" || kafkaTopic == "" || kafkaGroupID == "" || databaseURL == "" || downstreamURL == "" || metricsPort == "" {
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

	// Parse broker addresses
	brokers := strings.Split(kafkaBrokers, ",")
	log.Printf("Kafka brokers: %v", brokers)

	// Create topic with retries
	for i := 0; i < 30; i++ {
		conn, err := kafka.Dial("tcp", brokers[0])
		if err != nil {
			log.Printf("Kafka connection attempt %d failed, retrying...", i+1)
			time.Sleep(time.Second)
			continue
		}

		// Try to create topic
		err = conn.CreateTopics(kafka.TopicConfig{
			Topic:             kafkaTopic,
			NumPartitions:     1,
			ReplicationFactor: 1,
		})
		if err != nil && !strings.Contains(err.Error(), "topic already exists") {
			conn.Close()
			log.Printf("Failed to create topic attempt %d: %v, retrying...", i+1, err)
			time.Sleep(time.Second)
			continue
		}
		conn.Close()
		log.Printf("Topic %s ready", kafkaTopic)
		break
	}

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
			consumeLoop(db, brokers, workerID)
		}(i)
	}

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	log.Println("Shutting down...")
	wg.Wait()
}

func consumeLoop(db *sql.DB, brokers []string, workerID int) {
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        brokers,
		Topic:          kafkaTopic,
		GroupID:        kafkaGroupID,
		SessionTimeout: 10 * time.Second,
		ReadBackoffMin: 100 * time.Millisecond,
		ReadBackoffMax: 1 * time.Second,
	})
	defer reader.Close()

	log.Printf("Worker %d: Started consuming from topic %s", workerID, kafkaTopic)

	for {
		// Fetch message without auto-committing offset
		readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
		msg, err := reader.FetchMessage(readCtx)
		readCancel()

		if err != nil {
			if strings.Contains(err.Error(), "context deadline exceeded") {
				continue // No messages available, keep polling
			}
			log.Printf("Worker %d: Read error: %v, reconnecting...", workerID, err)
			reader.Close()
			time.Sleep(time.Second)
			reader = kafka.NewReader(kafka.ReaderConfig{
				Brokers:        brokers,
				Topic:          kafkaTopic,
				GroupID:        kafkaGroupID,
				SessionTimeout: 10 * time.Second,
				ReadBackoffMin: 100 * time.Millisecond,
				ReadBackoffMax: 1 * time.Second,
			})
			continue
		}

		processMessage(db, reader, msg, workerID)
	}
}

func processMessage(db *sql.DB, reader *kafka.Reader, msg kafka.Message, workerID int) {
	atomic.AddInt64(&inflightCount, 1)
	benchInflight.WithLabelValues().Set(float64(atomic.LoadInt64(&inflightCount)))
	defer func() {
		atomic.AddInt64(&inflightCount, -1)
		benchInflight.WithLabelValues().Set(float64(atomic.LoadInt64(&inflightCount)))
	}()

	var job proto.Job
	err := json.Unmarshal(msg.Value, &job)
	if err != nil {
		log.Printf("Worker %d: Failed to unmarshal job: %v", workerID, err)
		return
	}

	// Check if job was already processed (idempotency)
	var exists bool
	err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM processed_jobs WHERE job_id = $1)", job.ID).Scan(&exists)
	if err != nil {
		// DB unreachable — don't commit offset, message will be retried
		log.Printf("Worker %d: Failed to check job existence: %v (will retry)", workerID, err)
		benchRetryTotal.WithLabelValues().Inc()
		return
	}

	if exists {
		// Job already processed, commit offset
		reader.CommitMessages(context.Background(), msg)
		benchProcessedTotal.WithLabelValues().Inc()
		return
	}

	// Process job with inner retry loop
	processed := processJobWithRetry(db, job, workerID)

	if processed {
		// Commit offset only after successful processing
		reader.CommitMessages(context.Background(), msg)
		benchProcessedTotal.WithLabelValues().Inc()

		// Calculate latency
		latency := float64(time.Now().UnixMilli()-job.SubmittedAt.UnixMilli()) / 1000.0
		benchLatencySeconds.WithLabelValues().Observe(latency)
	} else {
		// Commit offset to avoid infinite reprocessing of poison messages
		reader.CommitMessages(context.Background(), msg)
		log.Printf("Worker %d: Job %s exceeded max retries, lost", workerID, job.ID)
		benchLostTotal.WithLabelValues().Inc()
	}
}

func processJobWithRetry(db *sql.DB, job proto.Job, workerID int) bool {
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			benchRetryTotal.WithLabelValues().Inc()
			log.Printf("Worker %d: Retrying job %s (attempt %d/%d)", workerID, job.ID, attempt+1, maxRetries)
			time.Sleep(time.Duration((1 << uint(attempt)) * 100) * time.Millisecond) // Exponential backoff
		}

		// Try to process
		if callDownstream(job) {
			if insertJob(db, job) {
				return true
			}
		}
	}
	return false
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
