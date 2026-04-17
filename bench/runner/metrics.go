package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MetricsSnapshot holds the raw counter values at a point in time.
type MetricsSnapshot struct {
	ProcessedTotal float64
	RetryTotal     float64
	LostTotal      float64
	DupHTTPTotal   float64
}

// snapshotCounters captures the current values of all counter metrics for baseline calculation.
func snapshotCounters(framework string) (MetricsSnapshot, error) {
	prometheusURL := "http://localhost:9090"
	var snap MetricsSnapshot
	var err error

	snap.ProcessedTotal, err = queryMetric(prometheusURL, "bench_processed_total", framework)
	if err != nil {
		return snap, err
	}
	snap.RetryTotal, _ = queryMetric(prometheusURL, "bench_retry_total", framework)
	snap.LostTotal, _ = queryMetric(prometheusURL, "bench_lost_total", framework)
	snap.DupHTTPTotal, _ = queryMetric(prometheusURL, "bench_dup_http_calls_total", framework)

	return snap, nil
}

// scrapeMetrics queries Prometheus for the metrics of a specific framework.
func scrapeMetrics(framework string) (map[string]interface{}, error) {
	prometheusURL := "http://localhost:9090"
	metrics := make(map[string]interface{})

	metricNames := []string{
		"bench_processed_total",
		"bench_retry_total",
		"bench_lost_total",
		"bench_dup_http_calls_total",
	}

	for _, metricName := range metricNames {
		value, err := queryMetric(prometheusURL, metricName, framework)
		if err != nil {
			log.Printf("Warning: failed to query %s: %v", metricName, err)
			continue
		}
		metrics[metricName] = value
	}

	// Also try to get histogram quantiles for latency
	for _, q := range []string{"0.5", "0.95", "0.99"} {
		query := fmt.Sprintf(`histogram_quantile(%s, rate(bench_latency_seconds_bucket{job="%s-worker"}[5m]))`, q, framework)
		value, err := queryPrometheus(prometheusURL, query)
		if err != nil {
			log.Printf("Warning: failed to query latency quantile %s: %v", q, err)
			continue
		}
		metrics[fmt.Sprintf("latency_p%s", strings.ReplaceAll(q, ".", ""))] = value
	}

	return metrics, nil
}

// queryMetric queries a single metric's current value, filtered by framework.
func queryMetric(prometheusURL, metricName, framework string) (float64, error) {
	query := fmt.Sprintf(`%s{job="%s-worker"}`, metricName, framework)
	return queryPrometheus(prometheusURL, query)
}

// queryPrometheus sends a PromQL query to Prometheus and returns the first result.
func queryPrometheus(prometheusURL, query string) (float64, error) {
	// URL encode the query
	v := url.Values{}
	v.Set("query", query)

	queryURL := fmt.Sprintf("%s/api/v1/query?%s", prometheusURL, v.Encode())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, queryURL, nil)
	if err != nil {
		return 0, err
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("prometheus returned status %d", resp.StatusCode)
	}

	var result struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value [2]interface{} `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}

	if result.Status != "success" {
		return 0, fmt.Errorf("prometheus query failed: %s", result.Status)
	}

	if len(result.Data.Result) == 0 {
		return 0, nil // No data points
	}

	// Extract the numeric value
	valueStr, ok := result.Data.Result[0].Value[1].(string)
	if !ok {
		return 0, fmt.Errorf("unexpected value format")
	}

	value, err := strconv.ParseFloat(valueStr, 64)
	if err != nil {
		return 0, err
	}

	return value, nil
}
