package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// ErrEnvLocked is returned to agent exec and computer calls while a human
// holds the environment. the api answers 423 with the intervention id, and
// the sdks wait for that intervention to resolve before retrying.
var ErrEnvLocked = errors.New("environment locked")

// LockedError names the intervention holding the lock.
type LockedError struct {
	EnvID          string
	InterventionID string
	Kind           hitl.Kind
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("environment %s is locked by %s intervention %s; wait for it to resolve", e.EnvID, e.Kind, e.InterventionID)
}

func (e *LockedError) Unwrap() error { return ErrEnvLocked }

// agentLock is one held environment. it lasts no longer than the
// intervention behind it: Deadline is that intervention's deadline.
type agentLock struct {
	InterventionID string
	Kind           hitl.Kind
	Deadline       time.Time
}

// checkAgentLock refuses an agent call on a locked environment. a lock past
// its deadline no longer holds; its intervention expires on the next read
// or reconcile.
func (fm *FleetManager) checkAgentLock(envID string) error {
	fm.hitlMu.Lock()
	l, ok := fm.hitlLocks[envID]
	fm.hitlMu.Unlock()
	if !ok || hitl.Expired(time.Now(), l.Deadline) {
		return nil
	}
	return &LockedError{EnvID: envID, InterventionID: l.InterventionID, Kind: l.Kind}
}

// setLockLocked records in as holding its environment. hitlMu must be held.
func (fm *FleetManager) setLockLocked(in hitl.Intervention) {
	if fm.hitlLocks == nil {
		fm.hitlLocks = make(map[string]agentLock)
	}
	fm.hitlLocks[in.EnvID] = agentLock{InterventionID: in.ID, Kind: in.Kind, Deadline: in.Deadline}
}

// dropLockLocked releases envID if id holds it. hitlMu must be held.
func (fm *FleetManager) dropLockLocked(envID, id string) {
	if l, ok := fm.hitlLocks[envID]; ok && l.InterventionID == id {
		delete(fm.hitlLocks, envID)
	}
}

// syncLocks rebuilds the lock table from the pending interventions, which
// are the source of truth: after a restart nothing is held in memory, and
// a lock whose intervention resolved some other way must not linger. it
// runs under hitlMu so no transition lands between the read and the swap.
func (fm *FleetManager) syncLocks(ctx context.Context) {
	if fm.store == nil {
		return
	}
	fm.hitlMu.Lock()
	defer fm.hitlMu.Unlock()
	recs, err := fm.store.ListInterventions(ctx, InterventionFilter{State: string(hitl.StatePending)})
	if err != nil {
		fm.logger.Warn("list pending interventions for locks failed", "err", err)
		return
	}
	locks := make(map[string]agentLock)
	for _, rec := range recs {
		if rec.Kind != string(hitl.KindTakeover) && rec.Kind != string(hitl.KindStall) {
			continue
		}
		in, err := fm.interventionFromRecord(rec)
		if err != nil {
			fm.logger.Warn("read intervention for locks failed", "intervention", rec.ID, "err", err)
			continue
		}
		if in.Locks() {
			locks[in.EnvID] = agentLock{InterventionID: in.ID, Kind: in.Kind, Deadline: in.Deadline}
		}
	}
	fm.hitlLocks = locks
}
