package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// orphanDestroyTimeout bounds one orphan destroy, the same 60s ceiling
// destroyStepTimeout puts on a durable host destroy. without it a single host call (for example a delete
// queued behind a runaway exec holding the agent's vm lock) blocks the whole
// reconcile cycle until the hostwire header timeout, minutes later.
const orphanDestroyTimeout = 60 * time.Second

// reconcileOrphans destroys provider VMs that are not tracked in the fleet.
// Repeated destroy failures are counted per-VM in fm.orphanRetries; once
// the count exceeds orphanDestroyMaxRetries the entry is dead-lettered and
// skipped on subsequent cycles until it disappears from the provider.
//
// on restate each orphan is a send to Environment/{vm}.DestroyOrphan
// instead, which holds the count in object state; fm.orphanRetries is then
// only the record of which orphans the handler has dead-lettered.
func (fm *FleetManager) reconcileOrphans(ctx context.Context, listing vmListing, tracked map[string]bool, summary *ReconcileSummary) {
	envs, hostOf := listing.envs, listing.hostOf
	durable := fm.durableOrphans()
	if durable != nil {
		fm.takeOrphanOutcomes(summary)
	}

	// Collect the current orphan set so we can clear stale retry counters.
	currentOrphans := make(map[string]bool, len(envs))

	for _, e := range envs {
		name := e.Name()
		if tracked[name] {
			continue
		}
		currentOrphans[name] = true

		fm.mu.Lock()
		retries := fm.orphanRetries[name]
		fm.mu.Unlock()

		if retries >= fm.orphanDestroyMaxRetries {
			// Already dead-lettered — log at debug volume and skip.
			fm.logger.Debug("orphan dead-lettered, skipping", "vm", name, "retries", retries)
			continue
		}
		if durable != nil {
			fm.sendOrphanDestroy(durable, name, hostOf[name], EnvEpoch(e))
			continue
		}

		fm.logger.Warn("orphan vm detected, destroying", "vm", name, "attempt", retries+1)
		provider, err := fm.providerForVM(hostOf[name])
		if err != nil {
			fm.logger.Error("orphan provider not found", "vm", name, "err", err)
			summary.OrphansFailed++
			continue
		}
		destroyCtx, cancel := context.WithTimeout(WithVMEpoch(ctx, EnvEpoch(e)), fm.orphanDestroyTimeout)
		err = destroyOrphan(destroyCtx, provider, name)
		cancel()
		if err != nil {
			fm.logger.Error("orphan destroy failed", "vm", name, "err", err, "attempt", retries+1)
			summary.OrphansFailed++

			fm.mu.Lock()
			fm.orphanRetries[name] = retries + 1
			attempt := fm.orphanRetries[name]
			fm.mu.Unlock()

			if attempt >= fm.orphanDestroyMaxRetries {
				fm.recordDeadLetter(ctx, DeadLetterRecord{
					Kind:       DeadLetterOrphanDestroy,
					EntityID:   name,
					Reason:     fmt.Sprintf("orphan destroy failed after %d attempts: %v", attempt, err),
					RetryCount: attempt,
					Payload:    dlPayload("last_error", err.Error()),
				})
				summary.OrphansDeadLettered++
				fm.logger.Error("orphan dead-lettered after max retries",
					"vm", name,
					"retries", attempt,
					"err", err,
				)
				fm.appendEventBackground("vm", name, "vm.orphan_dead_lettered", map[string]any{
					"retries": attempt,
					"error":   err.Error(),
				})
			}
			continue
		}

		// an orphan has no record of which egress backend it used, if any;
		// ask them all. after the vm is gone, so a backend bound to its tap
		// has nothing left to serve.
		fm.releaseOrphanEgress(ctx, name)

		// Success — clear retry counter.
		fm.mu.Lock()
		delete(fm.orphanRetries, name)
		fm.mu.Unlock()
		summary.OrphansDestroyed++
	}

	// Clear retry counters for orphans that no longer exist in the provider
	// (either because they were destroyed or disappeared on their own). an
	// orphan on a host that did not answer is not listed either, so nothing
	// is cleared while one is down.
	if len(listing.unreachable) > 0 {
		return
	}
	fm.mu.Lock()
	for name := range fm.orphanRetries {
		if !currentOrphans[name] {
			delete(fm.orphanRetries, name)
		}
	}
	fm.mu.Unlock()
}

