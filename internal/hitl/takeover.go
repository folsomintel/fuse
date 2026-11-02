package hitl

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// TakeoverVia is how a human takes over an environment.
type TakeoverVia string

const (
	// ViaTerminal gives the human their own shell on the attach path.
	ViaTerminal TakeoverVia = "terminal"
)

// TakeoverRequests configures `takeover` requests. Via lists the allowed
// ways in; empty means terminal only.
type TakeoverRequests struct {
	Via []TakeoverVia `json:"via,omitempty"`
}

// Validate checks the allowed ways in. desktop and port are designed but
// not built yet.
func (r TakeoverRequests) Validate() error {
	for i, v := range r.Via {
		switch v {
		case ViaTerminal:
		case "desktop", "port":
			return fmt.Errorf("hitl.requests.takeover.via[%d]: %q is not yet supported", i, v)
		default:
			return fmt.Errorf("hitl.requests.takeover.via[%d]: must be %q, got %q", i, ViaTerminal, v)
		}
	}
	return nil
}

// Allows reports whether a way in is declared.
func (r *TakeoverRequests) Allows(v TakeoverVia) bool {
	if r == nil {
		return false
	}
	if len(r.Via) == 0 {
		return v == ViaTerminal
	}
	return slices.Contains(r.Via, v)
}

// TakeoverPayload is a takeover's request and its live state. Holder and
// StartedAt are set once a human has taken control; until then the
// environment is not locked.
type TakeoverPayload struct {
	Via       TakeoverVia `json:"via"`
	Reason    string      `json:"reason,omitempty"`
	Holder    *Actor      `json:"holder,omitempty"`
	StartedAt *time.Time  `json:"started_at,omitempty"`
}

// Held reports whether a human holds control right now.
func (p *TakeoverPayload) Held() bool {
	return p != nil && p.StartedAt != nil
}

// ErrTakeoverHeld is returned when a second human tries to take over an
// environment someone already holds.
var ErrTakeoverHeld = errors.New("takeover already held")

// StartTakeover marks a pending takeover as held by by. the intervention
// stays pending: it resolves on release, and its deadline bounds the lock.
func StartTakeover(in Intervention, by Actor, now time.Time) (Intervention, error) {
	if in.Kind != KindTakeover || in.Takeover == nil {
		return in, fmt.Errorf("intervention %s is a %s, not a takeover", in.ID, in.Kind)
	}
	if in.State.Terminal() {
		return in, &ErrResolved{ID: in.ID, State: in.State}
	}
	if in.Takeover.Held() {
		return in, fmt.Errorf("%w by %s", ErrTakeoverHeld, in.Takeover.Holder.ID)
	}
	t := *in.Takeover
	holder := by
	at := now
	t.Holder, t.StartedAt = &holder, &at
	in.Takeover = &t
	in.UpdatedAt = now
	return in, nil
}

// ReleaseTakeover ends a held takeover. the note is what the agent gets
// back, so it is the decision's answer.
func ReleaseTakeover(in Intervention, note string, by Actor, now time.Time) (Intervention, error) {
	if in.Kind != KindTakeover || in.Takeover == nil {
		return in, fmt.Errorf("intervention %s is a %s, not a takeover", in.ID, in.Kind)
	}
	if err := Transition(in.ID, in.State, StateApproved); err != nil {
		return in, err
	}
	if !in.Takeover.Held() {
		return in, fmt.Errorf("takeover %s was never started; reject it instead", in.ID)
	}
	if len(note) > maxAnswerLen {
		return in, fmt.Errorf("note must be at most %d bytes", maxAnswerLen)
	}
	if strings.TrimSpace(note) == "" {
		note = "released by " + by.ID
	}
	in.State = StateApproved
	in.Decision = &Decision{
		Outcome:   OutcomeApprove,
		Answer:    note,
		DecidedBy: by,
		DecidedAt: now,
	}
	in.UpdatedAt = now
	return in, nil
}

// ValidateTakeoverReason bounds the agent's reason.
func ValidateTakeoverReason(reason string) error {
	if len(reason) > maxQuestionLen {
		return fmt.Errorf("reason must be at most %d bytes", maxQuestionLen)
	}
	return nil
}

// ValidateSecretMessage bounds the agent's text on a secret request.
func ValidateSecretMessage(msg string) error {
	if len(msg) > maxQuestionLen {
		return fmt.Errorf("message must be at most %d bytes", maxQuestionLen)
	}
	return nil
}

// Locks reports whether a pending intervention keeps the agent out of exec
// and computer calls: a held takeover does.
func (in Intervention) Locks() bool {
	return in.State == StatePending && in.Kind == KindTakeover && in.Takeover.Held()
}
