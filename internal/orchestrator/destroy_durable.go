package orchestrator

import (
	"context"
	"fmt"
	"time"
)

// destroy reasons carried on a durable destroy. they become the reason on
// the vm.destroying and vm.removed events.
const (
	DestroyReasonManual        = "manual_destroy"
	DestroyReasonTaskCompleted = "task_completed"
	DestroyReasonCreateCleanup = "create_cleanup"
	DestroyReasonRetry         = "teardown_retry"
	DestroyReasonExpired       = "expired"
)

// destroyStepTimeout bounds one attempt at the host destroy, the same bound
// destroyAndRemove puts on the whole teardown.
const destroyStepTimeout = 60 * time.Second

// DestroyRequest is the input of the durable Environment/{vm_id}.Destroy
// handler.
type DestroyRequest struct {
	Reason string `json:"reason"`
	// HostID names the host to destroy on when the fleet no longer tracks
	// the vm, as after a failed create, which drops the vm before cleanup.
	HostID string `json:"host_id,omitempty"`
}

// DestroyTarget is what DestroyBegin found.
type DestroyTarget struct {
	// Tracked is false when the fleet has no record of the vm.
	Tracked bool   `json:"tracked"`
	HostID  string `json:"host_id"`
}

// DurableDestroyer runs a destroy as the restate Environment/{vm_id}.Destroy
// handler. *durable.Client satisfies it. Destroy waits for the result, Send
// only enqueues it.
type DurableDestroyer interface {
	Destroy(ctx context.Context, vmID string, req DestroyRequest) error
	SendDestroy(ctx context.Context, vmID string, req DestroyRequest) error
}

// DestroyDeadLetterMetrics is an optional extension of ReconcileMetrics,
// counting durable destroys that ran out of retries.
type DestroyDeadLetterMetrics interface {
	DestroyDeadLettered()
}

// SetDurableDestroyer routes every destroy through restate. nil (mode off,
// the default) keeps the in-process teardown. set it before Start.
func (fm *FleetManager) SetDurableDestroyer(d DurableDestroyer) {
	fm.mu.Lock()
	fm.durableDestroy = d
	fm.mu.Unlock()
}

func (fm *FleetManager) durableDestroyer() DurableDestroyer {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	return fm.durableDestroy
}

// destroyAsync starts a teardown without waiting for it: a one-way send to
// restate, or the in-process destroyAndRemove in mode off. a send that fails
// leaves the vm in destroying, where reconcile re-sends it.
func (fm *FleetManager) destroyAsync(vmID, reason string) {
	d := fm.durableDestroyer()
	if d == nil {
		go fm.destroyAndRemove(vmID)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.SendDestroy(ctx, vmID, DestroyRequest{Reason: reason}); err != nil {
		fm.logger.Error("send durable destroy failed, reconcile will retry", "vm", vmID, "reason", reason, "err", err)
	}
}

// cleanupFailedCreate tears down a vm a failed create already dropped from
// the fleet. v is the dropped vm when its egress needs releasing, or nil.
func (fm *FleetManager) cleanupFailedCreate(provider Provider, v *vm, vmID, hostID string) {
	if d := fm.durableDestroyer(); d != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := d.SendDestroy(ctx, vmID, DestroyRequest{Reason: DestroyReasonCreateCleanup, HostID: hostID}); err != nil {
			fm.logger.Error("send durable cleanup destroy failed", "vm", vmID, "host", hostID, "err", err)
		}
		return
	}
	// TODO: Track this goroutine so Stop() can wait for it to finish
	// instead of leaking on shutdown.
	go func() {
		if v != nil {
			fm.releaseEgress(context.Background(), v)
		}
		if err := provider.Destroy(context.Background(), vmID); err != nil {
			fm.logger.Error("cleanup destroy failed", "vm", vmID, "err", err)
		}
	}()
}

// durableDestroyVM is DestroyVM in mode self or cloud. it checks what the
// in-process path checks before changing anything, then runs the handler and
// waits for it. a caller that gives up does not stop the destroy.
func (fm *FleetManager) durableDestroyVM(ctx context.Context, d DurableDestroyer, vmID string) error {
	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	hostID := ""
	if ok {
		hostID = v.hostID
	}
	fm.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	if _, err := fm.providerForVM(hostID); err != nil {
		return err
	}
	if err := d.Destroy(ctx, vmID, DestroyRequest{Reason: DestroyReasonManual}); err != nil {
		return fmt.Errorf("destroy vm %s: %w", vmID, err)
	}
	return nil
}

// the methods below are the steps of the durable destroy, in order. each runs
// inside a journaled restate step and must be safe to run again: a crash
// after a step's effect and before its journal entry replays it.

