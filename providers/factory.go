package providers

import (
	"fmt"

	"github.com/andrewn6/fuse/firecracker"
	"github.com/andrewn6/fuse/internal/core"
)

// Kind identifies a sandbox provider implementation.
type Kind string

const (
	Firecracker Kind = "firecracker"
	Mock        Kind = "mock"
)

// Config holds provider-specific configs and a selected Kind.
type Config struct {
	Kind        Kind
	Firecracker firecracker.Config
}

// New constructs an orchestrator.Provider from Config. Defaults to Firecracker
// when Kind is empty. Caller must Close the returned provider.
func New(cfg Config) (orchestrator.Provider, error) {
	switch cfg.Kind {
	case "", Firecracker:
		return firecracker.New(cfg.Firecracker), nil
	case Mock:
		return nil, fmt.Errorf("mock provider not constructed here; use test mocks")
	default:
		return nil, fmt.Errorf("unknown provider kind: %s", cfg.Kind)
	}
}
