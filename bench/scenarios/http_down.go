package scenarios

import (
	"context"
	"log"
)

// HTTPDownScenario simulates the downstream HTTP server being unreachable.
// It disconnects the "downstream" container from the bench-net network.
type HTTPDownScenario struct {
	BaseScenario
}

func NewHTTPDownScenario() *HTTPDownScenario {
	return &HTTPDownScenario{
		BaseScenario: BaseScenario{name: "http-down"},
	}
}

func (s *HTTPDownScenario) Inject(ctx context.Context) error {
	log.Println("Injecting http-down scenario: disconnecting downstream from network")
	return DockerNetworkDisconnect("downstream")
}

func (s *HTTPDownScenario) Recover(ctx context.Context) error {
	log.Println("Recovering http-down scenario: reconnecting downstream to network")
	return DockerNetworkConnect("downstream")
}
