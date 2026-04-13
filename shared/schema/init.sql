-- processed_jobs table: idempotency enforced by UNIQUE constraint on job_id
CREATE TABLE IF NOT EXISTS processed_jobs (
    id SERIAL PRIMARY KEY,
    job_id VARCHAR(36) NOT NULL UNIQUE,
    payload BYTEA NOT NULL,
    attempt INT NOT NULL,
    ts TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

-- Index for fast lookup by job_id
CREATE INDEX IF NOT EXISTS idx_processed_jobs_job_id ON processed_jobs(job_id);

-- Index for time-series queries
CREATE INDEX IF NOT EXISTS idx_processed_jobs_ts ON processed_jobs(ts DESC);
