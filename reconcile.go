package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// reconcileOrphans destroys provider VMs that are not tracked in the fleet.
// Repeated destroy failures are counted per-VM in fm.orphanRetries; once
// the count exceeds orphanDestroyMaxRetries the entry is dead-lettered and
// skipped on subsequent cycles until it disappears from the provider.
func (fm *FleetManager) reconcileOrphans(ctx context.Context, envs []Environment, tracked map[string]bool, summary *ReconcileSummary) {
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

		fm.logger.Warn("orphan vm detected, destroying", "vm", name, "attempt", retries+1)
		if err := fm.provider.Destroy(ctx, name); err != nil {
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

		// Success — clear retry counter.
		fm.mu.Lock()
		delete(fm.orphanRetries, name)
		fm.mu.Unlock()
		summary.OrphansDestroyed++
	}

	// Clear retry counters for orphans that no longer exist in the provider
	// (either because they were destroyed or disappeared on their own).
	fm.mu.Lock()
	for name := range fm.orphanRetries {
		if !currentOrphans[name] {
			delete(fm.orphanRetries, name)
		}
	}
	fm.mu.Unlock()
}

// reconcileStuckTasks flags and tears down VMs in the Running state whose
// age exceeds the configured runtime ceiling. This is a leak detector, not
// a liveness check — see the docstring on FleetConfig.TaskStuckTimeout.
//
// Detection is two-strike: a VM must exceed its timeout on two consecutive
// reconcile cycles before it is failed. Between strikes it is annotated
// with a task.stuck_suspected event.
func (fm *FleetManager) reconcileStuckTasks(ctx context.Context, summary *ReconcileSummary) {
	now := time.Now()

	type stuckCandidate struct {
		vmID     string
		taskID   string
		age      time.Duration
		ceiling  time.Duration
		strikes  int
		needFail bool
	}

	fm.mu.Lock()
	candidates := make([]stuckCandidate, 0)
	live := make(map[string]bool, len(fm.vms))
	for id, v := range fm.vms {
		live[id] = true

		if v.state != VMStateRunning || v.taskID == "" {
			// Reset any strike history — the VM either transitioned or no
			// longer carries a task, so past staleness is no longer meaningful.
			delete(fm.stuckStrikes, id)
			continue
		}

		ceiling := v.spec.MaxRuntime
		if ceiling <= 0 {
			ceiling = fm.taskStuckTimeout
		}
		age := now.Sub(v.createdAt)
		if age <= ceiling {
			delete(fm.stuckStrikes, id)
			continue
		}

		fm.stuckStrikes[id]++
		strikes := fm.stuckStrikes[id]
		candidates = append(candidates, stuckCandidate{
			vmID:     id,
			taskID:   v.taskID,
			age:      age,
			ceiling:  ceiling,
			strikes:  strikes,
			needFail: strikes >= 2,
		})
	}
	// Drop strike entries for VMs that no longer exist.
	for id := range fm.stuckStrikes {
		if !live[id] {
			delete(fm.stuckStrikes, id)
		}
	}
	fm.mu.Unlock()

	for _, c := range candidates {
		if !c.needFail {
			summary.StuckTasksSuspected++
			fm.logger.Warn("vm exceeded runtime ceiling, suspected stuck",
				"vm", c.vmID,
				"task", c.taskID,
				"age", c.age,
				"ceiling", c.ceiling,
				"strike", c.strikes,
			)
			fm.appendEventBackground("task", c.taskID, "task.stuck_suspected", map[string]any{
				"vm_id":   c.vmID,
				"age_s":   int(c.age.Seconds()),
				"ceiling": c.ceiling.String(),
				"strike":  c.strikes,
			})
			continue
		}

		// Second strike — fail the task and mark the VM destroying.
		fm.failStuckTask(ctx, c.vmID, c.taskID, c.age, c.ceiling)
		summary.StuckTasksFailed++
	}
}

// failStuckTask marks a Running VM as destroying and fails its task with
// a "stuck" reason. Shares structure with the "vm missing from provider"
// path in reconcile() but is triggered by the stuck-task detector.
func (fm *FleetManager) failStuckTask(ctx context.Context, vmID, taskID string, age, ceiling time.Duration) {
	reason := fmt.Sprintf("task stuck: runtime %s exceeded ceiling %s with no state transitions", age.Round(time.Second), ceiling)

	fm.mu.Lock()
	v, ok := fm.vms[vmID]
	if !ok || v.state != VMStateRunning || v.taskID != taskID {
		fm.mu.Unlock()
		return
	}
	v.state = VMStateDestroying
	v.taskID = ""
	v.err = reason
	v.updatedAt = time.Now()
	assignedAt := v.createdAt
	delete(fm.stuckStrikes, vmID)
	fm.mu.Unlock()

	fm.logger.Error("failing stuck task",
		"vm", vmID,
		"task", taskID,
		"age", age,
		"ceiling", ceiling,
	)

	fm.persistVMBackground(vmID)
	fm.appendEventBackground("vm", vmID, "vm.destroying", map[string]any{"reason": "stuck_task"})
	fm.publishStateChange(vmID, "")
	fm.upsertTaskBackground(TaskRecord{
		TaskID:     taskID,
		VMID:       vmID,
		RunStatus:  TaskRunFailed,
		RetryCount: 0,
		LastError:  reason,
		AssignedAt: assignedAt,
		UpdatedAt:  time.Now(),
	})
	fm.appendEventBackground("task", taskID, "task.failed", map[string]any{
		"vm_id":  vmID,
		"error":  reason,
		"reason": "stuck_task",
	})
	fm.recordDeadLetter(ctx, DeadLetterRecord{
		Kind:     DeadLetterStuckTask,
		EntityID: vmID,
		TaskID:   taskID,
		Reason:   reason,
		Payload: dlPayload(
			"age_s", fmt.Sprintf("%d", int(age.Seconds())),
			"ceiling_s", fmt.Sprintf("%d", int(ceiling.Seconds())),
		),
	})
	go fm.destroyAndRemove(vmID)
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
