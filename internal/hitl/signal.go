package hitl

import (
	"fmt"
	"time"
)

// signal kinds: fuse noticing on its own that the agent looks stuck, or
// that the environment is about to be reaped. declared as gates with
// `on: stall` and `on: expiry`.
const (
	KindStall  Kind = "stall"
	KindExpiry Kind = "expiry"
)

// Reaper names the window an expiry signal warns about.
type Reaper string

const (
	ReaperIdle       Reaper = "idle_timeout"
	ReaperMaxRuntime Reaper = "max_runtime"
)

const (
	maxStallFailures = 100
	// minSignalWindow keeps quiet and before above the reconcile cadence
	// that checks them.
	minSignalWindow = time.Minute
)

// SignalPayload is what fuse saw when it raised a signal.
type SignalPayload struct {
	// Failures is the run of consecutive non-zero exec exits (stall).
	Failures int `json:"failures,omitempty"`
	// LastExitCode is the exit code that completed the run (stall).
	LastExitCode int `json:"last_exit_code,omitempty"`
	// QuietSeconds is how long the agent made no exec or computer call
	// (stall).
	QuietSeconds int64 `json:"quiet_seconds,omitempty"`
	// Reaper and ReapAt say what will tear the environment down, and when
	// (expiry).
	Reaper Reaper     `json:"reaper,omitempty"`
	ReapAt *time.Time `json:"reap_at,omitempty"`
	// ExtendedUntil is set when a human approved an expiry: the reaper
	// leaves the environment alone until then.
	ExtendedUntil *time.Time `json:"extended_until,omitempty"`
}

// validateSignal checks a stall or expiry gate.
func validateSignal(at string, g Gate) []error {
	var errs []error
	m := g.Match
	if m.Shell || len(m.Program) > 0 || len(m.Args) > 0 || len(m.Action) > 0 {
		errs = append(errs, fmt.Errorf("%s.match: a %s gate matches only %s", at, g.On, signalFields(g.On)))
	}
	switch g.On {
	case KindStall:
		if m.BeforeSeconds != 0 {
			errs = append(errs, fmt.Errorf("%s.match.before: only applies to on: expiry", at))
		}
		if m.Failures == 0 && m.QuietSeconds == 0 {
			errs = append(errs, fmt.Errorf("%s.match: a stall gate needs failures or quiet", at))
		}
		if m.Failures < 0 || m.Failures > maxStallFailures {
			errs = append(errs, fmt.Errorf("%s.match.failures: must be between 1 and %d", at, maxStallFailures))
		}
		if m.QuietSeconds != 0 {
			errs = append(errs, validateWindow(at+".match.quiet", m.QuietSeconds)...)
		}
	case KindExpiry:
		if m.Failures != 0 || m.QuietSeconds != 0 {
			errs = append(errs, fmt.Errorf("%s.match: an expiry gate matches only before", at))
		}
		if m.BeforeSeconds == 0 {
			errs = append(errs, fmt.Errorf("%s.match.before: an expiry gate needs before", at))
		} else {
			errs = append(errs, validateWindow(at+".match.before", m.BeforeSeconds)...)
		}
	}
	if g.EffectiveMode() == ModeDeny {
		errs = append(errs, fmt.Errorf("%s.mode: a %s signal is ask or notify; there is no action to deny", at, g.On))
	}
	if g.Grant != "" {
		errs = append(errs, fmt.Errorf("%s.grant: a %s signal cannot grant", at, g.On))
	}
	return errs
}

func validateWindow(at string, secs int64) []error {
	d := time.Duration(secs) * time.Second
	if d < minSignalWindow || d > MaxTimeout {
		return []error{fmt.Errorf("%s: must be between %s and %s, got %s", at, minSignalWindow, MaxTimeout, d)}
	}
	return nil
}

func signalFields(on Kind) string {
	if on == KindStall {
		return "failures and quiet"
	}
	return "before"
}

// SignalGates returns the gates declared for one signal kind.
func (s Spec) SignalGates(k Kind) []Gate {
	var out []Gate
	for _, g := range s.Gates {
		if g.On == k {
			out = append(out, g)
		}
	}
	return out
}

// Extend approves a pending expiry, keeping the reaper off the environment
// until now+by.
func Extend(in Intervention, by time.Duration, decider Actor, reason string, now time.Time) (Intervention, error) {
	if in.Kind != KindExpiry {
		return in, fmt.Errorf("intervention %s is a %s, not an expiry", in.ID, in.Kind)
	}
	if by < minSignalWindow || by > MaxTimeout {
		return in, fmt.Errorf("extend must be between %s and %s, got %s", minSignalWindow, MaxTimeout, by)
	}
	if err := Transition(in.ID, in.State, StateApproved); err != nil {
		return in, err
	}
	if err := ValidateReason(reason); err != nil {
		return in, err
	}
	until := now.Add(by)
	sig := SignalPayload{}
	if in.Signal != nil {
		sig = *in.Signal
	}
	sig.ExtendedUntil = &until
	in.Signal = &sig
	in.State = StateApproved
	in.Decision = &Decision{
		Outcome:   OutcomeApprove,
		Answer:    "extended until " + until.UTC().Format(time.RFC3339),
		Reason:    reason,
		DecidedBy: decider,
		DecidedAt: now,
	}
	in.UpdatedAt = now
	return in, nil
}
