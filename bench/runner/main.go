package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/reivosar/at-least-once-bench/bench/scenarios"
)

type Config struct {
	Framework   string
	Scenario    string
	Duration    time.Duration
	Rate        int // messages per second
	Warmup      time.Duration
	Cooldown    time.Duration
	ReportPath  string
	DownstreamURL string
}

type Result struct {
	Framework       string                 `json:"framework"`
	Scenario        string                 `json:"scenario"`
	StartTime       time.Time              `json:"start_time"`
	EndTime         time.Time              `json:"end_time"`
	Duration        float64                `json:"duration_seconds"`
	JobsSubmitted   int                    `json:"jobs_submitted"`
	JobsProcessed   int                    `json:"jobs_processed"`
	JobsRetried     int                    `json:"jobs_retried"`
	JobsLost        int                    `json:"jobs_lost"`
	DupHTTPCalls    int                    `json:"duplicate_http_calls"`
	Throughput      float64                `json:"throughput_msg_sec"`
	Latency         LatencyStats           `json:"latency_seconds"`
	Metrics         map[string]interface{} `json:"metrics"`
}

type LatencyStats struct {
	Min    float64 `json:"min"`
	Max    float64 `json:"max"`
	Mean   float64 `json:"mean"`
	P50    float64 `json:"p50"`
	P95    float64 `json:"p95"`
	P99    float64 `json:"p99"`
}

type SinkRequest struct {
	ID        string `json:"id"`
	Payload   []byte `json:"payload"`
	Attempt   int    `json:"attempt"`
	Timestamp int64  `json:"timestamp"`
}

func main() {
	cfg := parseFlags()

	log.Printf("Starting benchmark: framework=%s scenario=%s duration=%v rate=%d",
		cfg.Framework, cfg.Scenario, cfg.Duration, cfg.Rate)

	result := &Result{
		Framework:     cfg.Framework,
		Scenario:      cfg.Scenario,
		StartTime:     time.Now(),
		JobsSubmitted: 0,
		Metrics:       make(map[string]interface{}),
	}

	// Wait for downstream to be healthy
	if !waitForDownstream(cfg.DownstreamURL, 30*time.Second) {
		log.Fatal("Downstream never became healthy")
	}

	// Warmup phase: submit jobs at specified rate
	log.Printf("Warmup phase: %v", cfg.Warmup)
	result.JobsSubmitted = submitJobs(cfg, cfg.Warmup)

	// Wait a bit for initial processing
	time.Sleep(2 * time.Second)

	// Create and inject scenario
	scenario := getScenario(cfg)
	if scenario == nil {
		log.Fatalf("Unknown scenario: %s", cfg.Scenario)
	}

	log.Printf("Injecting failure: %s", scenario.Name())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := scenario.Inject(ctx); err != nil {
		log.Printf("Warning: failed to inject failure: %v", err)
	}
	cancel()

	// Main test phase: continue submitting jobs during failure
	log.Printf("Test phase: %v", cfg.Duration)
	result.JobsSubmitted += submitJobs(cfg, cfg.Duration)

	// Recover from failure
	log.Printf("Recovering from failure")
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	if err := scenario.Recover(ctx); err != nil {
		log.Printf("Warning: failed to recover from failure: %v", err)
	}
	cancel()

	// Cooldown phase: wait for in-flight retries to settle
	log.Printf("Cooldown phase: %v", cfg.Cooldown)
	time.Sleep(cfg.Cooldown)

	result.EndTime = time.Now()
	result.Duration = result.EndTime.Sub(result.StartTime).Seconds()

	// Scrape metrics from Prometheus
	log.Println("Scraping metrics from Prometheus...")
	metrics, err := scrapeMetrics(cfg.Framework)
	if err != nil {
		log.Printf("Warning: failed to scrape metrics: %v", err)
	} else {
		result.Metrics = metrics
		if processed, ok := metrics["bench_processed_total"].(float64); ok {
			result.JobsProcessed = int(processed)
		}
		if retried, ok := metrics["bench_retry_total"].(float64); ok {
			result.JobsRetried = int(retried)
		}
		if lost, ok := metrics["bench_lost_total"].(float64); ok {
			result.JobsLost = int(lost)
		}
		if dupCalls, ok := metrics["bench_dup_http_calls_total"].(float64); ok {
			result.DupHTTPCalls = int(dupCalls)
		}
	}

	result.Throughput = float64(result.JobsProcessed) / result.Duration

	// Write report
	if err := writeReport(result, cfg.ReportPath); err != nil {
		log.Fatalf("Failed to write report: %v", err)
	}

	log.Printf("Benchmark complete. Report: %s", cfg.ReportPath)
	log.Printf("Results: processed=%d retried=%d lost=%d throughput=%.2f msg/sec",
		result.JobsProcessed, result.JobsRetried, result.JobsLost, result.Throughput)
}

