package hitl

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	// KindExec is an sdk/cli exec held by an exec gate.
	KindExec Kind = "exec"
	// KindLifecycle is an environment api action held by a lifecycle gate.
	KindLifecycle Kind = "lifecycle"
)

// Mode is what a gate does when it matches.
type Mode string

const (
	// ModeAsk holds the action until a human decides.
	ModeAsk Mode = "ask"
	// ModeNotify lets the action through and records it.
	ModeNotify Mode = "notify"
	// ModeDeny blocks the action with the gate's reason, no human involved.
	ModeDeny Mode = "deny"
)

// LifecycleAction is an environment api action a lifecycle gate can hold.
type LifecycleAction string

const (
	ActionDestroy     LifecycleAction = "destroy"
	ActionFork        LifecycleAction = "fork"
	ActionMigrate     LifecycleAction = "migrate"
	ActionDrain       LifecycleAction = "drain"
	ActionSnapshot    LifecycleAction = "snapshot"
	ActionRotateToken LifecycleAction = "rotate-token"
)

// LifecycleActions is every gateable action, in display order.
var LifecycleActions = []LifecycleAction{ActionDestroy, ActionFork, ActionMigrate, ActionDrain, ActionSnapshot, ActionRotateToken}

// Gate is one enforced gate from the fusefile.
type Gate struct {
	// Name identifies the gate on interventions and grants.
	Name string `json:"name"`
	// On is the surface: exec or lifecycle.
	On Kind `json:"on"`
	// Match selects the actions the gate holds.
	Match GateMatch `json:"match"`
	// Mode is ask (the default), notify or deny.
	Mode Mode `json:"mode,omitempty"`
	// Grant is the default grant an approval creates: once (the default),
	// env, or ttl:<duration>. only meaningful for ask.
	Grant string `json:"grant,omitempty"`
	// TimeoutSeconds overrides the spec's deadline for this gate.
	TimeoutSeconds int64 `json:"timeout_seconds,omitempty"`
	// Reason is the text a deny returns, and is shown to the human on ask.
	Reason string `json:"reason,omitempty"`
}

// GateMatch is the structured match. every field set must match.
type GateMatch struct {
	// Program matches the basename of argv[0] after known wrappers are
	// stripped, against any entry of the list.
	Program []string `json:"program,omitempty"`
	// Args matches when they appear in argv[1:] in order, not
	// necessarily next to each other.
	Args []string `json:"args,omitempty"`
	// Shell matches any opaque shell exec.
	Shell bool `json:"shell,omitempty"`
	// Action matches lifecycle actions.
	Action []LifecycleAction `json:"action,omitempty"`
}

// ExecPolicy is allowlist mode: an exec whose program is not in Allow is
// held with Default, interpreters and shells included.
type ExecPolicy struct {
	// Default is ask or deny.
	Default Mode `json:"default"`
	// Allow lists program basenames that run without a gate.
	Allow []string `json:"allow"`
}

// AllowlistGate is the name the allowlist reports on interventions.
const AllowlistGate = "exec.allowlist"

// EffectiveMode returns the gate's mode with its default applied.
func (g Gate) EffectiveMode() Mode {
	if g.Mode == "" {
		return ModeAsk
	}
	return g.Mode
}

// Timeout returns the gate's deadline duration, falling back to the spec.
func (g Gate) Timeout(spec Spec) time.Duration {
	if g.TimeoutSeconds > 0 {
		return time.Duration(g.TimeoutSeconds) * time.Second
	}
	return spec.Timeout()
}

// DenyReason is the text a deny of this gate returns.
func (g Gate) DenyReason() string {
	if strings.TrimSpace(g.Reason) != "" {
		return g.Reason
	}
	return "denied by hitl gate " + g.Name
}

// FindGate returns the named gate, including the synthetic allowlist gate.
func (s Spec) FindGate(name string) (Gate, bool) {
	if name == AllowlistGate && s.Exec != nil {
		return s.allowlistGate(), true
	}
	for _, g := range s.Gates {
		if g.Name == name {
			return g, true
		}
	}
	return Gate{}, false
}

func (s Spec) allowlistGate() Gate {
	return Gate{
		Name:   AllowlistGate,
		On:     KindExec,
		Mode:   s.Exec.Default,
		Reason: "program is not in hitl.exec.allow",
	}
}

const (
	maxGates      = 64
	maxMatchItems = 64
	maxGateReason = 1024
)

var gateNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

