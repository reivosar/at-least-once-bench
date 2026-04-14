package scenarios

import (
	"context"
	"log"
)

// DBDownScenario simulates the database being unreachable.
// It stops the nginx proxy for postgres specific to the framework.
// This tests the critical scenario where HTTP succeeds but DB write fails,
// causing over-delivery (duplicate HTTP calls) on retry.
type DBDownScenario struct {
	BaseScenario
	framework string
}

func NewDBDownScenario(framework string) *DBDownScenario {
	return &DBDownScenario{
		BaseScenario: BaseScenario{name: "db-down"},
		framework:    framework,
	}
}

func (s *DBDownScenario) Inject(ctx context.Context) error {
	nginxContainer := GetNginxContainerName(s.framework, "postgres")
	log.Printf("Injecting db-down scenario: stopping %s", nginxContainer)
	return DockerStop(nginxContainer)
}

func (s *DBDownScenario) Recover(ctx context.Context) error {
	nginxContainer := GetNginxContainerName(s.framework, "postgres")
	log.Printf("Recovering db-down scenario: starting %s", nginxContainer)
	return DockerStart(nginxContainer)
}
