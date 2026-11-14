package durable

import (
	"context"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// opCheckpoint is the op label on the checkpoint chain's step spans and
// metrics.
const opCheckpoint = "checkpoint"

// Checkpoints is the part of the fleet the checkpoint chain handlers drive.
// *FleetManager satisfies it.
type Checkpoints interface {
	ChainBegin(ctx context.Context, vmID string, chain orchestrator.ChainState) (orchestrator.ChainPlan, error)
	ChainMovePlan(ctx context.Context, chain orchestrator.ChainState, plan orchestrator.ChainPlan, child orchestrator.SnapshotRecord) (orchestrator.ChainMove, error)
	ChainRetire(ctx context.Context, vmID string, chain orchestrator.ChainState) error
	ChainReset(ctx context.Context, vmID, reason string) error
	ChainAdvanced(ctx context.Context, vmID string, chain orchestrator.ChainState, diff bool) error
	// MirrorChain is not a step: it copies the object's chain into the
	// fleet's memory, and runs again on every replay.
	MirrorChain(vmID string, chain orchestrator.ChainState, pending bool)
}

// chainKey is the Environment object state holding the checkpoint chain.
const chainKey = "chain"

// chainObject is the checkpoint chain as the Environment object keeps it.
type chainObject struct {
	Chain orchestrator.ChainState `json:"chain"`
	// Pending is a tick's copy to the standby that has not ended yet.
	Pending *chainPending `json:"pending,omitempty"`
	// HeldUntil is a migrate's hold, unix ms. ticks skip the chain until it
	// passes or the migrate releases it.
	HeldUntil int64 `json:"held_until,omitempty"`
	// HeldBy is the hold's id, which its release has to name.
	HeldBy string `json:"held_by,omitempty"`
	// Dropped is a vm destroyed while a copy or a hold was in flight: what
	// the chain holds is removed when that ends.
	Dropped bool `json:"dropped,omitempty"`
}

// chainPending is a tick's checkpoint on its way to the standby.
type chainPending struct {
	Child   orchestrator.SnapshotRecord `json:"child"`
	Standby string                      `json:"standby"`
	Diff    bool                        `json:"diff,omitempty"`
	LocalID string                      `json:"local_id"`
	// Since is when the copy was sent, unix ms.
	Since int64 `json:"since"`
}

// pendingLapse is how long a chain waits on a copy that never reports back
// (an Artifacts invocation an operator killed) before it forgets it. it is
// well past the longest a move's pull can retry.
const pendingLapse = 2 * time.Hour

// forgetLapsedCopy drops a copy that has not reported back within
// pendingLapse, and the chain with it, deleting nothing: the move may still
// be reading the checkpoint and writing onto the base. it reports whether
// it did. retention gc takes the checkpoint later, artifact gc the base.
func forgetLapsedCopy(ctx restate.ObjectContext, fleet Checkpoints, st *chainObject, now int64) (bool, error) {
	if st.Pending == nil || now-st.Pending.Since < pendingLapse.Milliseconds() {
		return false, nil
	}
	vmID := restate.Key(ctx)
	if err := chainReset(ctx, fleet, vmID, "checkpoint copy "+st.Pending.LocalID+" never reported back"); err != nil {
		return false, err
	}
	if st.Chain.Base != "" {
		restate.ObjectSend(ctx, ArtifactPinObject, st.Chain.Base, "Release").Send(chainHolder(vmID))
	}
	st.Pending, st.Chain = nil, orchestrator.ChainState{}
	return true, nil
}

// chainHolder is the ArtifactPin holder of vmID's chain base.
func chainHolder(vmID string) string {
	return "chain:" + vmID
}

func loadChain(ctx restate.ObjectContext) (chainObject, error) {
	st, err := restate.Get[*chainObject](ctx, chainKey)
	if err != nil || st == nil {
		return chainObject{}, err
	}
	return *st, nil
}

// saveChain stores st and mirrors it into the fleet. an object holding
// nothing is cleared.
func saveChain(ctx restate.ObjectContext, fleet Checkpoints, st chainObject) {
	fleet.MirrorChain(restate.Key(ctx), st.Chain, st.Pending != nil)
	if st.Chain.Empty() && st.Pending == nil && st.HeldUntil == 0 && !st.Dropped {
		restate.Clear(ctx, chainKey)
		return
	}
	restate.Set(ctx, chainKey, st)
}

func nowMillis(ctx restate.Context) (int64, error) {
	return restate.Run(ctx, func(restate.RunContext) (int64, error) {
		return time.Now().UnixMilli(), nil
	}, restate.WithName("now"))
}

// checkpointTick is Environment/{vm_id}.Checkpoint, one step of the vm's
// checkpoint chain: a full checkpoint copied whole to a standby if the chain
// is empty, otherwise a diff against its head merged onto its base there.
// running on the vm's object serializes it with the vm's other lifecycle ops,
// and the chain lives in the object's state, so a restart keeps it. the copy
// is a one-way send to Artifacts, which reports back to ChainMoved, so the
// object is not held for as long as the bytes take to move.
func checkpointTick(ctx restate.ObjectContext, fleet Fleet, policy RetryPolicy) error {
	vmID := restate.Key(ctx)
	st, err := loadChain(ctx)
	if err != nil {
		return err
	}
	fleet.MirrorChain(vmID, st.Chain, st.Pending != nil)
	if st.Dropped {
		return nil
	}
	now, err := nowMillis(ctx)
	if err != nil {
		return err
	}
	forgot, err := forgetLapsedCopy(ctx, fleet, &st, now)
	if err != nil {
		return err
	}
	if forgot {
		saveChain(ctx, fleet, st)
		return nil
	}
	if st.Pending != nil {
		// the last tick's copy is still running.
		return nil
	}
	if st.HeldUntil > now {
		// a migrate of this vm is in progress.
		return nil
	}
	st.HeldUntil, st.HeldBy = 0, ""

	plan, err := Run(ctx, opCheckpoint, "plan", func(c context.Context) (orchestrator.ChainPlan, error) {
		return fleet.ChainBegin(c, vmID, st.Chain)
	})
	if err != nil {
		return err
	}
	if plan.Gone || !plan.Diff {
		if err := retireChain(ctx, fleet, vmID, st.Chain); err != nil {
			return err
		}
		st.Chain = orchestrator.ChainState{}
	}
	if plan.Gone || plan.Standby == "" {
		saveChain(ctx, fleet, st)
		return nil
	}

	child, err := snapshotSteps(ctx, fleet, policy, opCheckpoint, vmID, plan.Options)
	if err != nil {
		if !restate.IsTerminalError(err) {
			return err
		}
		if plan.Diff {
			// the diff was refused (another live snapshot came between) or
			// failed: start over with a full one next tick.
			if err := chainReset(ctx, fleet, vmID, err.Error()); err != nil {
				return err
			}
			if err := retireChain(ctx, fleet, vmID, st.Chain); err != nil {
				return err
			}
			st.Chain = orchestrator.ChainState{}
		}
		saveChain(ctx, fleet, st)
		return nil
	}

	pending := &chainPending{Child: child, Standby: plan.Standby, Diff: plan.Diff}
	mv, err := Run(ctx, opCheckpoint, "plan_move", func(c context.Context) (orchestrator.ChainMove, error) {
		mv, err := fleet.ChainMovePlan(c, st.Chain, plan, child)
		if err != nil {
			// planning reads the fleet only; what it refuses, a retry refuses.
			return mv, restate.ToTerminalError(err)
		}
		return mv, nil
	})
	if err != nil {
		if !restate.IsTerminalError(err) {
			return err
		}
		return chainMoved(ctx, fleet, st, pending, orchestrator.ChainMoveResult{Err: err.Error()})
	}
	if mv.Existing != "" {
		return chainMoved(ctx, fleet, st, pending, orchestrator.ChainMoveResult{ID: mv.Existing})
	}
	job := mv.Job
	job.Notify = vmID
	pending.LocalID, pending.Since = job.LocalID, now
	st.Pending = pending
	saveChain(ctx, fleet, st)
	restate.ObjectSend(ctx, ArtifactsObject, job.LocalID, "Move").Send(job)
	return nil
}

// chainMovedHandler is Environment/{vm_id}.ChainMoved, the end of a tick's
// copy to the standby. a result for any other copy than the pending one is
// stale and ignored.
func chainMovedHandler(ctx restate.ObjectContext, fleet Fleet, res orchestrator.ChainMoveResult) error {
	st, err := loadChain(ctx)
	if err != nil {
		return err
	}
	p := st.Pending
	if p == nil || p.LocalID != res.LocalID {
		fleet.MirrorChain(restate.Key(ctx), st.Chain, p != nil)
		return nil
	}
	st.Pending = nil
	return chainMoved(ctx, fleet, st, p, res)
}

// chainMoved applies the end of a tick's copy: on success the copy becomes
// the chain's base, pinned against gc, and the checkpoint its head; on
// failure the checkpoint is deleted and, for a diff, the chain retired, since
// its base may or may not have been merged and dropped. a copy that ends
// after its vm was destroyed is removed with everything else the chain held.
func chainMoved(ctx restate.ObjectContext, fleet Fleet, st chainObject, p *chainPending, res orchestrator.ChainMoveResult) error {
	vmID := restate.Key(ctx)
	if st.Dropped {
		if res.Err == "" {
			if err := retireChain(ctx, fleet, vmID, orchestrator.ChainState{Standby: p.Standby, Base: res.ID}); err != nil {
				return err
			}
		}
		if err := retireChain(ctx, fleet, vmID, orchestrator.ChainState{Head: p.Child}); err != nil {
			return err
		}
		return dropChainNow(ctx, fleet, vmID, st)
	}
	if res.Err != "" {
		if err := chainReset(ctx, fleet, vmID, res.Err); err != nil {
			return err
		}
		if err := retireChain(ctx, fleet, vmID, orchestrator.ChainState{Head: p.Child}); err != nil {
			return err
		}
		if p.Diff {
			if err := retireChain(ctx, fleet, vmID, st.Chain); err != nil {
				return err
			}
			st.Chain = orchestrator.ChainState{}
		}
		saveChain(ctx, fleet, st)
		return nil
	}

	if _, err := restate.Object[restate.Void](ctx, ArtifactPinObject, res.ID, "Pin").
		Request(PinRequest{Holder: chainHolder(vmID)}); err != nil {
		return err
	}
	old := st.Chain
	if p.Diff {
		// the merge consumed the old base, so only its pin is left to drop.
		if old.Base != "" && old.Base != res.ID {
			restate.ObjectSend(ctx, ArtifactPinObject, old.Base, "Release").Send(chainHolder(vmID))
		}
		st.Chain.Head, st.Chain.Base = p.Child, res.ID
		if err := retireChain(ctx, fleet, vmID, orchestrator.ChainState{Head: old.Head}); err != nil {
			return err
		}
	} else {
		st.Chain = orchestrator.ChainState{Standby: p.Standby, Head: p.Child, Base: res.ID, Bytes: p.Child.SizeBytes}
	}
	if err := runStep(ctx, opCheckpoint, "advanced", func(c context.Context) error {
		return fleet.ChainAdvanced(c, vmID, st.Chain, p.Diff)
	}); err != nil {
		return err
	}
	saveChain(ctx, fleet, st)
	return nil
}

func chainReset(ctx restate.ObjectContext, fleet Checkpoints, vmID, reason string) error {
	return runStep(ctx, opCheckpoint, "reset", func(c context.Context) error {
		return fleet.ChainReset(c, vmID, reason)
	})
}

// retireChain removes what chain holds and drops the pin on its base.
func retireChain(ctx restate.ObjectContext, fleet Checkpoints, vmID string, chain orchestrator.ChainState) error {
	if chain.Empty() {
		return nil
	}
	if chain.Base != "" {
		restate.ObjectSend(ctx, ArtifactPinObject, chain.Base, "Release").Send(chainHolder(vmID))
	}
	return runStep(ctx, opCheckpoint, "retire", func(c context.Context) error {
		return fleet.ChainRetire(c, vmID, chain)
	})
}

// dropChainNow retires the whole chain and clears it from the object.
func dropChainNow(ctx restate.ObjectContext, fleet Checkpoints, vmID string, st chainObject) error {
	if err := retireChain(ctx, fleet, vmID, st.Chain); err != nil {
		return err
	}
	fleet.MirrorChain(vmID, orchestrator.ChainState{}, false)
	restate.Clear(ctx, chainKey)
	return nil
}

// dropChain removes what a gone vm's chain holds: Environment/{vm_id}.RetireChain,
// and the last step of a destroy. a copy or a migrate's hold still in flight
// may be using the base, so the chain is only marked dropped then, and is
// removed when that ends.
func dropChain(ctx restate.ObjectContext, fleet Checkpoints) error {
	vmID := restate.Key(ctx)
	st, err := loadChain(ctx)
	if err != nil {
		return err
	}
	if st.Chain.Empty() && st.Pending == nil && st.HeldUntil == 0 {
		return nil
	}
	now, err := nowMillis(ctx)
	if err != nil {
		return err
	}
	if forgot, err := forgetLapsedCopy(ctx, fleet, &st, now); err != nil || forgot {
		if err == nil {
			fleet.MirrorChain(vmID, orchestrator.ChainState{}, false)
			restate.Clear(ctx, chainKey)
		}
		return err
	}
	if st.Pending != nil || st.HeldUntil > now {
		st.Dropped = true
		restate.Set(ctx, chainKey, st)
		// the copy's report or the hold's release normally ends it. this
		// delayed retry comes back when the hold or the copy's lapse is due,
		// so a migrate or a move that died with its process does not leave
		// the chain dropped forever.
		restate.ObjectSend(ctx, EnvironmentObject, vmID, "RetireChain").
			Send(restate.Void{}, restate.WithDelay(dropRetryDelay(st, now)))
		return nil
	}
	return dropChainNow(ctx, fleet, vmID, st)
}

// dropRetryDelay is how long until a dropped chain's hold lapses or its copy
// counts as never reporting back, whichever comes later, plus a margin.
func dropRetryDelay(st chainObject, now int64) time.Duration {
	due := st.HeldUntil
	if st.Pending != nil {
		due = max(due, st.Pending.Since+pendingLapse.Milliseconds())
	}
	return time.Duration(max(due-now, 0))*time.Millisecond + time.Minute
}

// holdChain is Environment/{vm_id}.HoldChain, taken by a delta migrate.
func holdChain(ctx restate.ObjectContext, fleet Checkpoints, lease time.Duration) (orchestrator.ChainHold, error) {
	st, err := loadChain(ctx)
	if err != nil {
		return orchestrator.ChainHold{}, err
	}
	if st.Pending != nil || st.Dropped {
		return orchestrator.ChainHold{Busy: true}, nil
	}
	now, err := nowMillis(ctx)
	if err != nil {
		return orchestrator.ChainHold{}, err
	}
	if st.HeldUntil > now {
		// another migrate holds it: one chain, one consumer of its base.
		return orchestrator.ChainHold{Busy: true}, nil
	}
	st.HeldUntil, st.HeldBy = now+lease.Milliseconds(), ctx.Request().ID
	saveChain(ctx, fleet, st)
	return orchestrator.ChainHold{Holder: st.HeldBy, Chain: st.Chain}, nil
}

// releaseChain is Environment/{vm_id}.ReleaseChain. reset forgets the chain
// without deleting its base, which the migrate consumed or will leave to gc.
// a release that names any other hold than the current one (one that lapsed
// and was taken again) changes nothing.
func releaseChain(ctx restate.ObjectContext, fleet Checkpoints, req ReleaseRequest) error {
	vmID := restate.Key(ctx)
	st, err := loadChain(ctx)
	if err != nil {
		return err
	}
	if req.Holder == "" || req.Holder != st.HeldBy {
		return nil
	}
	st.HeldUntil, st.HeldBy = 0, ""
	if req.Reset {
		if st.Chain.Base != "" {
			restate.ObjectSend(ctx, ArtifactPinObject, st.Chain.Base, "Release").Send(chainHolder(vmID))
		}
		st.Chain = orchestrator.ChainState{}
	}
	if st.Dropped && st.Pending == nil {
		return dropChainNow(ctx, fleet, vmID, st)
	}
	saveChain(ctx, fleet, st)
	return nil
}

// HoldRequest is the input of HoldChain.
type HoldRequest struct {
	Lease time.Duration `json:"lease"`
}

// ReleaseRequest is the input of ReleaseChain: the hold it ends, and whether
// the chain is forgotten with it.
type ReleaseRequest struct {
	Holder string `json:"holder"`
	Reset  bool   `json:"reset,omitempty"`
}

// Checkpoint runs Environment/{vmID}.Checkpoint and waits for it.
func (c *Client) Checkpoint(ctx context.Context, vmID string) error {
	_, err := ingress.Object[restate.Void, restate.Void](c.longIngress, EnvironmentObject, vmID, "Checkpoint").
		Request(ctx, restate.Void{})
	return err
}

// HoldChain runs Environment/{vmID}.HoldChain. it waits behind a tick that is
// taking its checkpoint, so it goes through the client with no request
// timeout; ctx bounds the wait.
func (c *Client) HoldChain(ctx context.Context, vmID string, lease time.Duration) (orchestrator.ChainHold, error) {
	return ingress.Object[HoldRequest, orchestrator.ChainHold](c.longIngress, EnvironmentObject, vmID, "HoldChain").
		Request(ctx, HoldRequest{Lease: lease})
}

// ReleaseChain enqueues Environment/{vmID}.ReleaseChain without waiting: it
// may queue behind a long op on the vm, and a hold that is not released only
// lapses on its lease.
func (c *Client) ReleaseChain(ctx context.Context, vmID, holder string, reset bool) error {
	_, err := ingress.ObjectSend[ReleaseRequest](c.ingress, EnvironmentObject, vmID, "ReleaseChain").
		Send(ctx, ReleaseRequest{Holder: holder, Reset: reset})
	return err
}

// SendRetireChain enqueues Environment/{vmID}.RetireChain.
func (c *Client) SendRetireChain(ctx context.Context, vmID string) error {
	_, err := ingress.ObjectSend[restate.Void](c.ingress, EnvironmentObject, vmID, "RetireChain").
		Send(ctx, restate.Void{})
	return err
}
