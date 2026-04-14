package scenarios

import (
	"context"
	"log"
)

// HTTPDownScenario simulates the downstream HTTP server being unreachable.
// It stops the nginx proxy for downstream specific to the framework.
type HTTPDownScenario struct {
	BaseScenario
	framework string
}

func NewHTTPDownScenario(framework string) *HTTPDownScenario {
	return &HTTPDownScenario{
		BaseScenario: BaseScenario{name: "http-down"},
		framework:    framework,
	}
}

func (s *HTTPDownScenario) Inject(ctx context.Context) error {
	nginxContainer := GetNginxContainerName(s.framework, "downstream")
	log.Printf("Injecting http-down scenario: stopping %s", nginxContainer)
	return DockerStop(nginxContainer)
}

func (s *HTTPDownScenario) Recover(ctx context.Context) error {
	nginxContainer := GetNginxContainerName(s.framework, "downstream")
	log.Printf("Recovering http-down scenario: starting %s", nginxContainer)
	return DockerStart(nginxContainer)
}
