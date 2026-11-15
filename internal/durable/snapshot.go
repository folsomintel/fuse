package durable

import (
	"context"
	"errors"
	"fmt"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// op labels on the step spans and metrics.
const (
	opSnapshot  = "snapshot"
	opRestore   = "restore"
	opSnapshotG = "snapshot_gc"
)

// Snapshots is the part of the fleet the snapshot, restore and snapshot gc
// handlers drive, one method per step. *FleetManager satisfies it.
type Snapshots interface {
	SnapshotBegin(ctx context.Context, vmID string, opts orchestrator.SnapshotOptions) (orchestrator.SnapshotPlan, error)
	SnapshotCheckpoint(ctx context.Context, vmID string, plan orchestrator.SnapshotPlan) (orchestrator.Checkpoint, error)
	SnapshotRecordCreating(ctx context.Context, vmID string, plan orchestrator.SnapshotPlan, created orchestrator.Checkpoint) (orchestrator.SnapshotRecord, error)
	SnapshotList(ctx context.Context, vmID, snapshotID string) (orchestrator.Checkpoint, error)
	SnapshotReady(ctx context.Context, record orchestrator.SnapshotRecord, listed orchestrator.Checkpoint) (orchestrator.SnapshotRecord, error)
	SnapshotAbort(ctx context.Context, vmID string, plan orchestrator.SnapshotPlan, created orchestrator.Checkpoint, cause string) error

	RestoreValidate(ctx context.Context, vmID, snapshotID string) (orchestrator.SnapshotRecord, error)
	RestoreBegin(ctx context.Context, record orchestrator.SnapshotRecord) error
	RestoreOnHost(ctx context.Context, vmID, snapshotID string) error
	RestoreFinish(ctx context.Context, record orchestrator.SnapshotRecord) error
	RestoreFailed(ctx context.Context, record orchestrator.SnapshotRecord, lastErr string) error

	SnapshotCollect(ctx context.Context, snapshotID string) error
	SnapshotCollectFailed(ctx context.Context, snapshotID, lastErr string) error
}

// fleetErrorBase is the first error code a fleet sentinel error crosses
// restate as: orchestrator.SnapshotFleetErrors[i] is fleetErrorBase+i. the
// api maps the sentinel, so the code only has to bring it back. codes below
// 480 are restate's own (404 not found, 470 not ready).
const fleetErrorBase restate.Code = 480

// permanent marks err terminal when a retry cannot change it, carrying a
// fleet sentinel as its code. the code is journaled with the error, so a
// replay that returns the recorded failure still carries it.
func permanent(err error) error {
	if err == nil || !orchestrator.PermanentSnapshotError(err) {
		return err
	}
	for i, s := range orchestrator.SnapshotFleetErrors {
		if errors.Is(err, s) {
			return restate.ToTerminalError(err, restate.WithErrorCode(fleetErrorBase+restate.Code(i)))
		}
	}
	return restate.ToTerminalError(err)
}

// fleetError turns a handler's failure back into the fleet sentinel its code
// names, so the api answers it with the same status as in mode off.
func fleetError(err error) error {
	var ierr ingress.Error
	if !errors.As(err, &ierr) {
		return err
	}
	i := int(ierr.Code()) - int(fleetErrorBase)
	if i < 0 || i >= len(orchestrator.SnapshotFleetErrors) {
		return err
	}
	return fmt.Errorf("%w: %s", orchestrator.SnapshotFleetErrors[i], ierr.Message())
}

// retryUnder is policy as the options of one step.
func retryUnder(policy RetryPolicy) []restate.RunOption {
	return []restate.RunOption{
		restate.WithInitialRetryInterval(policy.InitialInterval),
		restate.WithMaxRetryInterval(policy.MaxInterval),
		restate.WithMaxRetryAttempts(uint(policy.MaxAttempts)),
	}
}

// snapshot is Environment/{vm_id}.Snapshot.
func snapshot(ctx restate.ObjectContext, fleet Snapshots, policy RetryPolicy, opts orchestrator.SnapshotOptions) (orchestrator.SnapshotRecord, error) {
	return snapshotSteps(ctx, fleet, policy, opSnapshot, restate.Key(ctx), opts)
}

// snapshotSteps takes one snapshot of vmID, each step journaled: begin
// (checks and quota), checkpoint on the host, the row in creating, the
// host's listing, ready. the checkpoint is keyed, so a retry or a replay
// after a crash gets the first checkpoint back and the row is written for
// it. a record that cannot be finished deletes the host artifact and marks
// the row error, so nothing is left on the host that no row tracks. the
// checkpoint chain takes its snapshots through the same steps.
func snapshotSteps(ctx restate.ObjectContext, fleet Snapshots, policy RetryPolicy, op, vmID string, opts orchestrator.SnapshotOptions) (orchestrator.SnapshotRecord, error) {
	retry := retryUnder(policy)
	plan, err := Run(ctx, op, "begin", func(c context.Context) (orchestrator.SnapshotPlan, error) {
		plan, err := fleet.SnapshotBegin(c, vmID, opts)
		return plan, permanent(err)
	})
	if err != nil {
		return orchestrator.SnapshotRecord{}, err
	}
	created, err := Run(ctx, op, "checkpoint", func(c context.Context) (orchestrator.Checkpoint, error) {
		created, err := fleet.SnapshotCheckpoint(c, vmID, plan)
		return created, permanent(err)
	}, retry...)
	if err != nil {
		return orchestrator.SnapshotRecord{}, err
	}

	record, err := Run(ctx, op, "record", func(c context.Context) (orchestrator.SnapshotRecord, error) {
		return fleet.SnapshotRecordCreating(c, vmID, plan, created)
	}, retry...)
	var listed orchestrator.Checkpoint
	if err == nil {
		listed, err = Run(ctx, op, "list", func(c context.Context) (orchestrator.Checkpoint, error) {
			return fleet.SnapshotList(c, vmID, created.ID)
		}, retry...)
	}
	if err == nil {
		record, err = Run(ctx, op, "ready", func(c context.Context) (orchestrator.SnapshotRecord, error) {
			return fleet.SnapshotReady(c, record, listed)
		}, retry...)
	}
	if err != nil {
		if !restate.IsTerminalError(err) {
			// suspension: restate resumes the snapshot here.
			return orchestrator.SnapshotRecord{}, err
		}
		cause := err.Error()
		if _, aerr := Run(ctx, op, "abort", func(c context.Context) (restate.Void, error) {
			return restate.Void{}, fleet.SnapshotAbort(c, vmID, plan, created, cause)
		}, retry...); aerr != nil && !restate.IsTerminalError(aerr) {
			return orchestrator.SnapshotRecord{}, aerr
		}
		return orchestrator.SnapshotRecord{}, err
	}
	return record, nil
}

// restore is Environment/{vm_id}.Restore: validate (the row and the host's
// copy), restoring, the restore on the host, ready. a restart replays to the
// first unfinished step, so the restore completes instead of leaving the row
// in restoring. a host restore that fails for good marks the row error.
func restore(ctx restate.ObjectContext, fleet Snapshots, policy RetryPolicy, snapshotID string) error {
	vmID := restate.Key(ctx)
	retry := retryUnder(policy)
	record, err := Run(ctx, opRestore, "validate", func(c context.Context) (orchestrator.SnapshotRecord, error) {
		record, err := fleet.RestoreValidate(c, vmID, snapshotID)
		return record, permanent(err)
	})
	if err != nil {
		return err
	}
	if err := runStep(ctx, opRestore, "restoring", func(c context.Context) error {
		return fleet.RestoreBegin(c, record)
	}, retry...); err != nil {
		return err
	}
	hostErr := runStep(ctx, opRestore, "restore", func(c context.Context) error {
		return permanent(fleet.RestoreOnHost(c, vmID, snapshotID))
	}, retry...)
	if hostErr != nil {
		if !restate.IsTerminalError(hostErr) {
			return hostErr
		}
		if err := runStep(ctx, opRestore, "failed", func(c context.Context) error {
			return fleet.RestoreFailed(c, record, hostErr.Error())
		}, retry...); err != nil && !restate.IsTerminalError(err) {
			return err
		}
		return hostErr
	}
	return runStep(ctx, opRestore, "finish", func(c context.Context) error {
		return fleet.RestoreFinish(c, record)
	}, retry...)
}

// collectSnapshot is Environment/{vm_id}.CollectSnapshot, the deletion of
// one snapshot retention gc marked deleting. the deletion retries under the
// Environment policy; when that runs out the row moves to error, and a later
// sweep marks it again.
func collectSnapshot(ctx restate.ObjectContext, fleet Snapshots, policy RetryPolicy, snapshotID string) error {
	retry := retryUnder(policy)
	err := runStep(ctx, opSnapshotG, "collect", func(c context.Context) error {
		return fleet.SnapshotCollect(c, snapshotID)
	}, retry...)
	if err == nil || !restate.IsTerminalError(err) {
		return err
	}
	lastErr := err.Error()
	if ferr := runStep(ctx, opSnapshotG, "collect_failed", func(c context.Context) error {
		return fleet.SnapshotCollectFailed(c, snapshotID, lastErr)
	}, retry...); ferr != nil && !restate.IsTerminalError(ferr) {
		return ferr
	}
	// the failure is recorded on the row; the invocation itself is done.
	return nil
}

// runStep is Run for a step with no result, under op.
func runStep(ctx restate.Context, op, name string, fn func(context.Context) error, opts ...restate.RunOption) error {
	_, err := Run(ctx, op, name, func(c context.Context) (restate.Void, error) {
		return restate.Void{}, fn(c)
	}, opts...)
	return err
}

// Snapshot runs Environment/{vmID}.Snapshot and waits for it. a live
// snapshot writes the whole memory image, so it goes through the client with
// no request timeout of its own; ctx bounds the wait.
func (c *Client) Snapshot(ctx context.Context, vmID string, opts orchestrator.SnapshotOptions) (orchestrator.SnapshotRecord, error) {
	rec, err := ingress.Object[orchestrator.SnapshotOptions, orchestrator.SnapshotRecord](c.longIngress, EnvironmentObject, vmID, "Snapshot").
		Request(ctx, opts, callerKey(ctx)...)
	return rec, fleetError(err)
}

// Restore runs Environment/{vmID}.Restore and waits for it.
func (c *Client) Restore(ctx context.Context, vmID, snapshotID string) error {
	_, err := ingress.Object[string, restate.Void](c.longIngress, EnvironmentObject, vmID, "Restore").
		Request(ctx, snapshotID, callerKey(ctx)...)
	return fleetError(err)
}

// SendCollectSnapshot enqueues Environment/{vmID}.CollectSnapshot. restate
// runs one invocation per key.
func (c *Client) SendCollectSnapshot(ctx context.Context, vmID, snapshotID, key string) error {
	_, err := ingress.ObjectSend[string](c.ingress, EnvironmentObject, vmID, "CollectSnapshot").
		Send(ctx, snapshotID, restate.WithIdempotencyKey(key))
	return err
}

var _ orchestrator.DurableSnapshots = (*Client)(nil)
