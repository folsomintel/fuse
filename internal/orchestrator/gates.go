package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// ErrHitlDenied is wrapped by every GateDeniedError.
var ErrHitlDenied = errors.New("denied by hitl gate")

// GateDeniedError is a deny gate (or the allowlist in deny mode) refusing an
// action. Reason is the gate's text, for the agent to read and replan on.
type GateDeniedError struct {
	Gate           string
	Reason         string
	InterventionID string
}

func (e *GateDeniedError) Error() string { return e.Reason }
func (e *GateDeniedError) Unwrap() error { return ErrHitlDenied }

// GateCaller is who is attempting a gated action.
type GateCaller struct {
	By         hitl.Actor
	RemoteAddr string
}

// GateExec runs an api exec through the environment's exec gates. it
// returns nil, nil when the exec may run now; a pending intervention when it
// is held (the orchestrator runs it on approval); or a *GateDeniedError.
// it never touches the host agent, so a held exec holds no vm lock.
func (fm *FleetManager) GateExec(ctx context.Context, envID string, p hitl.ExecPayload, c GateCaller) (*hitl.Intervention, error) {
	spec := fm.hitlSpec(envID)
	if spec == nil {
		return nil, nil
	}
	v := spec.EvaluateExec(hitl.ExecCall{Argv: p.Argv, Shell: p.Shell != ""})
	return fm.applyVerdict(ctx, envID, *spec, v, c, p.Argv, func(in *hitl.Intervention) {
		in.Kind = hitl.KindExec
		in.Exec = &p
	})
}

// GateLifecycle runs an environment action through the lifecycle gates, the
// same way GateExec does. params are the action's options, replayed as-is
// when the held action is approved.
func (fm *FleetManager) GateLifecycle(ctx context.Context, envID string, action hitl.LifecycleAction, params any, c GateCaller) (*hitl.Intervention, error) {
	spec := fm.hitlSpec(envID)
	if spec == nil {
		return nil, nil
	}
	v := spec.EvaluateLifecycle(action)
	if v.Mode() == "" {
		return nil, nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("encode %s params: %w", action, err)
	}
	return fm.applyVerdict(ctx, envID, *spec, v, c, string(action), func(in *hitl.Intervention) {
		in.Kind = hitl.KindLifecycle
		in.Lifecycle = &hitl.LifecyclePayload{Action: action, Params: raw}
	})
}

// hitlSpec returns an environment's hitl policy, nil when it has none or is
// unknown (the action itself then reports the 404).
func (fm *FleetManager) hitlSpec(envID string) *hitl.Spec {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	v, ok := fm.vms[envID]
	if !ok {
		return nil
	}
	return v.spec.Hitl
}