// noteStuckSuspected records a first strike against a vm past its runtime
// ceiling.
func (fm *FleetManager) noteStuckSuspected(vmID, taskID string, age, ceiling time.Duration, strike int) {
	fm.logger.Warn("vm exceeded runtime ceiling, suspected stuck",
		"vm", vmID,
		"task", taskID,
		"age", age,
		"ceiling", ceiling,
		"strike", strike,
	)
	fm.appendEventBackground("task", taskID, "task.stuck_suspected", map[string]any{
		"vm_id":   vmID,
		"age_s":   int(age.Seconds()),
		"ceiling": ceiling.String(),
		"strike":  strike,
	})
}

// noteIdleSuspected records a first strike against a vm past its idle
// timeout.
func (fm *FleetManager) noteIdleSuspected(vmID, taskID string, idle, timeout time.Duration, strike int) {
	fm.logger.Warn("vm exceeded idle timeout, suspected idle",
		"vm", vmID,
		"task", taskID,
		"idle", idle,
		"timeout", timeout,
		"strike", strike,
	)
	fm.appendEventBackground("vm", vmID, "vm.idle_suspected", map[string]any{
		"task_id": taskID,
		"idle_s":  int(idle.Seconds()),
		"timeout": timeout.String(),
		"strike":  strike,
	})
}

// teardownRetryAfter is how long a VM may sit in Destroying while its
// provider still lists it before reconcile tries the destroy again. it is
// longer than the 60s bound on a host destroy step and on a DELETE request,
// so a destroy still in flight is never doubled up.
const teardownRetryAfter = 2 * time.Minute

// retryFailedTeardowns re-runs the destroy of VMs left in Destroying whose
// provider still lists them. a one-way destroy whose send failed (from
// destroyAsync) leaves the VM there; without a retry nothing else touches
// it, since the orphan pass skips tracked VMs and reapDestroyedVMs waits for
// the provider to drop it, so the VM and its host allocation would outlive
// the host's recovery.
func (fm *FleetManager) retryFailedTeardowns(providerVMs map[string]bool) {
	now := time.Now()

	fm.mu.Lock()
	retry := make([]string, 0)
	for id, v := range fm.vms {
		if v.state != VMStateDestroying || !providerVMs[id] {
			continue
		}
		if now.Sub(v.updatedAt) <= teardownRetryAfter {
			continue
		}
		// restart the clock so the next attempt waits a full window.
		v.updatedAt = now
		retry = append(retry, id)
	}
	fm.mu.Unlock()

	for _, id := range retry {
		fm.logger.Warn("vm still present after destroy, retrying teardown", "vm", id)
		fm.appendEventBackground("vm", id, "vm.destroy_retried", nil)
		fm.destroyAsync(id, DestroyReasonRetry)
	}
}

// destroyGated reports whether an env's hitl policy holds or denies destroy.
// such an env is never reaped from draining: a human owns that destroy, and a
// rejected one must stay rejected rather than be redone by the reaper.
func destroyGated(spec *hitl.Spec) bool {
	if spec == nil {
		return false
	}
	switch spec.EvaluateLifecycle(hitl.ActionDestroy).Mode() {
	case hitl.ModeAsk, hitl.ModeDeny:
		return true
	}
	return false
}

// expiry describes why ExpiryCheck is tearing a VM down. the runtime
// ceiling, idle timeout and draining timeout differ only in these strings
// and counters, so they share markExpiredVM below.
type expiry struct {
	from       VMState        // the state the vm must still be in to be torn down
	reasonCode string         // short code carried on events and dead letters
	kind       DeadLetterKind // dead-letter classification
	reason     string         // human-readable, stored on the vm and the task
	age        time.Duration  // observed age or idle duration
	ceiling    time.Duration  // the window that was exceeded
}