func validateGates(gates []Gate, exec *ExecPolicy) []error {
	var errs []error
	if len(gates) > maxGates {
		errs = append(errs, fmt.Errorf("hitl.gates: at most %d gates are allowed", maxGates))
	}
	seen := make(map[string]bool, len(gates))
	for i, g := range gates {
		at := fmt.Sprintf("hitl.gates[%d]", i)
		switch {
		case !gateNameRE.MatchString(g.Name):
			errs = append(errs, fmt.Errorf("%s.name: must be 1-64 lowercase letters, digits, '-', '_' or '.', got %q", at, g.Name))
		case g.Name == AllowlistGate:
			errs = append(errs, fmt.Errorf("%s.name: %q is reserved for hitl.exec", at, g.Name))
		case seen[g.Name]:
			errs = append(errs, fmt.Errorf("%s.name: %q is a duplicate", at, g.Name))
		}
		seen[g.Name] = true
		errs = append(errs, validateMatch(at, g.On, g.Match)...)
		switch g.Mode {
		case "", ModeAsk, ModeNotify, ModeDeny:
		default:
			errs = append(errs, fmt.Errorf("%s.mode: must be ask, notify or deny, got %q", at, g.Mode))
		}
		if g.Grant != "" {
			if _, err := ParseGrant(g.Grant); err != nil {
				errs = append(errs, fmt.Errorf("%s.grant: %w", at, err))
			} else if g.EffectiveMode() != ModeAsk {
				errs = append(errs, fmt.Errorf("%s.grant: only an ask gate can grant", at))
			}
		}
		if g.TimeoutSeconds < 0 {
			errs = append(errs, fmt.Errorf("%s.timeout_seconds: must not be negative", at))
		} else if g.TimeoutSeconds > 0 {
			if err := ValidateTimeout(time.Duration(g.TimeoutSeconds) * time.Second); err != nil {
				errs = append(errs, fmt.Errorf("%s.timeout_seconds: %w", at, err))
			}
		}
		if len(g.Reason) > maxGateReason {
			errs = append(errs, fmt.Errorf("%s.reason: must be at most %d bytes", at, maxGateReason))
		}
	}
	if exec != nil {
		if exec.Default != ModeAsk && exec.Default != ModeDeny {
			errs = append(errs, fmt.Errorf("hitl.exec.default: must be ask or deny, got %q", exec.Default))
		}
		errs = append(errs, validatePrograms("hitl.exec.allow", exec.Allow)...)
	}
	return errs
}

func validateMatch(at string, on Kind, m GateMatch) []error {
	var errs []error
	switch on {
	case KindExec:
		if len(m.Action) > 0 {
			errs = append(errs, fmt.Errorf("%s.match.action: only applies to on: lifecycle", at))
		}
		if !m.Shell && len(m.Program) == 0 && len(m.Args) == 0 {
			errs = append(errs, fmt.Errorf("%s.match: an exec gate needs program, args or shell", at))
		}
		if m.Shell && (len(m.Program) > 0 || len(m.Args) > 0) {
			errs = append(errs, fmt.Errorf("%s.match.shell: a shell exec is opaque, so it cannot be combined with program or args", at))
		}
		errs = append(errs, validatePrograms(at+".match.program", m.Program)...)
		if len(m.Args) > maxMatchItems {
			errs = append(errs, fmt.Errorf("%s.match.args: at most %d entries", at, maxMatchItems))
		}
		for j, a := range m.Args {
			if a == "" {
				errs = append(errs, fmt.Errorf("%s.match.args[%d]: must not be empty", at, j))
			}
		}
	case KindLifecycle:
		if m.Shell || len(m.Program) > 0 || len(m.Args) > 0 {
			errs = append(errs, fmt.Errorf("%s.match: a lifecycle gate matches only action", at))
		}
		if len(m.Action) == 0 {
			errs = append(errs, fmt.Errorf("%s.match.action: list at least one of %s", at, joinActions()))
		}
		for j, a := range m.Action {
			if !slices.Contains(LifecycleActions, a) {
				errs = append(errs, fmt.Errorf("%s.match.action[%d]: must be one of %s, got %q", at, j, joinActions(), a))
			}
		}
	case "attach", "computer", "egress", "stall", "expiry":
		errs = append(errs, fmt.Errorf("%s.on: %q is not yet supported", at, on))
	default:
		errs = append(errs, fmt.Errorf("%s.on: must be exec or lifecycle, got %q", at, on))
	}
	return errs
}

func validatePrograms(at string, programs []string) []error {
	var errs []error
	if len(programs) > maxMatchItems {
		errs = append(errs, fmt.Errorf("%s: at most %d entries", at, maxMatchItems))
	}
	for j, p := range programs {
		if p == "" || strings.Contains(p, "/") {
			errs = append(errs, fmt.Errorf("%s[%d]: name a program basename like git, not a path, got %q", at, j, p))
		}
	}
	return errs
}

func joinActions() string {
	out := make([]string, len(LifecycleActions))
	for i, a := range LifecycleActions {
		out[i] = string(a)
	}
	return strings.Join(out, ", ")
}

// errNoPolicy is returned when a spec declares nothing at all.
var errNoPolicy = errors.New("hitl: declare at least one of requests.ask, gates or exec")
