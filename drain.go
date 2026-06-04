package orchestrator

import (
	"context"
	"fmt"
	"io"
	"time"
)

// drainTimeout is the default ceiling on a single drain command run inside
// the guest. The configured DrainCommand is bounded so a wedged guest cannot
// hang the orchestrator's request thread indefinitely. Callers that want a
// shorter ceiling can pass a context with their own deadline.
const drainTimeout = 30 * time.Second

// Drain transitions a Running VM into the Draining state and runs the
// configured drain command inside the guest (via Environment.ExecStream) to
// quiesce in-guest workloads gracefully. It is the first phase of two-phase
// environment teardown:
//
//  1. POST /v1/environments/{vmId}?action=drain → Drain → guest quiesce
//  2. DELETE /v1/environments/{vmId}            → DestroyVM → VM gone
//
// Drain is intentionally narrow:
//
//   - Only Running VMs can be drained. Any other state (Provisioning,
//     Draining, Destroying) returns ErrVMNotRunning so callers can map
//     it to a 409 Conflict and inspect the current state.
//   - Drain does not destroy the VM on success or on failure. Even if
//     the drain command fails, the VM stays in Draining so the caller
//     can still issue DELETE through the existing path. Auto-destroy
//     here would defeat the whole point of giving the harness a chance
//     to recover from a partial drain.
//   - An empty drain command leaves the VM Draining for the caller to
//     DELETE (back-compat): no graceful command is run.
//   - Drain takes its own context; if the caller's request context is
//     cancelled we abort the command cleanly. The default drainTimeout
//     is layered on top so a slow guest cannot hold a request thread
//     indefinitely.
//
// The state transition (Running → Draining) is performed and persisted
// before the drain command fires. That ordering matters: if the orchestrator
// crashes mid-drain, recovery will see a Draining VM and the operator
// can decide whether to retry Drain or proceed straight to DELETE. The
// alternative (command first, then state flip) would lose the intent of
// the drain on a crash.
func (fm *FleetManager) Drain(ctx context.Context, vmID string) error {
	fm.mu.Lock()
	v, ok := fm.vms[vmID]
	if !ok {
		fm.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	if v.state != VMStateRunning {
		current := v.state
		fm.mu.Unlock()
		return fmt.Errorf("%w: vm %s is in state %s", ErrVMNotRunning, vmID, current)
	}

	// Snapshot what we need before releasing the lock so the drain command
	// runs outside the critical section.
	env := v.env
	drainCmd := v.drainCommand
	prevState := v.state
	prevUpdatedAt := v.updatedAt
	v.state = VMStateDraining
	v.updatedAt = time.Now()
	fm.mu.Unlock()

	rollback := func() {
		fm.mu.Lock()
		current, exists := fm.vms[vmID]
		if exists {
			current.state = prevState
			current.updatedAt = prevUpdatedAt
		}
		fm.mu.Unlock()
		if exists {
			if err := fm.persistVMByID(context.Background(), vmID); err != nil {
				fm.logger.Warn("persist vm rollback after drain failure", "vm", vmID, "err", err)
			}
		}
	}

	if err := fm.persistVMByID(ctx, vmID); err != nil {
		rollback()
		return fmt.Errorf("persist vm %s draining state: %w", vmID, err)
	}
	fm.appendEvent(ctx, "vm", vmID, "vm.draining", map[string]any{"reason": "drain_requested"})
	fm.publishStateChange(vmID, "")
	fm.logger.Info("draining vm", "vm", vmID)

	if env == nil {
		// No live env handle — nothing to run the drain command against,
		// but the state transition still succeeded. This can happen for a
		// VM recovered from state but never re-attached to a provider
		// (extremely rare in practice). Record the condition so operators
		// see why no command was attempted, but treat it as a successful
		// drain so the caller can still DELETE.
		fm.mu.Lock()
		v, ok := fm.vms[vmID]
		if ok {
			v.err = "drain: no environment handle, drain command not invoked"
			v.updatedAt = time.Now()
		}
		fm.mu.Unlock()
		fm.persistVMBackground(vmID)
		fm.appendEventBackground("vm", vmID, "vm.drain_skipped", map[string]any{"reason": "no_environment"})
		return nil
	}

	if drainCmd == "" {
		// No drain command configured for this agent: leave the VM Draining
		// for the caller to DELETE (back-compat). No graceful command runs.
		fm.appendEventBackground("vm", vmID, "vm.drain_skipped", map[string]any{"reason": "no_drain_command"})
		return nil
	}

	// Bound the drain command even if the caller's context has none.
	rpcCtx, cancel := context.WithTimeout(ctx, drainTimeout)
	defer cancel()

	if err := env.ExecStream(rpcCtx, io.Discard, io.Discard, "sh", "-lc", drainCmd); err != nil {
		fm.recordDrainError(vmID, fmt.Sprintf("drain command: %v", err))
		fm.appendEventBackground("vm", vmID, "vm.drain_failed", map[string]any{"reason": "exec", "error": err.Error()})
		fm.logger.Warn("drain command failed", "vm", vmID, "err", err)
		// Stay in Draining: caller decides whether to retry or DELETE.
		return fmt.Errorf("drain vm %s: drain command: %w", vmID, err)
	}

	fm.mu.Lock()
	if v, ok := fm.vms[vmID]; ok {
		v.err = ""
		v.updatedAt = time.Now()
	}
	fm.mu.Unlock()
	fm.persistVMBackground(vmID)
	fm.appendEventBackground("vm", vmID, "vm.drained", nil)
	fm.logger.Info("vm drained", "vm", vmID)
	return nil
}

// recordDrainError stamps an error message onto the VM record without
// changing its state. The VM stays in Draining: the contract says a
// failed drain command does not auto-destroy and does not roll back to
// Running (the harness has likely already received the graceful-stop
// signal from the partial drain).
func (fm *FleetManager) recordDrainError(vmID, msg string) {
	fm.mu.Lock()
	v, ok := fm.vms[vmID]
	if ok {
		v.err = msg
		v.updatedAt = time.Now()
	}
	fm.mu.Unlock()
	if ok {
		fm.persistVMBackground(vmID)
	}
}
