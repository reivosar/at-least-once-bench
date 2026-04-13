package proto

import (
	"time"
)

// Job represents a unit of work in the benchmark.
type Job struct {
	ID          string    `json:"id"`           // UUID, stable across retries
	Payload     []byte    `json:"payload"`      // arbitrary data
	Attempt     int       `json:"attempt"`      // attempt counter (1-indexed)
	SubmittedAt time.Time `json:"submitted_at"` // timestamp for latency measurement
}
