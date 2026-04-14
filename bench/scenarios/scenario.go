package scenarios

import (
	"context"
	"fmt"
	"log"
	"os/exec"
	"time"
)

// Scenario defines the interface for failure injection scenarios.
type Scenario interface {
	// Name returns the scenario name (e.g., "http-down")
	Name() string

	// Inject applies the failure condition
	Inject(ctx context.Context) error

	// Recover removes the failure condition
	Recover(ctx context.Context) error
}

// BaseScenario provides common functionality for scenarios.
type BaseScenario struct {
	name string
}

func (s *BaseScenario) Name() string {
	return s.name
}

// DockerStop stops a container.
func DockerStop(containerName string) error {
	cmd := exec.Command("docker", "stop", containerName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("docker stop %s: %s", containerName, string(output))
		return fmt.Errorf("failed to stop %s: %w", containerName, err)
	}
	return nil
}

// DockerStart starts a stopped container.
func DockerStart(containerName string) error {
	cmd := exec.Command("docker", "start", containerName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("docker start %s: %s", containerName, string(output))
		return fmt.Errorf("failed to start %s: %w", containerName, err)
	}
	return nil
}

// DockerKillContainer sends SIGKILL to a container.
func DockerKillContainer(containerName string) error {
	cmd := exec.Command("docker", "kill", "--signal=SIGKILL", containerName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("docker kill %s: %s", containerName, string(output))
		return fmt.Errorf("failed to kill %s: %w", containerName, err)
	}
	return nil
}

// DockerRestartContainer restarts a stopped container.
func DockerRestartContainer(containerName string) error {
	cmd := exec.Command("docker", "restart", containerName)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("docker restart %s: %s", containerName, string(output))
		return fmt.Errorf("failed to restart %s: %w", containerName, err)
	}

	// Wait for container to be healthy (max 30s)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		cmd := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", containerName)
		output, err := cmd.CombinedOutput()
		if err == nil && string(output) == "true\n" {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}

	return fmt.Errorf("container %s did not restart within timeout", containerName)
}

// GetWorkerContainerName returns the worker container name for a framework.
func GetWorkerContainerName(framework string) string {
	return fmt.Sprintf("bench-%s-worker", framework)
}

// GetNginxContainerName returns the nginx container name for a framework and service.
func GetNginxContainerName(framework, service string) string {
	return fmt.Sprintf("bench-%s-nginx-%s", framework, service)
}
