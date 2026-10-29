// package hitl is the pure logic behind human-in-the-loop interventions:
// the policy an environment is created with, the intervention record, and
// the state machine that moves it. it does no i/o; the orchestrator owns
// storage, waiting and delivery.
package hitl

import (
	"errors"
	"fmt"
	"time"
)

// Kind is what raised an intervention. issue #309 only has the ask request;
// gates, the other requests and signals add kinds later.
type Kind string

const (
	KindAsk Kind = "ask"
)

// OnTimeout decides how an expired intervention resolves.
type OnTimeout string

const (
	OnTimeoutReject OnTimeout = "reject"
	OnTimeoutAllow  OnTimeout = "allow"
)

const (
	// DefaultTimeout is the deadline when the spec sets none.
	DefaultTimeout = 10 * time.Minute
	// MinTimeout keeps a deadline above the reconcile granularity that
	// backs it up.
	MinTimeout = 10 * time.Second
	// MaxTimeout bounds every wait. no unbounded waits, ever.
	MaxTimeout = 24 * time.Hour
)

// Spec is the hitl policy an environment is created with. it is compiled
// from the fusefile `hitl:` block, carried on the create request, stored on
// the vm and inherited by forks and migrates. nil means hitl is off.
type Spec struct {
	// TimeoutSeconds is the default deadline for every intervention. zero
	// means DefaultTimeout.
	TimeoutSeconds int64 `json:"timeout_seconds,omitempty"`
	// OnTimeout is how an expired intervention resolves. empty means reject.
	OnTimeout OnTimeout `json:"on_timeout,omitempty"`
	// Requests are the request kinds the agent may raise.
	Requests Requests `json:"requests"`
}

// Requests declares which request kinds are allowed. a nil entry is not
// declared and is refused at call time.
type Requests struct {
	Ask *AskRequests `json:"ask,omitempty"`
}

// AskRequests configures `ask` requests. it has no fields yet; declaring
// the block is what allows the kind.
type AskRequests struct{}

// Timeout returns the effective default deadline.
func (s Spec) Timeout() time.Duration {
	if s.TimeoutSeconds <= 0 {
		return DefaultTimeout
	}
	return time.Duration(s.TimeoutSeconds) * time.Second
}

// EffectiveOnTimeout returns the on_timeout with its default applied.
func (s Spec) EffectiveOnTimeout() OnTimeout {
	if s.OnTimeout == "" {
		return OnTimeoutReject
	}
	return s.OnTimeout
}

// Allows reports whether the spec declares the given request kind.
func (s Spec) Allows(k Kind) bool {
	switch k {
	case KindAsk:
		return s.Requests.Ask != nil
	}
	return false
}

// Validate checks the spec's invariants. the fusefile compiler applies the
// same rules client-side; this is the copy a raw api caller is held to.
func (s Spec) Validate() error {
	var errs []error
	if s.TimeoutSeconds < 0 {
		errs = append(errs, errors.New("hitl.timeout_seconds: must not be negative"))
	} else if s.TimeoutSeconds > 0 {
		if err := ValidateTimeout(time.Duration(s.TimeoutSeconds) * time.Second); err != nil {
			errs = append(errs, fmt.Errorf("hitl.timeout_seconds: %w", err))
		}
	}
	switch s.OnTimeout {
	case "", OnTimeoutReject, OnTimeoutAllow:
	default:
		errs = append(errs, fmt.Errorf("hitl.on_timeout: must be %q or %q, got %q", OnTimeoutReject, OnTimeoutAllow, s.OnTimeout))
	}
	if s.Requests.Ask == nil {
		errs = append(errs, errors.New("hitl.requests: declare at least one request kind (only ask is supported)"))
	}
	return errors.Join(errs...)
}

// ValidateTimeout checks a deadline duration against the bounds.
func ValidateTimeout(d time.Duration) error {
	if d < MinTimeout || d > MaxTimeout {
		return fmt.Errorf("must be between %s and %s, got %s", MinTimeout, MaxTimeout, d)
	}
	return nil
}