func stuckExpiry(age, ceiling time.Duration) expiry {
	return expiry{
		from:       VMStateRunning,
		reasonCode: "stuck_task",
		kind:       DeadLetterStuckTask,
		reason:     fmt.Sprintf("task stuck: runtime %s exceeded ceiling %s with no state transitions", age.Round(time.Second), ceiling),
		age:        age,
		ceiling:    ceiling,
	}
}

func idleExpiry(idle, timeout time.Duration) expiry {
	return expiry{
		from:       VMStateRunning,
		reasonCode: "idle_timeout",
		kind:       DeadLetterIdleTimeout,
		reason:     fmt.Sprintf("environment idle: no exec or attach for %s, exceeding idle timeout %s", idle.Round(time.Second), timeout),
		age:        idle,
		ceiling:    timeout,
	}
}

// drainExpiry is a vm left draining for age, past the draining timeout.
func (fm *FleetManager) drainExpiry(age time.Duration) expiry {
	return expiry{
		from:       VMStateDraining,
		reasonCode: "drain_timeout",
		kind:       DeadLetterDrainTimeout,
		reason:     fmt.Sprintf("environment left draining for %s with no DELETE, exceeding draining timeout %s", age.Round(time.Second), fm.drainingTimeout),
		age:        age,
		ceiling:    fm.drainingTimeout,
	}
}

// noteDrainExpired records a draining vm torn down at its timeout.
func (fm *FleetManager) noteDrainExpired(vmID, taskID string, age time.Duration) {
	fm.appendEventBackground("vm", vmID, "vm.drain_expired", map[string]any{
		"task_id":    taskID,
		"draining_s": int(age.Seconds()),
		"timeout":    fm.drainingTimeout.String(),
	})
}

// expiredMark is what claimExpired changed on a vm, so a durable check whose
// writes fail can put it back.
type expiredMark struct {
	// claimedAt is the updatedAt the claim set; a vm that no longer carries
	// it was moved on by someone else, whose change is not undone.
	claimedAt  time.Time
	state      VMState
	taskID     string
	err        string
	updatedAt  time.Time
	assignedAt time.Time
}

// claimExpired moves a VM still in e.from with taskID to destroying, in
// memory only. It reports false when the VM had already moved on.
func (fm *FleetManager) claimExpired(vmID, taskID string, e expiry) (expiredMark, bool) {
	fm.mu.Lock()
	v, ok := fm.vms[vmID]
	if !ok || v.state != e.from || v.taskID != taskID {
		fm.mu.Unlock()
		return expiredMark{}, false
	}
	m := expiredMark{state: v.state, taskID: v.taskID, err: v.err, updatedAt: v.updatedAt, assignedAt: v.createdAt}
	v.state = VMStateDestroying
	v.taskID = ""
	v.err = e.reason
	v.updatedAt = time.Now()
	m.claimedAt = v.updatedAt
	fm.mu.Unlock()

	fm.logger.Error("tearing down expired vm",
		"vm", vmID,
		"task", taskID,
		"reason", e.reasonCode,
		"age", e.age,
		"ceiling", e.ceiling,
	)
	return m, true
}

// unclaimExpired puts back what claimExpired changed, unless something else
// has touched the vm since (a DELETE, reconcile finding it gone).
func (fm *FleetManager) unclaimExpired(vmID string, m expiredMark) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	v, ok := fm.vms[vmID]
	if !ok || v.state != VMStateDestroying || !v.updatedAt.Equal(m.claimedAt) {
		return
	}
	v.state = m.state
	v.taskID = m.taskID
	v.err = m.err
	v.updatedAt = m.updatedAt
}

func expiredTask(vmID, taskID string, e expiry, assignedAt time.Time) TaskRecord {
	return TaskRecord{
		TaskID:     taskID,
		VMID:       vmID,
		RunStatus:  TaskRunFailed,
		RetryCount: 0,
		LastError:  e.reason,
		AssignedAt: assignedAt,
		UpdatedAt:  time.Now(),
	}
}

