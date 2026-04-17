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
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/segmentio/kafka-go"

	"github.com/reivosar/at-least-once-bench/bench/scenarios"
	"github.com/reivosar/at-least-once-bench/shared/proto"
)

type Config struct {
	Framework      string
	Scenario       string
	Duration       time.Duration
	Rate           int // messages per second
	Warmup         time.Duration
	Cooldown       time.Duration
	ReportPath     string
	BrokerAddr     string
	BrokerUser     string
	BrokerPassword string
}

type Result struct {
	Framework     string                 `json:"framework"`
	Scenario      string                 `json:"scenario"`
	StartTime     time.Time              `json:"start_time"`
	EndTime       time.Time              `json:"end_time"`
	Duration      float64                `json:"duration_seconds"`
	JobsSubmitted int                    `json:"jobs_submitted"`
	JobsProcessed int                    `json:"jobs_processed"`
	JobsRetried   int                    `json:"jobs_retried"`
	JobsLost      int                    `json:"jobs_lost"`
	DupHTTPCalls  int                    `json:"duplicate_http_calls"`
	Throughput    float64                `json:"throughput_msg_sec"`
	Latency       LatencyStats           `json:"latency_seconds"`
	Metrics       map[string]interface{} `json:"metrics"`
}

type LatencyStats struct {
	Min  float64 `json:"min"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
	P50  float64 `json:"p50"`
	P95  float64 `json:"p95"`
	P99  float64 `json:"p99"`
}

// Publisher publishes proto.Job to the appropriate message broker.
type Publisher interface {
	Connect() error
	Publish(ctx context.Context, job proto.Job) error
	Close() error
}

// --- RabbitMQ Publisher ---

type RabbitMQPublisher struct {
	addr     string
	user     string
	password string
	conn     *amqp.Connection
	ch       *amqp.Channel
}

func (p *RabbitMQPublisher) Connect() error {
	dsn := fmt.Sprintf("amqp://%s:%s@%s/", p.user, p.password, p.addr)
	var err error
	for i := 0; i < 30; i++ {
		p.conn, err = amqp.Dial(dsn)
		if err == nil {
			break
		}
		log.Printf("RabbitMQ connection attempt %d failed: %v", i+1, err)
		time.Sleep(time.Second)
	}
	if err != nil {
		return fmt.Errorf("failed to connect to RabbitMQ: %w", err)
	}

	p.ch, err = p.conn.Channel()
	if err != nil {
		return fmt.Errorf("failed to open channel: %w", err)
	}

	// Declare DLX
	err = p.ch.ExchangeDeclare("bench-dlx", "direct", true, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("failed to declare DLX: %w", err)
	}

	// Declare queue matching worker's declaration (quorum queue with DLX)
	_, err = p.ch.QueueDeclare("bench-jobs", true, false, false, false, amqp.Table{
		"x-queue-type":           "quorum",
		"x-dead-letter-exchange": "bench-dlx",
		"x-max-length":           1000000,
	})
	if err != nil {
		return fmt.Errorf("failed to declare queue: %w", err)
	}

	return nil
}

func (p *RabbitMQPublisher) Publish(ctx context.Context, job proto.Job) error {
	body, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return p.ch.PublishWithContext(ctx, "", "bench-jobs", false, false, amqp.Publishing{
		ContentType:  "application/json",
		Body:         body,
		DeliveryMode: amqp.Persistent,
	})
}

func (p *RabbitMQPublisher) Close() error {
	if p.ch != nil {
		p.ch.Close()
	}
	if p.conn != nil {
		p.conn.Close()
	}
	return nil
}

// --- NATS JetStream Publisher ---

type NATSPublisher struct {
	addr string
	nc   *nats.Conn
	js   nats.JetStreamContext
}

func (p *NATSPublisher) Connect() error {
	var err error
	for i := 0; i < 30; i++ {
		p.nc, err = nats.Connect(fmt.Sprintf("nats://%s", p.addr))
		if err == nil {
			break
		}
		log.Printf("NATS connection attempt %d failed: %v", i+1, err)
		time.Sleep(time.Second)
	}
	if err != nil {
		return fmt.Errorf("failed to connect to NATS: %w", err)
	}

	p.js, err = p.nc.JetStream()
	if err != nil {
		return fmt.Errorf("failed to get JetStream context: %w", err)
	}

	// Create or reuse the stream (must match worker's config)
	_, err = p.js.StreamInfo("bench-jobs")
	if err == nats.ErrStreamNotFound {
		_, err = p.js.AddStream(&nats.StreamConfig{
			Name:     "bench-jobs",
			Subjects: []string{"jobs"},
			Storage:  nats.FileStorage,
			MaxAge:   24 * time.Hour,
		})
		if err != nil {
			return fmt.Errorf("failed to create stream: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to get stream info: %w", err)
	}

	return nil
}

func (p *NATSPublisher) Publish(ctx context.Context, job proto.Job) error {
	body, err := json.Marshal(job)
	if err != nil {
		return err
	}
	_, err = p.js.Publish("jobs", body)
	return err
}

func (p *NATSPublisher) Close() error {
	if p.nc != nil {
		p.nc.Close()
	}
	return nil
}

// --- Kafka Publisher ---

type KafkaPublisher struct {
	addr   string
	writer *kafka.Writer
}

func (p *KafkaPublisher) Connect() error {
	// Ensure topic exists
	for i := 0; i < 30; i++ {
		conn, err := kafka.Dial("tcp", p.addr)
		if err != nil {
			log.Printf("Kafka connection attempt %d failed: %v", i+1, err)
			time.Sleep(time.Second)
			continue
		}
		err = conn.CreateTopics(kafka.TopicConfig{
			Topic:             "bench-jobs",
			NumPartitions:     1,
			ReplicationFactor: 1,
		})
		conn.Close()
		if err != nil && !strings.Contains(err.Error(), "topic already exists") {
			log.Printf("Kafka topic creation attempt %d failed: %v", i+1, err)
			time.Sleep(time.Second)
			continue
		}
		break
	}

	p.writer = &kafka.Writer{
		Addr:     kafka.TCP(p.addr),
		Topic:    "bench-jobs",
		Balancer: &kafka.LeastBytes{},
	}
	return nil
}

func (p *KafkaPublisher) Publish(ctx context.Context, job proto.Job) error {
	body, err := json.Marshal(job)
	if err != nil {
		return err
	}
	return p.writer.WriteMessages(ctx, kafka.Message{
		Key:   []byte(job.ID),
		Value: body,
	})
}

func (p *KafkaPublisher) Close() error {
	if p.writer != nil {
		return p.writer.Close()
	}
	return nil
}

// --- Temporal Publisher (HTTP workflow starter) ---

type TemporalPublisher struct {
	addr string
}

func (p *TemporalPublisher) Connect() error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://%s/start-workflow", p.addr))
		if err == nil {
			resp.Body.Close()
			// Any response (even 405 Method Not Allowed) means the server is up
			return nil
		}
		log.Printf("Temporal workflow starter connection attempt failed, retrying...")
		time.Sleep(time.Second)
	}
	return fmt.Errorf("temporal workflow starter not reachable at %s", p.addr)
}

func (p *TemporalPublisher) Publish(ctx context.Context, job proto.Job) error {
	body, err := json.Marshal(job)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://%s/start-workflow", p.addr), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("workflow start failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (p *TemporalPublisher) Close() error {
	return nil
}

// --- Factory ---

func newPublisher(cfg Config) (Publisher, error) {
	switch cfg.Framework {
	case "rabbitmq":
		return &RabbitMQPublisher{
			addr:     cfg.BrokerAddr,
			user:     cfg.BrokerUser,
			password: cfg.BrokerPassword,
		}, nil
	case "nats":
		return &NATSPublisher{addr: cfg.BrokerAddr}, nil
	case "kafka":
		return &KafkaPublisher{addr: cfg.BrokerAddr}, nil
	case "temporal":
		return &TemporalPublisher{addr: cfg.BrokerAddr}, nil
	default:
		return nil, fmt.Errorf("unsupported framework: %s", cfg.Framework)
	}
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

	// Connect to message broker
	pub, err := newPublisher(cfg)
	if err != nil {
		log.Fatalf("Failed to create publisher: %v", err)
	}
	if err := pub.Connect(); err != nil {
		log.Fatalf("Failed to connect to broker: %v", err)
	}
	defer pub.Close()
	log.Printf("Connected to %s broker at %s", cfg.Framework, cfg.BrokerAddr)

	// Warmup phase: submit jobs at specified rate
	log.Printf("Warmup phase: %v", cfg.Warmup)
	result.JobsSubmitted = submitJobs(cfg, pub, cfg.Warmup)

	// Wait a bit for initial processing
	time.Sleep(2 * time.Second)

	// Snapshot baseline metrics before failure injection
	baseline, err := snapshotCounters(cfg.Framework)
	if err != nil {
		log.Printf("Warning: failed to snapshot baseline metrics: %v", err)
	}
	log.Printf("Baseline metrics: processed=%.0f retry=%.0f lost=%.0f dup=%.0f",
		baseline.ProcessedTotal, baseline.RetryTotal, baseline.LostTotal, baseline.DupHTTPTotal)

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
	result.JobsSubmitted += submitJobs(cfg, pub, cfg.Duration)

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

	// Scrape metrics from Prometheus and compute deltas from baseline
	log.Println("Scraping metrics from Prometheus...")
	metrics, err := scrapeMetrics(cfg.Framework)
	if err != nil {
		log.Printf("Warning: failed to scrape metrics: %v", err)
	} else {
		result.Metrics = metrics
		if processed, ok := metrics["bench_processed_total"].(float64); ok {
			result.JobsProcessed = int(computeDelta(processed, baseline.ProcessedTotal))
		}
		if retried, ok := metrics["bench_retry_total"].(float64); ok {
			result.JobsRetried = int(computeDelta(retried, baseline.RetryTotal))
		}
		if lost, ok := metrics["bench_lost_total"].(float64); ok {
			result.JobsLost = int(computeDelta(lost, baseline.LostTotal))
		}
		if dupCalls, ok := metrics["bench_dup_http_calls_total"].(float64); ok {
			result.DupHTTPCalls = int(computeDelta(dupCalls, baseline.DupHTTPTotal))
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
	framework := flag.String("framework", "rabbitmq", "Framework to benchmark (rabbitmq, kafka, nats, temporal)")
	scenario := flag.String("scenario", "http-down", "Failure scenario (http-down, db-down, worker-crash)")
	duration := flag.Duration("duration", 2*time.Minute, "Test duration")
	rate := flag.Int("rate", 100, "Message submission rate (per second)")
	warmup := flag.Duration("warmup", 10*time.Second, "Warmup duration")
	cooldown := flag.Duration("cooldown", 20*time.Second, "Cooldown duration (wait for retries)")
	reportPath := flag.String("report", "results/benchmark.json", "Report output path")
	brokerAddr := flag.String("broker", "", "Broker address (auto-detected per framework if empty)")
	brokerUser := flag.String("broker-user", "bench", "Broker user (RabbitMQ)")
	brokerPassword := flag.String("broker-password", "benchpass", "Broker password (RabbitMQ)")

	flag.Parse()

	os.MkdirAll("results", 0755)

	cfg := Config{
		Framework:      *framework,
		Scenario:       *scenario,
		Duration:       *duration,
		Rate:           *rate,
		Warmup:         *warmup,
		Cooldown:       *cooldown,
		ReportPath:     *reportPath,
		BrokerAddr:     *brokerAddr,
		BrokerUser:     *brokerUser,
		BrokerPassword: *brokerPassword,
	}

	// Auto-detect broker address if not specified
	if cfg.BrokerAddr == "" {
		switch cfg.Framework {
		case "rabbitmq":
			cfg.BrokerAddr = "localhost:5672"
		case "nats":
			cfg.BrokerAddr = "localhost:4222"
		case "kafka":
			cfg.BrokerAddr = "localhost:9092"
		case "temporal":
			cfg.BrokerAddr = "localhost:7234"
		}
	}

	return cfg
}

func submitJobs(cfg Config, pub Publisher, duration time.Duration) int {
	ticker := time.NewTicker(time.Duration(float64(time.Second) / float64(cfg.Rate)))
	defer ticker.Stop()

	deadline := time.Now().Add(duration)
	count := 0

	for time.Now().Before(deadline) {
		select {
		case <-ticker.C:
			job := proto.Job{
				ID:          uuid.New().String(),
				Payload:     []byte("test-payload"),
				Attempt:     1,
				SubmittedAt: time.Now(),
			}
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := pub.Publish(ctx, job); err != nil {
					log.Printf("Failed to publish job: %v", err)
				}
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

func getScenario(cfg Config) scenarios.Scenario {
	switch cfg.Scenario {
	case "http-down":
		return scenarios.NewHTTPDownScenario(cfg.Framework)
	case "db-down":
		return scenarios.NewDBDownScenario(cfg.Framework)
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

// computeDelta calculates the difference between end and baseline counter values.
// If end < baseline (counter was reset, e.g. after worker-crash), use end as-is.
func computeDelta(end, baseline float64) float64 {
	delta := end - baseline
	if delta < 0 {
		return end
	}
	return delta
}