// DestroyBegin marks the vm destroying and fails its task. a vm the fleet does
// not track is reported untracked, not an error.
func (fm *FleetManager) DestroyBegin(ctx context.Context, vmID, reason string) (DestroyTarget, error) {
	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	if !ok {
		fm.mu.RUnlock()
		return DestroyTarget{}, nil
	}
	target := DestroyTarget{Tracked: true, HostID: v.hostID}
	priorTaskID := v.taskID
	assignedAt := v.createdAt
	// CompleteTask and the expiry detectors mark the vm destroying and emit
	// the event themselves before they send the destroy.
	already := v.state == VMStateDestroying
	fm.mu.RUnlock()

	// the task is failed before the vm drops it, so a crash between the two
	// writes replays with the task id still on the vm.
	if priorTaskID != "" && fm.store != nil {
		if err := fm.store.UpsertTask(ctx, TaskRecord{
			TaskID:     priorTaskID,
			VMID:       vmID,
			RunStatus:  TaskRunFailed,
			LastError:  "vm force destroyed",
			AssignedAt: assignedAt,
			UpdatedAt:  time.Now(),
		}); err != nil {
			return DestroyTarget{}, fmt.Errorf("persist task %s destroy state: %w", priorTaskID, err)
		}
		fm.appendEvent(ctx, "task", priorTaskID, "task.failed", map[string]any{"vm_id": vmID, "error": "vm force destroyed"})
	}

	fm.mu.Lock()
	v.state = VMStateDestroying
	v.taskID = ""
	v.updatedAt = time.Now()
	fm.mu.Unlock()
	if err := fm.persistVMByID(ctx, vmID); err != nil {
		return DestroyTarget{}, fmt.Errorf("persist vm %s destroying state: %w", vmID, err)
	}
	if !already {
		fm.appendEvent(ctx, "vm", vmID, "vm.destroying", map[string]any{"reason": reason})
		fm.publishStateChange(vmID, "")
	}
	fm.logger.InfoContext(ctx, "destroying vm", "vm", vmID, "reason", reason)
	return target, nil
}

// DestroyReleaseEgress releases the vm's proxy backend. best effort, like
// every teardown's egress release: it never fails the destroy.
func (fm *FleetManager) DestroyReleaseEgress(ctx context.Context, vmID string) error {
	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	fm.mu.RUnlock()
	if ok {
		fm.releaseEgress(ctx, v)
	} else {
		fm.releaseOrphanEgress(ctx, vmID)
	}
	return nil
}

// DestroyOnHost destroys the vm on its host. a vm the host does not have is
// already destroyed. an attempt restarts the vm's teardown clock, so
// reconcile does not re-send a destroy that is still retrying.
func (fm *FleetManager) DestroyOnHost(ctx context.Context, vmID, hostID string) error {
	fm.mu.Lock()
	if v, ok := fm.vms[vmID]; ok {
		v.updatedAt = time.Now()
	}
	fm.mu.Unlock()
	provider, err := fm.providerForVM(hostID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, destroyStepTimeout)
	defer cancel()
	if err := provider.Destroy(ctx, vmID); err != nil && !IsNotFound(err) {
		fm.logger.WarnContext(ctx, "destroy attempt failed", "vm", vmID, "host", hostID, "err", err)
		return err
	}
	return nil
}

// DestroyUnpublishIngress removes the vm's ingress routes.
func (fm *FleetManager) DestroyUnpublishIngress(ctx context.Context, vmID string) error {
	fm.unpublishIngress(ctx, vmID)
	return nil
}

// DestroyFinish returns the vm's host allocation, deletes its row and closes
// out what belonged to it: pending interventions, grants and scoped keys.
func (fm *FleetManager) DestroyFinish(ctx context.Context, vmID, reason string) error {
	fm.mu.Lock()
	v, ok := fm.vms[vmID]
	if ok {
		if v.hostID != "" {
			fm.deallocateOnHost(v.hostID, v.spec)
		}
		delete(fm.vms, vmID)
	}
	fm.mu.Unlock()
	if fm.store != nil {
		if err := fm.store.DeleteVM(ctx, vmID); err != nil {
			return fmt.Errorf("delete vm %s row: %w", vmID, err)
		}
	}
	if ok {
		fm.appendEvent(ctx, "vm", vmID, "vm.removed", map[string]any{"reason": reason})
		fm.publishTerminalEvent(vmID, VMStateDestroyed, "")
	}
	fm.cancelEnvInterventions(ctx, v, vmID, "environment destroyed")
	fm.dropEnvGrants(ctx, vmID)
	fm.revokeEnvKeys(ctx, vmID)
	return nil
}

// DestroyFailed records a destroy that ran out of retries: the vm moves to
// destroy_failed with the last error, and a dead letter is written. a later
// DELETE retries it.
func (fm *FleetManager) DestroyFailed(ctx context.Context, vmID, reason, lastErr string) error {
	fm.mu.Lock()
	v, ok := fm.vms[vmID]
	if ok {
		v.state = VMStateDestroyFailed
		v.err = lastErr
		v.updatedAt = time.Now()
	}
	fm.mu.Unlock()
	if ok {
		if err := fm.persistVMByID(ctx, vmID); err != nil {
			return fmt.Errorf("persist vm %s destroy_failed state: %w", vmID, err)
		}
		fm.publishStateChange(vmID, "")
	}
	fm.recordDeadLetter(ctx, DeadLetterRecord{
		Kind:     DeadLetterDestroy,
		EntityID: vmID,
		Reason:   lastErr,
		Payload:  dlPayload("reason", reason),
	})
	fm.appendEvent(ctx, "vm", vmID, "vm.destroy_failed", map[string]any{"reason": reason, "error": lastErr})
	if m, ok := fm.metrics.(DestroyDeadLetterMetrics); ok {
		m.DestroyDeadLettered()
	}
	fm.logger.ErrorContext(ctx, "destroy dead-lettered", "vm", vmID, "reason", reason, "err", lastErr)
	return nil
}