// applyVerdict acts on the gates that matched one action: a deny refuses it,
// each notify gate leaves a terminal record, and the first ask gate not
// covered by a grant holds it.
func (fm *FleetManager) applyVerdict(ctx context.Context, envID string, spec hitl.Spec, v hitl.Verdict, c GateCaller, call any, set func(*hitl.Intervention)) (*hitl.Intervention, error) {
	if v.Mode() == "" {
		return nil, nil
	}
	now := time.Now()
	open := func(g hitl.Gate) hitl.Intervention {
		in := hitl.Intervention{
			ID:          hitl.NewID(now),
			EnvID:       envID,
			Gate:        g.Name,
			Mode:        g.EffectiveMode(),
			State:       hitl.StatePending,
			TriggeredBy: c.By,
			Deadline:    now.Add(g.Timeout(spec)),
			OnTimeout:   spec.EffectiveOnTimeout(),
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		set(&in)
		return in
	}

	if v.Deny != nil {
		in := open(*v.Deny)
		reason := v.Deny.DenyReason()
		in.State = hitl.StateRejected
		in.Decision = &hitl.Decision{Outcome: hitl.OutcomeReject, Reason: reason, DecidedBy: hitl.SystemActor, DecidedAt: now}
		if err := fm.recordGateIntervention(ctx, in, c.RemoteAddr); err != nil {
			fm.logger.Warn("record deny intervention failed", "vm", envID, "gate", in.Gate, "err", err)
		}
		return nil, &GateDeniedError{Gate: in.Gate, Reason: reason, InterventionID: in.ID}
	}

	for _, g := range v.Notify {
		in := open(g)
		in.State = hitl.StateApproved
		in.Decision = &hitl.Decision{Outcome: hitl.OutcomeApprove, Reason: "notify gate " + g.Name + ": let through", DecidedBy: hitl.SystemActor, DecidedAt: now}
		if err := fm.recordGateIntervention(ctx, in, c.RemoteAddr); err != nil {
			fm.logger.Warn("record notify intervention failed", "vm", envID, "gate", g.Name, "err", err)
		}
	}

	for _, g := range v.Ask {
		if grant, ok := fm.activeGrant(ctx, envID, g.Name, hitl.GrantKey(g.Name, call, fm.tokenEncryptionKey), now); ok {
			fm.recordAudit(ctx, hitl.AuditEntry{
				Event: hitl.AuditGrantUsed, InterventionID: grant.InterventionID, EnvID: envID, Actor: c.By, RemoteAddr: c.RemoteAddr,
			}, fm.webhookTargetsFor(envID), nil)
			continue
		}
		in := open(g)
		if err := fm.recordGateIntervention(ctx, in, c.RemoteAddr); err != nil {
			return nil, err
		}
		fm.touchActivity(envID)
		return &in, nil
	}
	return nil, nil
}

// recordGateIntervention stores an intervention a gate raised and announces
// it. a notify or deny record is born terminal, so it gets both its raised
// and its decided audit rows.
func (fm *FleetManager) recordGateIntervention(ctx context.Context, in hitl.Intervention, remoteAddr string) error {
	rec, err := fm.interventionToRecord(in)
	if err != nil {
		return err
	}
	if err := fm.store.UpsertIntervention(ctx, rec); err != nil {
		return err
	}
	fm.appendEvent(ctx, "intervention", in.ID, "intervention.raised", map[string]any{
		"env_id": in.EnvID, "kind": string(in.Kind), "gate": in.Gate, "mode": string(in.Mode), "deadline": in.Deadline,
	})
	fm.publishInterventionEvent(in)
	raised := in
	raised.State, raised.Decision = hitl.StatePending, nil
	fm.auditIntervention(ctx, raised, auditMeta{remoteAddr: remoteAddr})
	if in.State.Terminal() {
		fm.auditIntervention(ctx, in, auditMeta{})
	}
	return nil
}

// activeGrant finds a live grant for the gate and action. a store error
// counts as no grant, so the action is asked about rather than let through.
func (fm *FleetManager) activeGrant(ctx context.Context, envID, gate, key string, now time.Time) (hitl.Grant, bool) {
	grants, err := fm.store.ListHitlGrants(ctx, envID)
	if err != nil {
		fm.logger.Warn("list hitl grants failed", "vm", envID, "err", err)
		return hitl.Grant{}, false
	}
	for _, g := range grants {
		if g.Gate == gate && g.Key == key && g.Active(now) {
			return g, true
		}
	}
	return hitl.Grant{}, false
}

// maxEditedArgvBytes bounds an edited argv.
const maxEditedArgvBytes = 64 << 10

// applyGateDecision fills in the gate parts of an approval: the edited
// argv, re-checked against the deny gates, and the grant. an edit cannot
// carry a grant: the grant would cover the original argv, which the human
// did not approve as-is.
func applyGateDecision(in *hitl.Intervention, spec *hitl.Spec, editedArgv []string, grant string) error {
	if in.Kind == hitl.KindAsk {
		if editedArgv != nil || grant != "" {
			return fmt.Errorf("%w: edited_argv and grant only apply to a gated action", ErrInvalidIntervention)
		}
		return nil
	}
	if editedArgv != nil {
		if in.Kind != hitl.KindExec {
			return fmt.Errorf("%w: edited_argv only applies to an exec", ErrInvalidIntervention)
		}
		if len(editedArgv) == 0 || editedArgv[0] == "" {
			return fmt.Errorf("%w: edited_argv must not be empty", ErrInvalidIntervention)
		}
		n := 0
		for _, a := range editedArgv {
			n += len(a)
		}
		if n > maxEditedArgvBytes {
			return fmt.Errorf("%w: edited_argv must be at most %d bytes", ErrInvalidIntervention, maxEditedArgvBytes)
		}
		if spec != nil {
			if v := spec.EvaluateExec(hitl.ExecCall{Argv: editedArgv}); v.Deny != nil {
				return &GateDeniedError{Gate: v.Deny.Name, Reason: "the edited argv matches deny gate " + v.Deny.Name + ": " + v.Deny.DenyReason()}
			}
		}
		in.Decision.EditedArgv = editedArgv
	}
	explicit := grant != ""
	if !explicit && spec != nil {
		if g, ok := spec.FindGate(in.Gate); ok {
			grant = g.Grant
		}
	}
	gs, err := hitl.ParseGrant(grant)
	if err != nil {
		return fmt.Errorf("%w: grant %w", ErrInvalidIntervention, err)
	}
	if editedArgv != nil && gs.Scope != hitl.GrantOnce {
		if explicit {
			return fmt.Errorf("%w: a grant cannot be combined with an edit; it would cover the original argv", ErrInvalidIntervention)
		}
		gs = hitl.GrantSpec{Scope: hitl.GrantOnce}
	}
	in.Decision.Grant = gs.String()
	return nil
}

// storeGrant writes the grant an approval asked for and audits it.
func (fm *FleetManager) storeGrant(ctx context.Context, in hitl.Intervention, remoteAddr string) {
	if in.Decision == nil || in.Decision.Outcome != hitl.OutcomeApprove {
		return
	}
	gs, err := hitl.ParseGrant(in.Decision.Grant)
	if err != nil {
		return
	}
	var call any
	switch {
	case in.Exec != nil:
		call = in.Exec.Argv
	case in.Lifecycle != nil:
		call = string(in.Lifecycle.Action)
	default:
		return
	}
	g, ok := hitl.NewGrant(in, gs, hitl.GrantKey(in.Gate, call, fm.tokenEncryptionKey), time.Now())
	if !ok {
		return
	}
	if err := fm.store.InsertHitlGrant(ctx, g); err != nil {
		fm.logger.Warn("store hitl grant failed", "intervention", in.ID, "err", err)
		return
	}
	fm.recordAudit(ctx, hitl.AuditEntry{
		Event: hitl.AuditGrantCreated, InterventionID: in.ID, EnvID: in.EnvID, Actor: in.Decision.DecidedBy, RemoteAddr: remoteAddr,
	}, fm.webhookTargetsFor(in.EnvID), nil)
}

// dropEnvGrants removes a destroyed environment's grants.
func (fm *FleetManager) dropEnvGrants(ctx context.Context, envID string) {
	if err := fm.store.DeleteHitlGrants(ctx, envID); err != nil {
		fm.logger.Warn("delete hitl grants failed", "vm", envID, "err", err)
	}
}
