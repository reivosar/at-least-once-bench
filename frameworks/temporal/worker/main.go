package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	_ "github.com/lib/pq"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/reivosar/at-least-once-bench/shared/proto"
)

var (
	temporalHost  = os.Getenv("TEMPORAL_HOST")
	temporalPort  = os.Getenv("TEMPORAL_PORT")
	databaseURL   = os.Getenv("DATABASE_URL")
	downstreamURL = os.Getenv("DOWNSTREAM_URL")
	metricsPort   = os.Getenv("METRICS_PORT")

	// Global state shared with activities
	globalDB            *sql.DB
	globalDownstreamURL string

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

const (
	TaskQueueName = "bench-task-queue"
	WorkflowName  = "ProcessJobWorkflow"
)

// WorkflowStarter is a simple server to start workflows
type WorkflowStarter struct {
	client client.Client
}

func (ws *WorkflowStarter) Start(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var job proto.Job
	if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	workflowID := fmt.Sprintf("job-%s", job.ID)

	_, err := ws.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: TaskQueueName,
	}, WorkflowName, job)

	if err != nil {
		http.Error(w, fmt.Sprintf("Failed to start workflow: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"workflow_id": workflowID})
}

func main() {
	flag.Parse()

	if temporalHost == "" || temporalPort == "" || databaseURL == "" || downstreamURL == "" || metricsPort == "" {
		log.Fatalf("Missing required environment variables")
	}

	// Store globals for activities
	globalDownstreamURL = downstreamURL

	// Connect to PostgreSQL
	var err error
	globalDB, err = sql.Open("postgres", databaseURL)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer globalDB.Close()

	// Test database connection with retries
	for i := 0; i < 30; i++ {
		err = globalDB.Ping()
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

	// Create Temporal client
	temporalURL := fmt.Sprintf("%s:%s", temporalHost, temporalPort)
	c, err := client.Dial(client.Options{
		HostPort: temporalURL,
	})
	if err != nil {
		log.Fatalf("Failed to create Temporal client: %v", err)
	}
	defer c.Close()
	log.Printf("Connected to Temporal at %s", temporalURL)

	// Start metrics server
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		if err := http.ListenAndServe(fmt.Sprintf(":%s", metricsPort), mux); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Metrics server error: %v", err)
		}
	}()
	log.Printf("Metrics server started on port %s", metricsPort)

	// Start workflow submission server
	starter := &WorkflowStarter{client: c}
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/start-workflow", starter.Start)
		if err := http.ListenAndServe(":7234", mux); err != nil && err != http.ErrServerClosed {
			log.Printf("Workflow starter error: %v", err)
		}
	}()
	log.Println("Workflow starter started on port 7234")

	// Create and run worker
	w := worker.New(c, TaskQueueName, worker.Options{})

	// Register workflow and activities
	w.RegisterWorkflow(ProcessJobWorkflow)
	w.RegisterActivity(ProcessJobActivity)
	w.RegisterActivity(RecordLostJobActivity)

	// Run worker
	go func() {
		if err := w.Run(worker.InterruptCh()); err != nil {
			log.Printf("Worker error: %v", err)
		}
	}()
	log.Printf("Worker started, listening on task queue: %s", TaskQueueName)

	// Graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan
	log.Println("Shutting down...")
	w.Stop()
}

// Note: In a real benchmark, we'd need a proper workflow submitter.
// This worker-based approach is designed for background processing.
// The benchmark runner would need to be updated to use Temporal SDK
// to submit workflows (shown in WorkflowStarter above).
