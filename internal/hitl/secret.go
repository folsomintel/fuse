package hitl

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

// kinds added for secrets, takeover and signals (#312). kept in their own
// block so the gate kinds (#311) merge beside them cleanly.
const (
	KindSecret   Kind = "secret"
	KindTakeover Kind = "takeover"
)

// SecretRequests configures `secret` requests: the only places a value may
// land. the agent names one of these targets; it can never pick a path or
// variable of its own, because it also writes the prompt the human reads.
type SecretRequests struct {
	Targets []SecretTarget `json:"targets"`
}

// SecretTarget is one declared destination. exactly one of Env and File is
// set. Label is what the human sees first, ahead of the agent's text.
type SecretTarget struct {
	Env   string `json:"env,omitempty"`
	File  string `json:"file,omitempty"`
	Label string `json:"label"`
}

// Name is how the agent refers to the target: the variable name or the
// file path.
func (t SecretTarget) Name() string {
	if t.Env != "" {
		return t.Env
	}
	return t.File
}

// SecretPayload is what the agent sends with a secret request, plus the
// declared label copied in at raise time. it never holds the value.
type SecretPayload struct {
	Target  string `json:"target"`
	Label   string `json:"label"`
	Message string `json:"message,omitempty"`
}

// SecretAnswer is the only answer an agent ever gets back for a secret.
const SecretAnswer = "done"

const (
	maxSecretTargets  = 20
	maxSecretLabelLen = 200
	// MaxSecretValueLen bounds a value a human types or pastes.
	MaxSecretValueLen = 64 << 10
	// ReservedGuestDir holds fuse's own credentials in the guest; no secret
	// target may point into it.
	ReservedGuestDir = "/fuse"
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate checks the declared targets.
func (r SecretRequests) Validate() error {
	var errs []error
	if len(r.Targets) == 0 {
		errs = append(errs, errors.New("hitl.requests.secret.targets: declare at least one target"))
	}
	if len(r.Targets) > maxSecretTargets {
		errs = append(errs, fmt.Errorf("hitl.requests.secret.targets: at most %d targets are allowed", maxSecretTargets))
	}
	seen := make(map[string]bool, len(r.Targets))
	for i, t := range r.Targets {
		if err := t.validate(); err != nil {
			errs = append(errs, fmt.Errorf("hitl.requests.secret.targets[%d]: %w", i, err))
			continue
		}
		if seen[t.Name()] {
			errs = append(errs, fmt.Errorf("hitl.requests.secret.targets[%d]: %q is declared twice", i, t.Name()))
		}
		seen[t.Name()] = true
	}
	return errors.Join(errs...)
}

func (t SecretTarget) validate() error {
	switch {
	case t.Env != "" && t.File != "":
		return errors.New("set one of env or file, not both")
	case t.Env == "" && t.File == "":
		return errors.New("set one of env or file")
	case t.Env != "" && !envNameRE.MatchString(t.Env):
		return fmt.Errorf("env %q is not a valid variable name", t.Env)
	case t.File != "":
		if err := ValidateSecretFile(t.File); err != nil {
			return err
		}
	}
	if strings.TrimSpace(t.Label) == "" {
		return errors.New("label is required; it is what the human sees")
	}
	if len(t.Label) > maxSecretLabelLen {
		return fmt.Errorf("label must be at most %d bytes", maxSecretLabelLen)
	}
	return nil
}

// ValidateSecretFile checks a file target is an absolute, clean guest path
// outside the reserved directory.
func ValidateSecretFile(p string) error {
	if !strings.HasPrefix(p, "/") {
		return fmt.Errorf("file %q must be an absolute path", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("file %q must not contain ..", p)
		}
	}
	clean := path.Clean(p)
	if clean != p || strings.HasSuffix(p, "/") {
		return fmt.Errorf("file %q must be a clean path to a file", p)
	}
	if clean == ReservedGuestDir || strings.HasPrefix(clean, ReservedGuestDir+"/") {
		return fmt.Errorf("file %q is under %s, which fuse reserves for its own credentials", p, ReservedGuestDir)
	}
	return nil
}

// Target returns the declared target the agent named, if any.
func (r *SecretRequests) Target(name string) (SecretTarget, bool) {
	if r == nil {
		return SecretTarget{}, false
	}
	for _, t := range r.Targets {
		if t.Name() == name {
			return t, true
		}
	}
	return SecretTarget{}, false
}

// ValidateSecretValue bounds a value before it is written into the guest.
func ValidateSecretValue(v string) error {
	if v == "" {
		return errors.New("value is required")
	}
	if len(v) > MaxSecretValueLen {
		return fmt.Errorf("value must be at most %d bytes", MaxSecretValueLen)
	}
	return nil
}

// AnswerSecret resolves a pending secret request once its value is in the
// guest. the decision records who answered and when, and the agent's answer
// is always SecretAnswer: the value itself never reaches the record.
func AnswerSecret(in Intervention, by Actor, now time.Time) (Intervention, error) {
	if in.Kind != KindSecret {
		return in, fmt.Errorf("intervention %s is a %s, not a secret request", in.ID, in.Kind)
	}
	if err := Transition(in.ID, in.State, StateApproved); err != nil {
		return in, err
	}
	in.State = StateApproved
	in.Decision = &Decision{
		Outcome:   OutcomeApprove,
		Answer:    SecretAnswer,
		DecidedBy: by,
		DecidedAt: now,
	}
	in.UpdatedAt = now
	return in, nil
}
