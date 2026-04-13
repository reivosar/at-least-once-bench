package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"go.temporal.io/sdk/activity"

	"github.com/reivosar/at-least-once-bench/shared/proto"
)

type SinkRequest struct {
	ID        string `json:"id"`
	Payload   []byte `json:"payload"`
	Attempt   int    `json:"attempt"`
	Timestamp int64  `json:"timestamp"`
}

// ProcessJobActivity is the activity that processes a single job
func ProcessJobActivity(ctx context.Context, job proto.Job) (ProcessJobResult, error) {
	// Get activity info for retry tracking
	actInfo := activity.GetInfo(ctx)
	activityID := actInfo.ActivityID
	attempt := actInfo.Attempt

	// Heartbeat to show activity is still running
	activity.RecordHeartbeat(ctx)

	log.Printf("[Activity %s] Processing job %s (attempt %d)", activityID, job.ID, attempt)

	result := ProcessJobResult{
		Success:   false,
		Processed: false,
		Retries:   int(attempt) - 1,
	}

	// Check if job was already processed (idempotency)
	exists, err := jobExists(job.ID)
	if err != nil {
		log.Printf("[Activity %s] Failed to check job existence: %v", activityID, err)
		// Don't fail on this error; activity will be retried
		return result, err
	}

	if exists {
		// Job already processed
		result.Success = true
		result.Processed = true
		return result, nil
	}

	// Call downstream HTTP endpoint
	if !callDownstream(ctx, job) {
		log.Printf("[Activity %s] Downstream call failed, will retry", activityID)
		return result, fmt.Errorf("downstream call failed")
	}

	activity.RecordHeartbeat(ctx)

	// Insert into database
	if !insertJob(job) {
		log.Printf("[Activity %s] Database insert failed, will retry", activityID)
		return result, fmt.Errorf("database insert failed")
	}

	result.Success = true
	result.Processed = true

	log.Printf("[Activity %s] Successfully processed job %s", activityID, job.ID)
	return result, nil
}

func jobExists(jobID string) (bool, error) {
	var exists bool
	err := globalDB.QueryRow("SELECT EXISTS(SELECT 1 FROM processed_jobs WHERE job_id = $1)", jobID).Scan(&exists)
	if err != nil {
		return false, err
	}
	return exists, nil
}

func callDownstream(ctx context.Context, job proto.Job) bool {
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

	httpCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(httpCtx, http.MethodPost, fmt.Sprintf("%s/sink", globalDownstreamURL), bytes.NewReader(body))
	if err != nil {
		return false
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	benchDupHTTPCallsTotal.WithLabelValues().Inc()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func insertJob(job proto.Job) bool {
	// INSERT ... ON CONFLICT DO NOTHING for idempotency
	var inserted bool
	err := globalDB.QueryRow(`
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
