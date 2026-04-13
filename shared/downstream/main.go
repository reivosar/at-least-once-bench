package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// Flags
	port     = flag.Int("port", 8080, "HTTP server port")
	metricsPort = flag.Int("metrics-port", 8081, "Prometheus metrics port")

	// Failure mode (accessed via /admin/fail and /admin/recover)
	failureMu sync.RWMutex
	isFailing bool

	// Prometheus metrics
	sinkRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "downstream_requests_total",
			Help: "Total number of requests to /sink",
		},
		[]string{"status"},
	)
	sinkLatencySeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "downstream_latency_seconds",
			Help:    "Latency of /sink requests",
			Buckets: prometheus.DefBuckets,
		},
		[]string{},
	)
)

func init() {
	prometheus.MustRegister(sinkRequestsTotal)
	prometheus.MustRegister(sinkLatencySeconds)
}

// SinkRequest represents a job submission to the sink.
type SinkRequest struct {
	ID        string `json:"id"`
	Payload   []byte `json:"payload"`
	Attempt   int    `json:"attempt"`
	Timestamp int64  `json:"timestamp"` // unix millis for latency calc
}

func handleSink(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	failureMu.RLock()
	failing := isFailing
	failureMu.RUnlock()

	if failing {
		w.WriteHeader(http.StatusServiceUnavailable)
		sinkRequestsTotal.WithLabelValues("503").Inc()
		sinkLatencySeconds.WithLabelValues().Observe(time.Since(start).Seconds())
		fmt.Fprintf(w, "Service unavailable (admin-injected failure)\n")
		return
	}

	// Read and validate request
	body, err := io.ReadAll(r.Body)
	defer r.Body.Close()
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		sinkRequestsTotal.WithLabelValues("400").Inc()
		sinkLatencySeconds.WithLabelValues().Observe(time.Since(start).Seconds())
		return
	}

	var req SinkRequest
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		sinkRequestsTotal.WithLabelValues("400").Inc()
		sinkLatencySeconds.WithLabelValues().Observe(time.Since(start).Seconds())
		return
	}

	// Echo the request back (lightweight, just confirm receipt)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"id": req.ID,
		"attempt": req.Attempt,
	})

	sinkRequestsTotal.WithLabelValues("200").Inc()
	sinkLatencySeconds.WithLabelValues().Observe(time.Since(start).Seconds())
}

func handleAdminFail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	failureMu.Lock()
	isFailing = true
	failureMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "failing"})
}

func handleAdminRecover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	failureMu.Lock()
	isFailing = false
	failureMu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "recovered"})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	failureMu.RLock()
	failing := isFailing
	failureMu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	if failing {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "unhealthy",
			"failing": true,
		})
	} else {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "healthy",
			"failing": false,
		})
	}
}

func main() {
	flag.Parse()

	// Main HTTP server
	mux := http.NewServeMux()
	mux.HandleFunc("/sink", handleSink)
	mux.HandleFunc("/admin/fail", handleAdminFail)
	mux.HandleFunc("/admin/recover", handleAdminRecover)
	mux.HandleFunc("/health", handleHealth)

	go func() {
		log.Printf("Starting HTTP server on port %d", *port)
		if err := http.ListenAndServe(fmt.Sprintf(":%d", *port), mux); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	// Metrics server
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())

	log.Printf("Starting Prometheus metrics server on port %d", *metricsPort)
	if err := http.ListenAndServe(fmt.Sprintf(":%d", *metricsPort), metricsMux); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Metrics server error: %v", err)
	}
}
