package fuse

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Hitl gate surfaces, the Kind of an intervention a gate raised.
const (
	HitlKindExec      = "exec"
	HitlKindLifecycle = "lifecycle"
)

// Gate modes.
const (
	GateModeAsk    = "ask"
	GateModeNotify = "notify"
	GateModeDeny   = "deny"
)

// Lifecycle actions a gate can hold.
const (
	LifecycleDestroy     = "destroy"
	LifecycleFork        = "fork"
	LifecycleMigrate     = "migrate"
	LifecycleDrain       = "drain"
	LifecycleSnapshot    = "snapshot"
	LifecycleRotateToken = "rotate-token"
)

// AllowlistGate is the gate name the exec allowlist reports.
const AllowlistGate = "exec.allowlist"

// ForbiddenHitlDenied is the forbidden reason of an action a deny gate (or
// the allowlist in deny mode) refused. The error message is the gate's
// reason; Details["gate"] names the gate.
const ForbiddenHitlDenied = "hitl_denied"

// IsHitlDenied reports whether err is an action a hitl gate denied.
func IsHitlDenied(err error) bool { return ForbiddenReason(err) == ForbiddenHitlDenied }

// HitlGate is one enforced gate.
type HitlGate struct {
	Name string `json:"name"`
	// On is HitlKindExec or HitlKindLifecycle.
	On    string        `json:"on"`
	Match HitlGateMatch `json:"match"`
	// Mode is GateModeAsk (the default), GateModeNotify or GateModeDeny.
	Mode string `json:"mode,omitempty"`
	// Grant is the default reach of an approval: "once", "env" or
	// "ttl:<duration>".
	Grant          string `json:"grant,omitempty"`
	TimeoutSeconds int64  `json:"timeout_seconds,omitempty"`
	// Reason is returned on a deny and shown to the human on an ask.
	Reason string `json:"reason,omitempty"`
}

// HitlGateMatch selects the actions a gate applies to. Every field set must
// match.
type HitlGateMatch struct {
	// Program matches the basename of argv[0] after env, sudo, nice,
	// timeout, xargs, nohup and stdbuf are stripped.
	Program []string `json:"program,omitempty"`
	// Args matches when they appear in argv in this order.
	Args []string `json:"args,omitempty"`
	// Shell matches any opaque shell exec.
	Shell bool `json:"shell,omitempty"`
	// Action matches lifecycle actions (the Lifecycle constants).
	Action []string `json:"action,omitempty"`
	// Failures raises a stall after this many consecutive non-zero exec
	// exits (On HitlKindStall).
	Failures int `json:"failures,omitempty"`
	// QuietSeconds raises a stall after this long with no exec or computer
	// call (On HitlKindStall).
	QuietSeconds int64 `json:"quiet_seconds,omitempty"`
	// BeforeSeconds raises an expiry this long before the idle_timeout or
	// max_runtime reap (On HitlKindExpiry).
	BeforeSeconds int64 `json:"before_seconds,omitempty"`
}

// HitlExecPolicy is allowlist mode for exec: a program not in Allow is held
// with Default ("ask" or "deny"), interpreters and shells included.
type HitlExecPolicy struct {
	Default string   `json:"default"`
	Allow   []string `json:"allow"`
}

// ExecPayload is a held exec.
type ExecPayload struct {
	Argv      []string `json:"argv"`
	Shell     string   `json:"shell,omitempty"`
	TimeoutMS int64    `json:"timeout_ms,omitempty"`
}

// LifecyclePayload is a held environment action.
type LifecyclePayload struct {
	Action string `json:"action"`
}

// InterventionResult is what running an approved gated action produced.
type InterventionResult struct {
	Exec       *ExecResult `json:"exec,omitempty"`
	EnvID      string      `json:"env_id,omitempty"`
	SnapshotID string      `json:"snapshot_id,omitempty"`
	Error      string      `json:"error,omitempty"`
	FinishedAt time.Time   `json:"finished_at"`
}

// Settled reports whether a caller has its final answer: resolved, and for
// an approved gated action that fuse performs, performed.
func (in *Intervention) Settled() bool {
	if !in.Terminal() {
		return false
	}
	runs := in.Mode == GateModeAsk && (in.Payload.Exec != nil || in.Payload.Lifecycle != nil)
	return !runs || !in.Approved() || in.Result != nil
}

