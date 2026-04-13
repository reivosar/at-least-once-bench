package scenarios

import (
	"context"
	"log"
)

// WorkerCrashScenario simulates a worker process crashing and restarting.
// It sends SIGKILL to the framework-specific worker container, then restarts it.
type WorkerCrashScenario struct {
	BaseScenario
	framework string
}

func NewWorkerCrashScenario(framework string) *WorkerCrashScenario {
	return &WorkerCrashScenario{
		BaseScenario: BaseScenario{name: "worker-crash"},
		framework:    framework,
	}
}

func (s *WorkerCrashScenario) Inject(ctx context.Context) error {
	containerName := GetWorkerContainerName(s.framework)
	log.Printf("Injecting worker-crash scenario: killing %s", containerName)
	return DockerKillContainer(containerName)
}

func (s *WorkerCrashScenario) Recover(ctx context.Context) error {
	containerName := GetWorkerContainerName(s.framework)
	log.Printf("Recovering worker-crash scenario: restarting %s", containerName)
	return DockerRestartContainer(containerName)
}
