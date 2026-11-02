package hitl

import (
	"fmt"
	"time"
)

// State is the lifecycle of a single intervention. it starts pending and
// moves to exactly one terminal state. the move is one way: a resolved
// intervention is never re-opened.
type State string

const (
	StatePending   State = "pending"
	StateApproved  State = "approved"
	StateRejected  State = "rejected"
	StateExpired   State = "expired"
	StateCancelled State = "cancelled"
)

// Valid reports whether s is one of the five known states.
func (s State) Valid() bool {
	switch s {
	case StatePending, StateApproved, StateRejected, StateExpired, StateCancelled:
		return true
	}
	return false
}

// Terminal reports whether s is final.
func (s State) Terminal() bool {
	switch s {
	case StateApproved, StateRejected, StateExpired, StateCancelled:
		return true
	}
	return false
}

// ErrResolved is returned for any transition out of a terminal state.
type ErrResolved struct {
	ID    string
	State State
}

func (e *ErrResolved) Error() string {
	return fmt.Sprintf("intervention %s is already %s", e.ID, e.State)
}

// Transition checks that from -> to is legal. the only legal moves are
// pending -> one of the terminal states.
func Transition(id string, from, to State) error {
	if from.Terminal() {
		return &ErrResolved{ID: id, State: from}
	}
	if from != StatePending {
		return fmt.Errorf("intervention %s: unknown state %q", id, from)
	}
	if !to.Terminal() {
		return fmt.Errorf("intervention %s: %q is not a terminal state", id, to)
	}
	return nil
}

// Deadline returns when an intervention opened at now must be resolved by.
// a per-call timeout can only shorten the spec's timeout, never extend it.
func Deadline(now time.Time, spec Spec, callTimeout time.Duration) time.Time {
	timeout := spec.Timeout()
	if callTimeout > 0 && callTimeout < timeout {
		timeout = callTimeout
	}
	return now.Add(timeout)
}

// Expired reports whether a pending intervention has passed its deadline.
func Expired(now, deadline time.Time) bool {
	return !deadline.IsZero() && !now.Before(deadline)
}
