package providers

import (
	"fmt"

	"github.com/surf-dev/surf/apps/orchestrator/daytona"
	"github.com/surf-dev/surf/apps/orchestrator/firecracker"
	"github.com/surf-dev/surf/apps/orchestrator/internal/core"
)

// Kind identifies a sandbox provider implementation.
type Kind string

const (
	Firecracker Kind = "firecracker"
	Daytona     Kind = "daytona"
	Mock        Kind = "mock"
)

// Config holds provider-specific configs and a selected Kind.
type Config struct {
	Kind        Kind
	Firecracker firecracker.Config
	Daytona     daytona.Config
}

// New constructs an orchestrator.Provider from Config. Defaults to Firecracker
// when Kind is empty. Caller must Close the returned provider.
func New(cfg Config) (orchestrator.Provider, error) {
	switch cfg.Kind {
	case "", Firecracker:
		return firecracker.New(cfg.Firecracker), nil
	case Daytona:
		return daytona.New(cfg.Daytona), nil
	case Mock:
		return nil, fmt.Errorf("mock provider not constructed here; use test mocks")
	default:
		return nil, fmt.Errorf("unknown provider kind: %s", cfg.Kind)
	}
}
