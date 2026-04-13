package scenarios

import (
	"context"
	"log"
)

// DBDownScenario simulates the database being unreachable.
// It disconnects the "postgres" container from the bench-net network.
// This tests the critical scenario where HTTP succeeds but DB write fails,
// causing over-delivery (duplicate HTTP calls) on retry.
type DBDownScenario struct {
	BaseScenario
}

func NewDBDownScenario() *DBDownScenario {
	return &DBDownScenario{
		BaseScenario: BaseScenario{name: "db-down"},
	}
}

func (s *DBDownScenario) Inject(ctx context.Context) error {
	log.Println("Injecting db-down scenario: disconnecting postgres from network")
	return DockerNetworkDisconnect("postgres")
}

func (s *DBDownScenario) Recover(ctx context.Context) error {
	log.Println("Recovering db-down scenario: reconnecting postgres to network")
	return DockerNetworkConnect("postgres")
}
