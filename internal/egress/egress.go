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
	// Secret names the environment secret that carries the provider's
	// credential (wireguard's base64 config). it is a name, never a value:
	// the value is resolved from the create request's secrets at provision
	// time and handed to the provider alone, never to the guest.
	Secret string `json:"secret,omitempty"`
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
		if s.Secret != "" {
			return fmt.Errorf("egress: secret %q requires mode %q", s.Secret, ModeProxy)
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
	// Credential is the resolved value of Spec.Secret, empty when the spec
	// names none. a provider that takes no credential refuses a non-empty
	// one rather than ignoring it.
	Credential string
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
	// Lease is what a backend that keeps per-vm state in memory needs to
	// take it back after a restart (wireguard: the config's fingerprint).
	// never a credential.
	Lease string `json:"lease,omitempty"`
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

// Status is the resolved policy as echoed back on an environment: never a
// credential, and the endpoint only because it is a host-local address that
// is useless to anyone not already inside the sandbox. Health is live,
// refreshed by the orchestrator's reconcile pass and never persisted.
type Status struct {
	Mode     Mode     `json:"mode"`
	Provider string   `json:"provider,omitempty"`
	Protocol Protocol `json:"protocol,omitempty"`
	Endpoint string   `json:"endpoint,omitempty"`
	// Lease is the endpoint's, persisted with the vm so a restarted
	// orchestrator can hand it back to the backend through Restorer.
	Lease  string `json:"lease,omitempty"`
	Health Health `json:"health"`
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

// Restorer is a Provider that keeps per-vm state in memory and takes it back
// from a vm's persisted lease when the orchestrator restarts. Forget drops
// that state for a vm that is gone, with no host call.
type Restorer interface {
	Restore(hostID, vmID, lease string) error
	Forget(vmID string)
}
