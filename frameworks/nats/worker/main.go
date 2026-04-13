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

	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	_ "github.com/lib/pq"

	"github.com/reivosar/at-least-once-bench/shared/proto"
)

var (
	natsURL      = os.Getenv("NATS_URL")
	databaseURL  = os.Getenv("DATABASE_URL")
	downstreamURL = os.Getenv("DOWNSTREAM_URL")
	metricsPort  = os.Getenv("METRICS_PORT")
	concurrency  = flag.Int("concurrency", 4, "Number of concurrent workers")

	// NATS configuration
	streamName        = "bench-jobs"
	consumerName      = "bench-consumer"
	subject           = "jobs"
	ackWait           = 30 * time.Second
	maxDeliver        = 5
	processingTimeout = 30 * time.Second

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

	if natsURL == "" || databaseURL == "" || downstreamURL == "" || metricsPort == "" {
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

	// Connect to NATS with retries
	var nc *nats.Conn
	for i := 0; i < 30; i++ {
		nc, err = nats.Connect(natsURL)
		if err == nil {
			break
		}
		log.Printf("NATS connection attempt %d failed, retrying...", i+1)
		time.Sleep(time.Second)
	}
	if err != nil {
		log.Fatalf("Failed to connect to NATS: %v", err)
	}
	defer nc.Close()
	log.Println("Connected to NATS")

	// Enable JetStream
	js, err := nc.JetStream()
	if err != nil {
		log.Fatalf("Failed to get JetStream context: %v", err)
	}

	// Create or update stream
	streamInfo, err := js.StreamInfo(streamName)
	if err == nats.ErrStreamNotFound {
		// Create stream
		streamInfo, err = js.AddStream(&nats.StreamConfig{
			Name:     streamName,
			Subjects: []string{subject},
			Storage:  nats.FileStorage,
			MaxAge:   24 * time.Hour,
		})
		if err != nil {
			log.Fatalf("Failed to create stream: %v", err)
		}
		log.Printf("Created stream: %s", streamName)
	} else if err != nil {
		log.Fatalf("Failed to get stream info: %v", err)
	} else {
		log.Printf("Using existing stream: %s", streamName)
	}
	_ = streamInfo

	// Create or update consumer
	consumerInfo, err := js.ConsumerInfo(streamName, consumerName)
	if err == nats.ErrConsumerNotFound {
		// Create consumer
		consumerInfo, err = js.AddConsumer(streamName, &nats.ConsumerConfig{
			Name:           consumerName,
			Durable:        consumerName,
			AckPolicy:      nats.AckExplicitPolicy,
			AckWait:        ackWait,
			MaxDeliver:     maxDeliver,
			DeliverPolicy:  nats.DeliverAllPolicy,
			InactiveThreshold: 30 * time.Second,
		})
		if err != nil {
			log.Fatalf("Failed to create consumer: %v", err)
		}
		log.Printf("Created consumer: %s", consumerName)
	} else if err != nil {
		log.Fatalf("Failed to get consumer info: %v", err)
	} else {
		log.Printf("Using existing consumer: %s", consumerName)
	}
	_ = consumerInfo

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
			consumeLoop(db, js, workerID)
		}(i)
	}

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	log.Println("Shutting down...")
	wg.Wait()
}

func consumeLoop(db *sql.DB, js nats.JetStreamContext, workerID int) {
	for {
		// Subscribe to messages
		sub, err := js.PullSubscribe(subject, consumerName)
		if err != nil {
			log.Printf("Worker %d: Failed to subscribe: %v, retrying in 5s...", workerID, err)
			time.Sleep(5 * time.Second)
			continue
		}

		log.Printf("Worker %d: Started consuming from stream %s", workerID, streamName)
		processMessages(db, sub, workerID)

		sub.Unsubscribe()
		time.Sleep(time.Second)
	}
}

func processMessages(db *sql.DB, sub *nats.Subscription, workerID int) {
	for {
		// Fetch messages with timeout
		msgs, err := sub.Fetch(10, nats.MaxWait(5*time.Second), nats.Context(context.Background()))
		if err != nil && err != nats.ErrTimeout {
			log.Printf("Worker %d: Fetch error: %v", workerID, err)
			return
		}

		if len(msgs) == 0 {
			continue
		}

		for _, msg := range msgs {
			atomic.AddInt64(&inflightCount, 1)
			benchInflight.WithLabelValues().Set(float64(atomic.LoadInt64(&inflightCount)))

			var job proto.Job
			err := json.Unmarshal(msg.Data, &job)
			if err != nil {
				log.Printf("Worker %d: Failed to unmarshal job: %v", workerID, err)
				msg.Nak()
				atomic.AddInt64(&inflightCount, -1)
				continue
			}

			// Check if job was already processed (idempotency)
			var exists bool
			err = db.QueryRow("SELECT EXISTS(SELECT 1 FROM processed_jobs WHERE job_id = $1)", job.ID).Scan(&exists)
			if err != nil {
				log.Printf("Worker %d: Failed to check job existence: %v", workerID, err)
				msg.Nak()
				atomic.AddInt64(&inflightCount, -1)
				continue
			}

			if exists {
				// Job already processed
				msg.Ack()
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
				msg.Ack()
				benchProcessedTotal.WithLabelValues().Inc()

				// Calculate latency
				latency := float64(time.Now().UnixMilli()-job.SubmittedAt.UnixMilli()) / 1000.0
				benchLatencySeconds.WithLabelValues().Observe(latency)
			} else {
				// Determine if this is a retry
				if job.Attempt > 1 {
					benchRetryTotal.WithLabelValues().Inc()
				}

				// Check metadata for delivery count
				metadata, err := msg.Metadata()
				if err == nil && metadata.NumDelivered >= uint64(maxDeliver) {
					log.Printf("Worker %d: Job %s exceeded max retries (%d), sending to DLQ", workerID, job.ID, maxDeliver)
					msg.Nak()
					benchLostTotal.WithLabelValues().Inc()
				} else {
					// Increment attempt and redelivery
					job.Attempt++
					msg.Nak()
				}
			}

			atomic.AddInt64(&inflightCount, -1)
			benchInflight.WithLabelValues().Set(float64(atomic.LoadInt64(&inflightCount)))
		}
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
