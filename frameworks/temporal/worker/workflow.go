package main

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/reivosar/at-least-once-bench/shared/proto"
)

// ProcessJobWorkflow is the Temporal workflow for processing a job
func ProcessJobWorkflow(ctx workflow.Context, job proto.Job) error {
	// Configure activity options with retries
	retryPolicy := &temporal.RetryPolicy{
		InitialInterval:    time.Second,
		BackoffCoefficient: 2.0,
		MaximumInterval:    time.Minute,
		MaximumAttempts:    5,
	}

	options := workflow.ActivityOptions{
		RetryPolicy:            retryPolicy,
		ScheduleToCloseTimeout: 5 * time.Minute,
		StartToCloseTimeout:    5 * time.Minute,
		HeartbeatTimeout:       10 * time.Second,
	}
	ctx = workflow.WithActivityOptions(ctx, options)

	// Call activity to process the job
	var result ProcessJobResult
	err := workflow.ExecuteActivity(ctx, ProcessJobActivity, job).Get(ctx, &result)
	if err != nil {
		return err
	}

	if !result.Success {
		return fmt.Errorf("failed to process job")
	}

	return nil
}

// ProcessJobResult is the result of processing a job
type ProcessJobResult struct {
	Success   bool
	Processed bool
	Retries   int
}