// heldResponse is the 202 body of a held action.
type heldResponse struct {
	InterventionID string       `json:"intervention_id"`
	Intervention   Intervention `json:"intervention"`
}

// PendingInterventionError is returned instead of waiting when a gate held
// the call and waiting is off (ExecRequest.NoWait or WithHitlNoWait). Wait
// on Intervention with Interventions.Wait; the result is on its Result.
type PendingInterventionError struct {
	Intervention *Intervention
}

func (e *PendingInterventionError) Error() string {
	return fmt.Sprintf("held by hitl gate %s: waiting on intervention %s", e.Intervention.Gate, e.Intervention.ID)
}

// InterventionRejectedError is a held call a human rejected, or that
// expired or was cancelled without approval. Reason is for the agent to
// read and replan on.
type InterventionRejectedError struct {
	Intervention *Intervention
}

func (e *InterventionRejectedError) Error() string {
	reason := ""
	if e.Intervention.Decision != nil {
		reason = e.Intervention.Decision.Reason
	}
	return fmt.Sprintf("intervention %s %s: %s", e.Intervention.ID, e.Intervention.State, reason)
}

// HeldActionError is an approved action fuse could not perform.
type HeldActionError struct {
	Intervention *Intervention
}

func (e *HeldActionError) Error() string {
	return fmt.Sprintf("approved action of intervention %s failed: %s", e.Intervention.ID, e.Intervention.Result.Error)
}

// heldOptions are the per-call overrides of the client's hitl wait options.
type heldOptions struct {
	noWait bool
	onHeld func(*Intervention)
}

// readHeld returns the held intervention of a 202 response, or nil when
// the response is the action's own answer. It closes the body of a 202.
func readHeld(resp *http.Response) (*Intervention, error) {
	if resp.StatusCode != http.StatusAccepted {
		return nil, nil
	}
	defer func() { _ = resp.Body.Close() }()
	var held heldResponse
	if err := json.NewDecoder(resp.Body).Decode(&held); err != nil {
		return nil, fmt.Errorf("decode held response: %w", err)
	}
	return &held.Intervention, nil
}

// settleHeld waits out a held call: it reports the hold, waits until the
// intervention settles, and turns anything but a successful run into an
// error. ctx bounds the wait; every intervention has a deadline, so it
// always ends.
func (t *transport) settleHeld(ctx context.Context, held *Intervention, o heldOptions) (*Intervention, error) {
	onHeld := o.onHeld
	if onHeld == nil {
		onHeld = t.onHeld
	}
	if onHeld != nil {
		onHeld(held)
	}
	if o.noWait || t.hitlNoWait {
		return nil, &PendingInterventionError{Intervention: held}
	}
	in, err := waitIntervention(ctx, t, held.ID)
	if err != nil {
		return nil, err
	}
	if !in.Approved() {
		return nil, &InterventionRejectedError{Intervention: in}
	}
	if in.Result != nil && in.Result.Error != "" {
		return nil, &HeldActionError{Intervention: in}
	}
	return in, nil
}

// WithHitlNoWait makes every gated call return a *PendingInterventionError
// at once instead of waiting for the human. ExecRequest.NoWait does the
// same for one exec.
func WithHitlNoWait() Option {
	return func(cfg *clientConfig) {
		cfg.hitlNoWait = true
	}
}

// WithOnHeld is called with the pending intervention whenever a gate holds
// a call, before the call waits on it.
func WithOnHeld(fn func(*Intervention)) Option {
	return func(cfg *clientConfig) {
		cfg.onHeld = fn
	}
}

// ApproveOptions is an approval of a held action or an answer to an ask.
type ApproveOptions struct {
	// Answer is required to approve an ask.
	Answer string `json:"answer,omitempty"`
	Reason string `json:"reason,omitempty"`
	// EditedArgv replaces a held exec's argv. It is re-checked against the
	// deny gates, and cannot be combined with Grant.
	EditedArgv []string `json:"edited_argv,omitempty"`
	// Grant is "once", "env" or "ttl:<duration>"; empty takes the gate's
	// default.
	Grant string `json:"grant,omitempty"`
	// ExtendSeconds is how long approving an expiry keeps the environment
	// alive. Zero is one more full window of the reaper that was due.
	ExtendSeconds int64 `json:"extend_seconds,omitempty"`
}
