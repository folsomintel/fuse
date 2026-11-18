package orchestrator

import (
	"context"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// maxExpirySends bounds the one-way expiry arms in flight, so a restate that
// accepts connections and never answers holds at most this many.
const maxExpirySends = 8

// expiryRecheck is how soon an expiry timer looks again at an environment a
// pending intervention holds, or one in a state it cannot judge yet.
const expiryRecheck = time.Minute

// DurableExpiry runs drain and the ttl, idle and drain-grace expiry of every
// environment on the restate Environment/{vm_id} object. *durable.Client
// satisfies it.
type DurableExpiry interface {
	// Drain runs Environment/{vmID}.Drain and waits for it. the drain keeps
	// running if ctx ends first.
	Drain(ctx context.Context, vmID string) error
	// SendArmExpiry enqueues Environment/{vmID}.Expire with no timer token:
	// it checks the vm now and schedules the next check, and any timer
	// already pending for the vm becomes a no-op.
	SendArmExpiry(ctx context.Context, vmID string) error
}

// ExpiryMetrics is an optional extension of ReconcileMetrics. with restate on
// the expiry timers, not reconcile, strike and reap environments, and report
// each here. reason is stuck_task, idle_timeout or drain_timeout.
type ExpiryMetrics interface {
	VMExpiry(reason string, reaped bool)
}

// SetDurableExpiry routes drain and environment expiry through restate. nil
// (mode off, the default) keeps the in-process drain and the reconcile
// detectors. set it before Start.
func (fm *FleetManager) SetDurableExpiry(d DurableExpiry) {
	fm.mu.Lock()
	fm.durableExpiries = d
	if fm.expirySendSlots == nil {
		fm.expirySendSlots = make(chan struct{}, maxExpirySends)
	}
	if fm.expiryArmed == nil {
		fm.expiryArmed = make(map[string]time.Time)
	}
	fm.mu.Unlock()
}

func (fm *FleetManager) durableExpiry() DurableExpiry {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.durableExpiries
}

// armExpiry hands vmID's expiry to restate, off the caller's goroutine. a vm
// whose arm is queued, in flight or done is not sent again; a send that fails
// is forgotten, so the next reconcile sends it again. the armed mark is the
// vm's create time, so a later vm that reuses the id is armed afresh. mode
// off does nothing.
func (fm *FleetManager) armExpiry(vmID string) {
	fm.sendArm(vmID, false)
}

// rearmExpiry is armExpiry for a vm whose deadline moved, such as an approved
// extension: it is sent even when the vm was armed before.
func (fm *FleetManager) rearmExpiry(vmID string) {
	fm.sendArm(vmID, true)
}

func (fm *FleetManager) sendArm(vmID string, again bool) {
	d := fm.durableExpiry()
	if d == nil {
		return
	}
	fm.mu.Lock()
	v, ok := fm.vms[vmID]
	if !ok || (!again && fm.expiryArmed[vmID].Equal(v.createdAt)) {
		fm.mu.Unlock()
		return
	}
	created := v.createdAt
	fm.expiryArmed[vmID] = created
	fm.mu.Unlock()
	fm.sendArmExpiry(d, vmID, created)
}

func (fm *FleetManager) sendArmExpiry(d DurableExpiry, vmID string, created time.Time) {
	fm.expirySends.Add(1)
	go func() {
		defer fm.expirySends.Done()
		fm.expirySendSlots <- struct{}{}
		defer func() { <-fm.expirySendSlots }()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := d.SendArmExpiry(ctx, vmID); err != nil {
			fm.logger.Error("send durable expiry failed, reconcile will retry", "vm", vmID, "err", err)
			fm.mu.Lock()
			if fm.expiryArmed[vmID].Equal(created) {
				delete(fm.expiryArmed, vmID)
			}
			fm.mu.Unlock()
		}
	}()
}

// armExpiries arms every running or draining vm not armed by this process
// yet: one whose create-time arm failed, one a fork or migrate brought up,
// and after a restart every vm, since the armed set is memory only. arming
// again is safe: it supersedes the timer restate already holds. it also
// forgets vms that are gone. mode off does nothing.
func (fm *FleetManager) armExpiries() {
	if fm.durableExpiry() == nil {
		return
	}
	var arm []string
	fm.mu.Lock()
	for id, v := range fm.vms {
		if (v.state == VMStateRunning || v.state == VMStateDraining) && !fm.expiryArmed[id].Equal(v.createdAt) {
			arm = append(arm, id)
		}
	}
	for id, created := range fm.expiryArmed {
		if v, ok := fm.vms[id]; !ok || !v.createdAt.Equal(created) {
			delete(fm.expiryArmed, id)
		}
	}
	fm.mu.Unlock()
	for _, id := range arm {
		fm.armExpiry(id)
	}
}

// ExpiryVerdict is what one expiry check settled. restate journals it.
type ExpiryVerdict struct {
	// Reap is a vm the check marked destroying, or found destroying: the
	// timer sends its destroy.
	Reap bool `json:"reap,omitempty"`
	// Strike is the reason a vm is past a window for the first time:
	// stuck_task or idle_timeout. the timer looks again after In and reaps
	// it if it is still past the same window.
	Strike string `json:"strike,omitempty"`
	// In is when to look again. zero with Reap and Strike unset means
	// nothing is left to watch.
	In time.Duration `json:"in,omitempty"`
}

// expiryView is what an expiry check reads of a vm, copied under fm.mu.
type expiryView struct {
	state          VMState
	taskID         string
	createdAt      time.Time
	updatedAt      time.Time
	lastActivityAt time.Time
	attachSessions int
	maxRuntime     time.Duration
	idleTimeout    time.Duration
	hitl           *hitl.Spec
}

// ExpiryCheck is the step of the durable Environment/{vm_id}.Expire timer: it
// judges vmID by the rules reconcile applies in mode off. strike is the
// window the previous check struck the vm for, or empty; a vm is reaped only
// on a second consecutive check past the same window, as reconcile counts
// idle and runtime strikes apart.
//
//   - running: past its max_runtime (with a task) or its idle timeout, the
//     first check strikes and the next reaps. a pending expiry
//     intervention holds the runtime ceiling, any pending intervention holds
//     idle, and an approved extension holds both until it ends.
//   - draining: past the draining timeout it is reaped at once, unless a
//     human holds it or its hitl policy gates destroy.
//   - destroying: the destroy is sent again. this is the replay of a check
//     that reaped before a crash; a destroy is safe to send twice.
//
// it is safe to run again: a replay finds the vm where the first run left
// it.
func (fm *FleetManager) ExpiryCheck(ctx context.Context, vmID, strike string) (ExpiryVerdict, error) {
	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	var e expiryView
	if ok {
		e = expiryView{
			state:          v.state,
			taskID:         v.taskID,
			createdAt:      v.createdAt,
			updatedAt:      v.updatedAt,
			lastActivityAt: v.lastActivityAt,
			attachSessions: v.attachSessions,
			maxRuntime:     v.spec.MaxRuntime,
			idleTimeout:    v.spec.IdleTimeout,
			hitl:           v.spec.Hitl,
		}
	}
	fm.mu.RUnlock()
	if !ok {
		return ExpiryVerdict{}, nil
	}

	switch e.state {
	case VMStateDestroying:
		return ExpiryVerdict{Reap: true}, nil
	case VMStateProvisioning:
		return ExpiryVerdict{In: expiryRecheck}, nil
	case VMStateRunning, VMStateDraining:
	default:
		// destroy_failed: a DELETE owns it now.
		return ExpiryVerdict{}, nil
	}

	pendingAny, pendingExpiry, err := fm.pendingFor(ctx, vmID)
	if err != nil {
		return ExpiryVerdict{}, err
	}
	now := time.Now()
	var extendedUntil time.Time
	fm.hitlMu.Lock()
	if st, ok := fm.hitlSignals[vmID]; ok {
		extendedUntil = st.extendedUntil
	}
	fm.hitlMu.Unlock()
	if e.state == VMStateDraining {
		return fm.checkDraining(ctx, vmID, e, now, pendingAny, extendedUntil)
	}
	if now.Before(extendedUntil) {
		return ExpiryVerdict{In: extendedUntil.Sub(now)}, nil
	}

	// when each reaper is next worth a look, zero for one that is off.
	var runtimeAt, idleAt time.Time
	var ceiling, timeout time.Duration
	if e.taskID != "" {
		ceiling = e.maxRuntime
		if ceiling <= 0 {
			ceiling = fm.taskStuckTimeout
		}
		if ceiling > 0 {
			runtimeAt = e.createdAt.Add(ceiling)
			if pendingExpiry {
				runtimeAt = now.Add(expiryRecheck)
			}
		}
	}
	timeout = e.idleTimeout
	if timeout <= 0 {
		timeout = fm.defaultIdleTimeout
	}
	since := e.lastActivityAt
	if since.IsZero() {
		since = e.createdAt
	}
	if timeout > 0 {
		switch {
		case pendingAny:
			idleAt = now.Add(expiryRecheck)
		case e.attachSessions > 0:
			// a detach restarts the idle clock, so it cannot be due
			// sooner than a full timeout from now.
			idleAt = now.Add(timeout)
		default:
			idleAt = since.Add(timeout)
		}
	}

	var ex expiry
	switch {
	case !runtimeAt.IsZero() && !runtimeAt.After(now):
		ex = stuckExpiry(now.Sub(e.createdAt), ceiling)
	case !idleAt.IsZero() && !idleAt.After(now):
		ex = idleExpiry(now.Sub(since), timeout)
	default:
		next := runtimeAt
		if next.IsZero() || (!idleAt.IsZero() && idleAt.Before(next)) {
			next = idleAt
		}
		if next.IsZero() {
			return ExpiryVerdict{}, nil
		}
		return ExpiryVerdict{In: next.Sub(now)}, nil
	}

	if strike != ex.reasonCode {
		if ex.reasonCode == "stuck_task" {
			fm.noteStuckSuspected(vmID, e.taskID, ex.age, ex.ceiling, 1)
		} else {
			fm.noteIdleSuspected(vmID, e.taskID, ex.age, ex.ceiling, 1)
		}
		fm.noteExpiryMetric(ex.reasonCode, false)
		return ExpiryVerdict{Strike: ex.reasonCode, In: fm.reconcileInterval}, nil
	}
	marked, err := fm.markExpiredVMDurable(ctx, vmID, e.taskID, ex)
	if err != nil {
		return ExpiryVerdict{}, err
	}
	if !marked {
		// the vm moved on between the read and the teardown.
		return ExpiryVerdict{In: expiryRecheck}, nil
	}
	fm.noteExpiryMetric(ex.reasonCode, true)
	return ExpiryVerdict{Reap: true}, nil
}

// checkDraining is ExpiryCheck for a draining vm.
func (fm *FleetManager) checkDraining(ctx context.Context, vmID string, e expiryView, now time.Time, pendingAny bool, extendedUntil time.Time) (ExpiryVerdict, error) {
	if destroyGated(e.hitl) {
		// a human owns this destroy, and a rejected one stays rejected.
		return ExpiryVerdict{}, nil
	}
	if now.Before(extendedUntil) {
		return ExpiryVerdict{In: extendedUntil.Sub(now)}, nil
	}
	if pendingAny {
		return ExpiryVerdict{In: expiryRecheck}, nil
	}
	age := now.Sub(e.updatedAt)
	if age < fm.drainingTimeout {
		return ExpiryVerdict{In: fm.drainingTimeout - age}, nil
	}
	marked, err := fm.markExpiredVMDurable(ctx, vmID, e.taskID, fm.drainExpiry(age))
	if err != nil {
		return ExpiryVerdict{}, err
	}
	if !marked {
		return ExpiryVerdict{In: expiryRecheck}, nil
	}
	fm.noteDrainExpired(vmID, e.taskID, age)
	fm.noteExpiryMetric("drain_timeout", true)
	return ExpiryVerdict{Reap: true}, nil
}

// pendingFor reports whether vmID has a pending intervention, and whether
// one of them is an expiry.
func (fm *FleetManager) pendingFor(ctx context.Context, vmID string) (anyPending, expiryPending bool, err error) {
	if fm.store == nil {
		return false, false, nil
	}
	recs, err := fm.store.ListInterventions(ctx, InterventionFilter{EnvID: vmID, State: string(hitl.StatePending)})
	if err != nil {
		return false, false, err
	}
	for _, rec := range recs {
		anyPending = true
		if rec.Kind == string(hitl.KindExpiry) {
			expiryPending = true
		}
	}
	return anyPending, expiryPending, nil
}

func (fm *FleetManager) noteExpiryMetric(reason string, reaped bool) {
	if m, ok := fm.metrics.(ExpiryMetrics); ok {
		m.VMExpiry(reason, reaped)
	}
}
