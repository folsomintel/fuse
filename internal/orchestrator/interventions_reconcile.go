package orchestrator

import (
	"context"
	"errors"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// reconcileInterventions is the backstop that keeps the hitl invariants
// true however a caller behaves: every pending intervention past its
// deadline is expired (applying on_timeout), and every pending intervention
// whose environment is gone or being destroyed is cancelled. readers expire
// on the spot too (GetIntervention), so this only matters for interventions
// nobody is waiting on, and for destroys that happened while the process
// was down.
func (fm *FleetManager) reconcileInterventions(ctx context.Context) {
	if fm.store == nil {
		return
	}
	recs, err := fm.store.ListInterventions(ctx, InterventionFilter{State: string(hitl.StatePending)})
	if err != nil {
		fm.logger.Warn("list pending interventions failed", "err", err)
		return
	}
	now := time.Now()
	for _, rec := range recs {
		if hitl.Expired(now, rec.Deadline) {
			if _, err := fm.expireIntervention(ctx, rec.ID); err != nil {
				fm.logger.Warn("expire intervention failed", "intervention", rec.ID, "err", err)
			}
			continue
		}
		fm.mu.RLock()
		v, ok := fm.vms[rec.EnvID]
		gone := !ok || v.state == VMStateDestroying
		fm.mu.RUnlock()
		if gone {
			if _, err := fm.resolveIntervention(ctx, rec.ID, func(in hitl.Intervention, now time.Time) (hitl.Intervention, error) {
				return hitl.Cancel(in, "environment destroyed", now)
			}); err != nil && !errors.Is(err, ErrInterventionResolved) {
				fm.logger.Warn("cancel intervention failed", "intervention", rec.ID, "vm", rec.EnvID, "err", err)
			}
		}
	}
}

// pendingInterventionEnvs returns the environments holding at least one
// pending intervention. a human deciding is activity, so the idle reaper
// skips these. ok is false when the store could not be read, in which case
// the caller must not reap anything it cannot rule out.
func (fm *FleetManager) pendingInterventionEnvs(ctx context.Context) (map[string]bool, bool) {
	out := make(map[string]bool)
	if fm.store == nil {
		return out, true
	}
	recs, err := fm.store.ListInterventions(ctx, InterventionFilter{State: string(hitl.StatePending)})
	if err != nil {
		fm.logger.Warn("list pending interventions for idle check failed", "err", err)
		return nil, false
	}
	for _, rec := range recs {
		out[rec.EnvID] = true
	}
	return out, true
}
