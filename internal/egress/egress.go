package egress

import (
	"context"
	"fmt"
	"time"
)

type Mode string

const (
	ModeDirect Mode = "direct"
	ModeProxy  Mode = "proxy"
)

type Protocol string

const (
	ProtocolSOCKS5 Protocol = "socks5"
	ProtocolHTTP   Protocol = "http"
)

type Spec struct {
	Mode     Mode     `json:"mode,omitempty"`
	Provider string   `json:"provider,omitempty"`
	Protocol Protocol `json:"protocol,omitempty"`
}

func (s Spec) IsProxy() bool { return s.Mode == ModeProxy }

func (s Spec) Normalize() Spec {
	if s.Mode == "" {
		s.Mode = ModeDirect
	}
	if s.Mode == ModeProxy && s.Protocol == "" {
		s.Protocol = ProtocolSOCKS5
	}
	return s
}

func (s Spec) Validate() error {
	s = s.Normalize()
	switch s.Mode {
	case ModeDirect:
		// a provider or protocol on a direct spec means the caller believes
		// they asked for proxying and did not. rejecting is the only answer
		// that does not silently hand them direct egress.
		if s.Provider != "" {
			return fmt.Errorf("egress: provider %q requires mode %q", s.Provider, ModeProxy)
		}
		if s.Protocol != "" {
			return fmt.Errorf("egress: protocol %q requires mode %q", s.Protocol, ModeProxy)
		}
		return nil
	case ModeProxy:
		if s.Provider == "" {
			return fmt.Errorf("egress: mode %q requires a provider", ModeProxy)
		}
		switch s.Protocol {
		case ProtocolSOCKS5, ProtocolHTTP:
			return nil
		default:
			return fmt.Errorf("egress: unknown protocol %q (want %q or %q)", s.Protocol, ProtocolSOCKS5, ProtocolHTTP)
		}
	default:
		return fmt.Errorf("egress: unknown mode %q (want %q or %q)", s.Mode, ModeDirect, ModeProxy)
	}
}

type Context struct {
	VMID     string
	HostID   string
	ListenIP string
	GuestIP  string
	Protocol Protocol
	Host     HostAgent
}

type HostAgent interface {
	ProvisionEgress(ctx context.Context, vmID string, req HostRequest) (Endpoint, error)
	DestroyEgress(ctx context.Context, vmID string) error
}

type HostRequest struct {
	Provider string            `json:"provider"`
	Protocol Protocol          `json:"protocol"`
	ListenIP string            `json:"listen_ip"`
	GuestIP  string            `json:"guest_ip"`
	Config   map[string]string `json:"config,omitempty"`
}

type Endpoint struct {
	URL      string   `json:"url"`
	Protocol Protocol `json:"protocol"`
	Provider string   `json:"provider"`
}

type HealthState string

const (
	HealthUnknown   HealthState = "unknown"
	HealthHealthy   HealthState = "healthy"
	HealthUnhealthy HealthState = "unhealthy"
)

type Health struct {
	State     HealthState `json:"state"`
	Reason    string      `json:"reason,omitempty"`
	CheckedAt time.Time   `json:"checked_at"`
}

// is useless to anyone not already inside the sandbox.
type Status struct {
	Mode     Mode     `json:"mode"`
	Provider string   `json:"provider,omitempty"`
	Protocol Protocol `json:"protocol,omitempty"`
	Endpoint string   `json:"endpoint,omitempty"`
	Health   Health   `json:"health"`
}

// Provider is the whole backend contract. cloudflare warp is one registered
// implementation of it and not the abstraction.
//
// Healthcheck returns an error rather than a bool because the reason is what
// surfaces on the degraded environment, and false has no reason.
type Provider interface {
	Name() string
	Provision(ctx context.Context, ec Context) (Endpoint, error)
	Healthcheck(ctx context.Context, ep Endpoint) error
	Destroy(ctx context.Context, ec Context) error
}