func parseFlags() Config {
	framework := flag.String("framework", "rabbitmq", "Framework to benchmark (rabbitmq, kafka, nats, temporal, celery)")
	scenario := flag.String("scenario", "http-down", "Failure scenario (http-down, db-down, worker-crash)")
	duration := flag.Duration("duration", 2*time.Minute, "Test duration")
	rate := flag.Int("rate", 100, "Message submission rate (per second)")
	warmup := flag.Duration("warmup", 10*time.Second, "Warmup duration")
	cooldown := flag.Duration("cooldown", 20*time.Second, "Cooldown duration (wait for retries)")
	reportPath := flag.String("report", "results/benchmark.json", "Report output path")
	downstreamURL := flag.String("downstream", "http://localhost:8080", "Downstream HTTP sink URL")

	flag.Parse()

	os.MkdirAll("results", 0755)

	return Config{
		Framework:     *framework,
		Scenario:      *scenario,
		Duration:      *duration,
		Rate:          *rate,
		Warmup:        *warmup,
		Cooldown:      *cooldown,
		ReportPath:    *reportPath,
		DownstreamURL: *downstreamURL,
	}
}

func waitForDownstream(url string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("%s/health", url))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusServiceUnavailable {
				return true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return false
}

func submitJobs(cfg Config, duration time.Duration) int {
	ticker := time.NewTicker(time.Duration(float64(time.Second) / float64(cfg.Rate)))
	defer ticker.Stop()

	deadline := time.Now().Add(duration)
	count := 0

	for time.Now().Before(deadline) {
		select {
		case <-ticker.C:
			go func() {
				job := SinkRequest{
					ID:        uuid.New().String(),
					Payload:   []byte("test-payload"),
					Attempt:   1,
					Timestamp: time.Now().UnixMilli(),
				}
				submitJob(cfg.DownstreamURL, job)
			}()
			count++
		default:
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	return count
}

func submitJob(url string, job SinkRequest) error {
	body, err := json.Marshal(job)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/sink", url), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return nil
}

func getScenario(cfg Config) scenarios.Scenario {
	switch cfg.Scenario {
	case "http-down":
		return scenarios.NewHTTPDownScenario()
	case "db-down":
		return scenarios.NewDBDownScenario()
	case "worker-crash":
		return scenarios.NewWorkerCrashScenario(cfg.Framework)
	default:
		return nil
	}
}

func writeReport(result *Result, path string) error {
	os.MkdirAll(filepath.Dir(path), 0755)

	// Write JSON report
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		return err
	}

	// Write Markdown report
	mdPath := path[:len(path)-5] + ".md" // Replace .json with .md
	mdFile, err := os.Create(mdPath)
	if err != nil {
		return err
	}
	defer mdFile.Close()

	if err := writeMarkdownReport(mdFile, result); err != nil {
		return err
	}

	return nil
}

func writeMarkdownReport(w io.Writer, result *Result) error {
	fmt.Fprintf(w, "# Benchmark Report: %s\n\n", result.Framework)
	fmt.Fprintf(w, "## Test Parameters\n")
	fmt.Fprintf(w, "- **Framework**: %s\n", result.Framework)
	fmt.Fprintf(w, "- **Scenario**: %s\n", result.Scenario)
	fmt.Fprintf(w, "- **Duration**: %.2f seconds\n", result.Duration)
	fmt.Fprintf(w, "- **Test Window**: %s to %s\n", result.StartTime.Format("2006-01-02 15:04:05"), result.EndTime.Format("15:04:05"))

	fmt.Fprintf(w, "\n## Results\n")
	fmt.Fprintf(w, "- **Jobs Submitted**: %d\n", result.JobsSubmitted)
	fmt.Fprintf(w, "- **Jobs Processed**: %d\n", result.JobsProcessed)
	fmt.Fprintf(w, "- **Jobs Retried**: %d\n", result.JobsRetried)
	fmt.Fprintf(w, "- **Jobs Lost**: %d\n", result.JobsLost)
	fmt.Fprintf(w, "- **Duplicate HTTP Calls**: %d\n", result.DupHTTPCalls)
	fmt.Fprintf(w, "- **Throughput**: %.2f msg/sec\n", result.Throughput)

	fmt.Fprintf(w, "\n## Latency\n")
	fmt.Fprintf(w, "| Metric | Value (seconds) |\n")
	fmt.Fprintf(w, "|--------|--------|\n")
	fmt.Fprintf(w, "| Min | %.4f |\n", result.Latency.Min)
	fmt.Fprintf(w, "| Max | %.4f |\n", result.Latency.Max)
	fmt.Fprintf(w, "| Mean | %.4f |\n", result.Latency.Mean)
	fmt.Fprintf(w, "| p50 | %.4f |\n", result.Latency.P50)
	fmt.Fprintf(w, "| p95 | %.4f |\n", result.Latency.P95)
	fmt.Fprintf(w, "| p99 | %.4f |\n", result.Latency.P99)

	return nil
}
