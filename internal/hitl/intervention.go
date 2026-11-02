package hitl

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Outcome is the effective result of a resolved intervention. every
// terminal intervention has one, including expired (from on_timeout) and
// cancelled (always reject), so a caller only ever has to read this.
type Outcome string

const (
	OutcomeApprove Outcome = "approve"
	OutcomeReject  Outcome = "reject"
)

// ActorSource says how an actor was established.
type ActorSource string

const (
	// ActorKey is derived from the authenticating credential.
	ActorKey ActorSource = "key"
	// ActorSystem is fuse itself (deadline expiry, cancel on destroy).
	ActorSystem ActorSource = "system"
)

// Actor is a plain label for who raised or decided an intervention. fuse
// records it and never checks it.
type Actor struct {
	ID     string      `json:"id"`
	Name   string      `json:"name,omitempty"`
	Source ActorSource `json:"source"`
	KeyID  string      `json:"key_id,omitempty"`
}

// SystemActor is the actor recorded for decisions fuse makes on its own.
var SystemActor = Actor{ID: "system", Source: ActorSystem}

// Intervention is one request for a human. Payload, Answer and Reason hold
// content and are encrypted at rest by the orchestrator; the rest is
// metadata the sweeper and list filters read in the clear.
type Intervention struct {
	ID          string
	EnvID       string
	Kind        Kind
	State       State
	// Gate names the gate that raised it; empty for a request.
	Gate string
	// Mode is the gate's mode; empty for a request.
	Mode Mode
	// Payload is the ask, for KindAsk.
	Payload AskPayload
	// Exec is the held exec, for KindExec.
	Exec *ExecPayload
	// Lifecycle is the held environment action, for KindLifecycle.
	Lifecycle   *LifecyclePayload
	TriggeredBy Actor
	Deadline    time.Time
	OnTimeout   OnTimeout
	Decision    *Decision
	// Result is what the orchestrator got running the approved action.
	Result    *Result
	CreatedAt time.Time
	UpdatedAt   time.Time
}

// Decision is how an intervention was resolved.
type Decision struct {
	Outcome Outcome
	// Answer is the human's reply to an ask. empty unless approved.
	Answer string
	// Reason is text fed back to the agent. always set on a reject.
	Reason    string
	DecidedBy Actor
	DecidedAt time.Time
	// EditedArgv replaces a held exec's argv, set when the human edited it.
	EditedArgv []string
	// Grant is how far the approval reaches: once, env or ttl:<duration>.
	Grant string
}

// ExecPayload is a held exec.
type ExecPayload struct {
	Argv []string `json:"argv"`
	// Shell is the script when the caller sent shell:, in which case Argv
	// is the sh -lc that runs it.
	Shell     string `json:"shell,omitempty"`
	TimeoutMS int64  `json:"timeout_ms,omitempty"`
}

// LifecyclePayload is a held environment action. Params is the action's
// options as the orchestrator will replay them.
type LifecyclePayload struct {
	Action LifecycleAction `json:"action"`
	Params []byte          `json:"params,omitempty"`
}

// Result is the outcome of running an approved action.
type Result struct {
	// Exec is set for an exec that ran.
	Exec *ExecResult `json:"exec,omitempty"`
	// EnvID is the environment a fork or migrate produced.
	EnvID string `json:"env_id,omitempty"`
	// SnapshotID is the snapshot a snapshot action produced.
	SnapshotID string `json:"snapshot_id,omitempty"`
	// Error is set when the approved action could not be performed.
	Error      string    `json:"error,omitempty"`
	FinishedAt time.Time `json:"finished_at"`
}

// ExecResult is the guest's answer to an approved exec.
type ExecResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// Runs reports whether fuse performs the action itself once approved: an
// ask gate holding an exec or a lifecycle action.
func (in Intervention) Runs() bool {
	return in.Mode == ModeAsk && (in.Exec != nil || in.Lifecycle != nil)
}

// Settled reports whether a waiting caller has its final answer: resolved,
// and when fuse runs the action on approval, run.
func (in Intervention) Settled() bool {
	if !in.State.Terminal() {
		return false
	}
	if !in.Runs() || in.Decision == nil || in.Decision.Outcome != OutcomeApprove {
		return true
	}
	return in.Result != nil
}

// Argv returns the argv an approved exec runs: the edit when there is one.
func (in Intervention) Argv() []string {
	if in.Decision != nil && in.Decision.EditedArgv != nil {
		return in.Decision.EditedArgv
	}
	if in.Exec == nil {
		return nil
	}
	return in.Exec.Argv
}

// AskPayload is what the agent sends with an ask request.
type AskPayload struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices,omitempty"`
}