func expiredDeadLetter(vmID, taskID string, e expiry) DeadLetterRecord {
	return DeadLetterRecord{
		Kind:     e.kind,
		EntityID: vmID,
		TaskID:   taskID,
		Reason:   e.reason,
		Payload: dlPayload(
			"age_s", fmt.Sprintf("%d", int(e.age.Seconds())),
			"ceiling_s", fmt.Sprintf("%d", int(e.ceiling.Seconds())),
		),
	}
}

// markExpiredVMDurable moves a vm still in e.from to destroying and fails its
// task, for the durable expiry check: the vm, its task and the dead letter
// are written before it returns, and a write that fails puts the vm back and
// fails the step, so restate retries the check instead of journaling a
// teardown that never reached the store. the timer sends the destroy itself,
// as a journaled step.
func (fm *FleetManager) markExpiredVMDurable(ctx context.Context, vmID, taskID string, e expiry) (bool, error) {
	m, ok := fm.claimExpired(vmID, taskID, e)
	if !ok {
		return false, nil
	}
	if err := fm.writeExpired(ctx, vmID, taskID, e, m); err != nil {
		fm.unclaimExpired(vmID, m)
		return false, err
	}
	fm.appendEvent(ctx, "vm", vmID, "vm.destroying", map[string]any{"reason": e.reasonCode})
	fm.publishStateChange(vmID, "")
	if taskID != "" {
		fm.appendEvent(ctx, "task", taskID, "task.failed", map[string]any{
			"vm_id":  vmID,
			"error":  e.reason,
			"reason": e.reasonCode,
		})
	}
	return true, nil
}

// writeExpired writes the task and the dead letter, then the vm. both upserts
// are safe to repeat, and the vm row goes last, so a failure never leaves it
// destroying behind a vm that unclaimExpired put back.
func (fm *FleetManager) writeExpired(ctx context.Context, vmID, taskID string, e expiry, m expiredMark) error {
	if fm.store == nil {
		return nil
	}
	if taskID != "" {
		if err := fm.store.UpsertTask(ctx, expiredTask(vmID, taskID, e, m.assignedAt)); err != nil {
			return fmt.Errorf("persist expired task %s: %w", taskID, err)
		}
	}
	dl := expiredDeadLetter(vmID, taskID, e)
	dl.FirstSeenAt = time.Now()
	dl.LastSeenAt = dl.FirstSeenAt
	if err := fm.store.UpsertDeadLetter(ctx, dl); err != nil {
		return fmt.Errorf("record expiry dead letter %s: %w", vmID, err)
	}
	if err := fm.persistVMByID(ctx, vmID); err != nil {
		return fmt.Errorf("persist expired vm %s: %w", vmID, err)
	}
	return nil
}

// recordDeadLetter upserts a dead-letter entry through the state store.
// Failures are logged but not propagated — the DLQ is best-effort and a
// missing row should not block reconcile progress.
func (fm *FleetManager) recordDeadLetter(ctx context.Context, entry DeadLetterRecord) {
	if fm.store == nil {
		return
	}
	if entry.FirstSeenAt.IsZero() {
		entry.FirstSeenAt = time.Now()
	}
	if entry.LastSeenAt.IsZero() {
		entry.LastSeenAt = entry.FirstSeenAt
	}
	if err := fm.store.UpsertDeadLetter(ctx, entry); err != nil {
		fm.logger.Warn("upsert dead letter failed",
			"kind", entry.Kind,
			"entity", entry.EntityID,
			"err", err,
		)
	}
}

// dlPayload is a small helper for building the key/value JSON blobs
// attached to dead-letter entries. Pairs are passed as alternating key and
// value strings. Malformed input produces an empty object.
func dlPayload(kv ...string) json.RawMessage {
	if len(kv)%2 != 0 {
		return json.RawMessage(`{}`)
	}
	m := make(map[string]string, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
