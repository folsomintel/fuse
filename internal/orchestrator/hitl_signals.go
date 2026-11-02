package orchestrator

import (
	"context"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// signalState is what the stall and expiry detectors remember about one
// environment between calls. guarded by hitlMu.
type signalState struct {
	// failures is the current run of non-zero exec exits.
	failures int
	// lastCall is the agent's last exec or computer call.
	lastCall time.Time
	// quietFor is the lastCall a quiet stall already fired for, so one
	// silence raises one stall.
	quietFor time.Time
	// expiryFor is the reap time an expiry already fired for.
	expiryFor time.Time
	// extendedUntil keeps the reapers off the environment after a human
	// approved an expiry. it is rebuilt from the approved interventions on
	// restart.
	extendedUntil time.Time
}

// signalStateLocked returns the env's detector state. hitlMu must be held.
func (fm *FleetManager) signalStateLocked(envID string) *signalState {
	if fm.hitlSignals == nil {
		fm.hitlSignals = make(map[string]*signalState)
	}
	st, ok := fm.hitlSignals[envID]
	if !ok {
		st = &signalState{}
		fm.hitlSignals[envID] = st
	}
	return st
}

// noteAgentCall records an agent exec or computer call for the quiet stall.
func (fm *FleetManager) noteAgentCall(envID string) {
	fm.hitlMu.Lock()
	fm.signalStateLocked(envID).lastCall = time.Now()
	fm.hitlMu.Unlock()
}

// noteExecExit counts consecutive failing execs and raises a stall when a
// gate's failures threshold is reached. the run restarts after a stall is
// raised, and any zero exit resets it.
func (fm *FleetManager) noteExecExit(ctx context.Context, envID string, exitCode int) {
	spec := fm.hitlSpec(envID)
	if spec == nil {
		return
	}
	var fire *hitl.Gate
	fm.hitlMu.Lock()
	st := fm.signalStateLocked(envID)
	if exitCode == 0 {
		st.failures = 0
	} else {
		st.failures++
		for _, g := range spec.SignalGates(hitl.KindStall) {
			if g.Match.Failures > 0 && st.failures >= g.Match.Failures {
				fire = &g
				break
			}
		}
	}
	failures := st.failures
	_, locked := fm.hitlLocks[envID]
	if fire != nil {
		st.failures = 0
	}
	fm.hitlMu.Unlock()
	if fire == nil || locked {
		return
	}
	fm.raiseSignal(ctx, envID, *spec, *fire, hitl.SignalPayload{Failures: failures, LastExitCode: exitCode}, time.Time{})
}

// raiseSignal opens a stall or expiry intervention. an ask stall locks the
// agent out until a human acks it; an ask expiry holds the reaper; notify
// records it and changes nothing. deadline, when set, caps the
// intervention's own deadline (an expiry is due when the reap is).
func (fm *FleetManager) raiseSignal(ctx context.Context, envID string, spec hitl.Spec, g hitl.Gate, sig hitl.SignalPayload, deadline time.Time) {
	now := time.Now()
	in := hitl.Intervention{
		ID:          hitl.NewID(now),
		EnvID:       envID,
		Kind:        g.On,
		Gate:        g.Name,
		Mode:        g.EffectiveMode(),
		State:       hitl.StatePending,
		Signal:      &sig,
		TriggeredBy: hitl.SystemActor,
		Deadline:    now.Add(g.Timeout(spec)),
		OnTimeout:   spec.EffectiveOnTimeout(),
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if g.On == hitl.KindExpiry {
		// nobody answering means the reap goes ahead.
		in.OnTimeout = hitl.OnTimeoutReject
	}
	if !deadline.IsZero() && deadline.Before(in.Deadline) {
		in.Deadline = deadline
		if floor := now.Add(hitl.MinTimeout); in.Deadline.Before(floor) {
			in.Deadline = floor
		}
	}
	if in.Mode == hitl.ModeNotify {
		in.State = hitl.StateApproved
		in.Decision = &hitl.Decision{Outcome: hitl.OutcomeApprove, Reason: "notify gate " + g.Name, DecidedBy: hitl.SystemActor, DecidedAt: now}
	}
	if err := fm.recordGateIntervention(ctx, in, ""); err != nil {
		fm.logger.Warn("raise hitl signal failed", "vm", envID, "gate", g.Name, "err", err)
		return
	}
	if in.Locks() {
		fm.hitlMu.Lock()
		fm.setLockLocked(in)
		fm.hitlMu.Unlock()
	}
}

// reconcileSignals raises the signals only time can trip: a quiet stall and
// an expiry ahead of a reap. failure stalls are raised in Exec.
func (fm *FleetManager) reconcileSignals(ctx context.Context) {
	type candidate struct {
		envID    string
		spec     hitl.Spec
		gate     hitl.Gate
		sig      hitl.SignalPayload
		deadline time.Time
	}
	now := time.Now()

	// snapshot the vms first: fm.mu is never held while taking hitlMu,
	// because the decide paths take them the other way round.
	var envs []reapView
	fm.mu.RLock()
	for id, v := range fm.vms {
		if v.state != VMStateRunning || v.spec.Hitl == nil || len(v.spec.Hitl.Gates) == 0 {
			continue
		}
		envs = append(envs, fm.reapViewLocked(id, v))
	}
	fm.mu.RUnlock()

	var fire []candidate
	fm.hitlMu.Lock()
	for _, e := range envs {
		st := fm.signalStateLocked(e.id)
		_, locked := fm.hitlLocks[e.id]

		last := st.lastCall
		if last.IsZero() {
			last = e.createdAt
		}
		for _, g := range e.spec.SignalGates(hitl.KindStall) {
			quiet := time.Duration(g.Match.QuietSeconds) * time.Second
			if quiet <= 0 || locked || st.quietFor.Equal(last) || now.Sub(last) < quiet {
				continue
			}
			st.quietFor = last
			fire = append(fire, candidate{envID: e.id, spec: e.spec, gate: g, sig: hitl.SignalPayload{QuietSeconds: int64(now.Sub(last) / time.Second)}})
			break
		}

		reapAt, reaper := e.nextReap(st.extendedUntil)
		if reapAt.IsZero() || st.expiryFor.Equal(reapAt) {
			continue
		}
		for _, g := range e.spec.SignalGates(hitl.KindExpiry) {
			before := time.Duration(g.Match.BeforeSeconds) * time.Second
			if now.Before(reapAt.Add(-before)) {
				continue
			}
			st.expiryFor = reapAt
			at := reapAt
			fire = append(fire, candidate{envID: e.id, spec: e.spec, gate: g, sig: hitl.SignalPayload{Reaper: reaper, ReapAt: &at}, deadline: reapAt})
			break
		}
	}
	fm.hitlMu.Unlock()

	for _, c := range fire {
		fm.raiseSignal(ctx, c.envID, c.spec, c.gate, c.sig, c.deadline)
	}
}

// reapView is what nextReap needs from a vm, copied out under fm.mu.
type reapView struct {
	id        string
	spec      hitl.Spec
	createdAt time.Time
	// idleAt is when the idle reaper is due; zero when it is off or
	// someone is attached.
	idleAt time.Time
	// runtimeAt is when the max_runtime ceiling is due; zero for an env
	// with no task.
	runtimeAt time.Time
}

func (fm *FleetManager) reapViewLocked(id string, v *vm) reapView {
	r := reapView{id: id, spec: *v.spec.Hitl, createdAt: v.createdAt}
	idle := v.spec.IdleTimeout
	if idle <= 0 {
		idle = fm.defaultIdleTimeout
	}
	if idle > 0 && v.attachSessions == 0 {
		since := v.lastActivityAt
		if since.IsZero() {
			since = v.createdAt
		}
		r.idleAt = since.Add(idle)
	}
	if v.taskID != "" {
		ceiling := v.spec.MaxRuntime
		if ceiling <= 0 {
			ceiling = fm.taskStuckTimeout
		}
		if ceiling > 0 {
			r.runtimeAt = v.createdAt.Add(ceiling)
		}
	}
	return r
}

// nextReap returns when a reaper will next tear the environment down, and
// which one. an approved extension pushes it out. zero means nothing will
// reap it.
func (r reapView) nextReap(extendedUntil time.Time) (time.Time, hitl.Reaper) {
	at, reaper := r.idleAt, hitl.ReaperIdle
	if !r.runtimeAt.IsZero() && (at.IsZero() || r.runtimeAt.Before(at)) {
		at, reaper = r.runtimeAt, hitl.ReaperMaxRuntime
	}
	if at.IsZero() {
		return at, ""
	}
	if extendedUntil.After(at) {
		at = extendedUntil
	}
	return at, reaper
}

// extendedEnvs returns the environments an approved expiry still keeps
// away from the reapers.
func (fm *FleetManager) extendedEnvs(now time.Time) map[string]bool {
	fm.hitlMu.Lock()
	defer fm.hitlMu.Unlock()
	out := make(map[string]bool)
	for id, st := range fm.hitlSignals {
		if now.Before(st.extendedUntil) {
			out[id] = true
		}
	}
	return out
}

// pendingExpiryEnvs returns the environments with a pending expiry. ok is
// false when the store could not be read; the caller then reaps nothing it
// cannot rule out.
func (fm *FleetManager) pendingExpiryEnvs(ctx context.Context) (map[string]bool, bool) {
	out := make(map[string]bool)
	if fm.store == nil {
		return out, true
	}
	recs, err := fm.store.ListInterventions(ctx, InterventionFilter{State: string(hitl.StatePending)})
	if err != nil {
		fm.logger.Warn("list pending interventions for reap check failed", "err", err)
		return nil, false
	}
	for _, rec := range recs {
		if rec.Kind == string(hitl.KindExpiry) {
			out[rec.EnvID] = true
		}
	}
	return out, true
}

// applyExtension records an approved expiry's extension.
func (fm *FleetManager) applyExtension(in hitl.Intervention) {
	if in.Kind != hitl.KindExpiry || in.Signal == nil || in.Signal.ExtendedUntil == nil {
		return
	}
	fm.hitlMu.Lock()
	st := fm.signalStateLocked(in.EnvID)
	if in.Signal.ExtendedUntil.After(st.extendedUntil) {
		st.extendedUntil = *in.Signal.ExtendedUntil
	}
	fm.hitlMu.Unlock()
}

// recoverExtensions rebuilds approved extensions after a restart, so an env
// a human kept alive is not reaped early because the process forgot.
func (fm *FleetManager) recoverExtensions(ctx context.Context) {
	fm.mu.RLock()
	var envs []string
	for id, v := range fm.vms {
		if v.spec.Hitl != nil && len(v.spec.Hitl.SignalGates(hitl.KindExpiry)) > 0 {
			envs = append(envs, id)
		}
	}
	fm.mu.RUnlock()
	now := time.Now()
	for _, id := range envs {
		recs, err := fm.store.ListInterventions(ctx, InterventionFilter{EnvID: id, State: string(hitl.StateApproved)})
		if err != nil {
			fm.logger.Warn("list approved expiries failed", "vm", id, "err", err)
			continue
		}
		for _, rec := range recs {
			if rec.Kind != string(hitl.KindExpiry) {
				continue
			}
			in, err := fm.interventionFromRecord(rec)
			if err == nil && in.Signal != nil && in.Signal.ExtendedUntil != nil && in.Signal.ExtendedUntil.After(now) {
				fm.applyExtension(in)
			}
		}
	}
}

// defaultExtend is how long an approved expiry keeps the env alive when the
// approver names no duration: one more full window of whatever was about to
// reap it, bounded by the hitl maximum.
func (fm *FleetManager) defaultExtend(in hitl.Intervention) time.Duration {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	v, ok := fm.vms[in.EnvID]
	if !ok {
		return time.Hour
	}
	d := v.spec.IdleTimeout
	if d <= 0 {
		d = fm.defaultIdleTimeout
	}
	if in.Signal != nil && in.Signal.Reaper == hitl.ReaperMaxRuntime {
		d = v.spec.MaxRuntime
		if d <= 0 {
			d = fm.taskStuckTimeout
		}
	}
	if d <= 0 {
		d = time.Hour
	}
	return min(d, hitl.MaxTimeout)
}