const (
	maxQuestionLen = 4096
	maxChoices     = 20
	maxChoiceLen   = 200
	maxAnswerLen   = 16 << 10
	maxReasonLen   = 4096
)

// Validate checks an ask payload.
func (p AskPayload) Validate() error {
	if strings.TrimSpace(p.Question) == "" {
		return errors.New("question is required")
	}
	if len(p.Question) > maxQuestionLen {
		return fmt.Errorf("question must be at most %d bytes", maxQuestionLen)
	}
	if len(p.Choices) > maxChoices {
		return fmt.Errorf("at most %d choices are allowed", maxChoices)
	}
	seen := make(map[string]bool, len(p.Choices))
	for i, c := range p.Choices {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("choices[%d] must not be empty", i)
		}
		if len(c) > maxChoiceLen {
			return fmt.Errorf("choices[%d] must be at most %d bytes", i, maxChoiceLen)
		}
		if seen[c] {
			return fmt.Errorf("choices[%d] %q is a duplicate", i, c)
		}
		seen[c] = true
	}
	return nil
}

// ValidateAnswer checks an approval's answer against the ask it answers.
func (p AskPayload) ValidateAnswer(answer string) error {
	if strings.TrimSpace(answer) == "" {
		return errors.New("answer is required to approve an ask")
	}
	if len(answer) > maxAnswerLen {
		return fmt.Errorf("answer must be at most %d bytes", maxAnswerLen)
	}
	if len(p.Choices) > 0 && !slices.Contains(p.Choices, answer) {
		return fmt.Errorf("answer must be one of %s", strings.Join(p.Choices, ", "))
	}
	return nil
}

// ValidateReason bounds a decision reason.
func ValidateReason(reason string) error {
	if len(reason) > maxReasonLen {
		return fmt.Errorf("reason must be at most %d bytes", maxReasonLen)
	}
	return nil
}

// Expire resolves a pending intervention whose deadline passed, applying
// its on_timeout.
func Expire(in Intervention, now time.Time) (Intervention, error) {
	if err := Transition(in.ID, in.State, StateExpired); err != nil {
		return in, err
	}
	outcome := OutcomeReject
	if in.OnTimeout == OnTimeoutAllow {
		outcome = OutcomeApprove
	}
	in.State = StateExpired
	in.Decision = &Decision{
		Outcome:   outcome,
		Reason:    "deadline passed with no decision; on_timeout is " + string(effectiveOnTimeout(in.OnTimeout)),
		DecidedBy: SystemActor,
		DecidedAt: now,
	}
	in.UpdatedAt = now
	return in, nil
}

// Cancel resolves a pending intervention that can no longer be answered,
// such as one whose environment was destroyed.
func Cancel(in Intervention, reason string, now time.Time) (Intervention, error) {
	if err := Transition(in.ID, in.State, StateCancelled); err != nil {
		return in, err
	}
	in.State = StateCancelled
	in.Decision = &Decision{
		Outcome:   OutcomeReject,
		Reason:    reason,
		DecidedBy: SystemActor,
		DecidedAt: now,
	}
	in.UpdatedAt = now
	return in, nil
}

// Decide resolves a pending intervention with a human's decision. approving
// an ask must carry a valid answer; a reject always ends up with a reason.
func Decide(in Intervention, outcome Outcome, answer, reason string, by Actor, now time.Time) (Intervention, error) {
	var to State
	switch outcome {
	case OutcomeApprove:
		to = StateApproved
	case OutcomeReject:
		to = StateRejected
	default:
		return in, fmt.Errorf("unknown outcome %q", outcome)
	}
	if err := Transition(in.ID, in.State, to); err != nil {
		return in, err
	}
	if err := ValidateReason(reason); err != nil {
		return in, err
	}
	if outcome == OutcomeApprove {
		if in.Kind == KindAsk {
			if err := in.Payload.ValidateAnswer(answer); err != nil {
				return in, err
			}
		} else if answer != "" {
			return in, errors.New("answer only applies to an ask")
		}
	} else {
		answer = ""
		if strings.TrimSpace(reason) == "" {
			reason = "rejected by " + by.ID
		}
	}
	in.State = to
	in.Decision = &Decision{
		Outcome:   outcome,
		Answer:    answer,
		Reason:    reason,
		DecidedBy: by,
		DecidedAt: now,
	}
	in.UpdatedAt = now
	return in, nil
}

func effectiveOnTimeout(o OnTimeout) OnTimeout {
	if o == "" {
		return OnTimeoutReject
	}
	return o
}

// NewID returns an intervention id that sorts by creation time, so listing
// by id is listing oldest first.
func NewID(now time.Time) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("int_%016x%s", now.UnixNano(), hex.EncodeToString(b[:]))
}
