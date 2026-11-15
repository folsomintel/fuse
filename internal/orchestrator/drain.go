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

// DefaultDrainingTimeout is how long a VM may stay Draining before the
// reconcile loop destroys it, when FleetConfig.DrainingTimeout is unset. It
// is the drain command's ceiling plus a margin for the caller to read the
// result and send DELETE.
const DefaultDrainingTimeout = drainTimeout + 5*time.Minute

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
//     can still issue DELETE through the existing path. Destroying it
//     here would defeat the whole point of giving the harness a chance
//     to recover from a partial drain. A VM nobody DELETEs is destroyed
//     once the draining timeout (the grace) passes: by reconcile in mode
//     off, by the Environment/{vm_id}.Expire timer the durable drain arms
//     in modes self and cloud.
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
	if d := fm.durableExpiry(); d != nil {
		// refuse what mode off refuses before restate sees it: the durable
		// begin step accepts a vm already draining, so a replay after a
		// crash finds its own transition done.
		if err := fm.drainable(vmID); err != nil {
			return err
		}
		ctx, cancel := durableWait(ctx)
		defer cancel()
		return d.Drain(ctx, vmID)
	}
	return fm.drainInProcess(ctx, vmID)
}

// drainInProcess is Drain run in this process. a durable migrate drains its
// source with it: the migrate holds the source's Environment object, so a
// drain sent to that object would wait on the migrate forever.
func (fm *FleetManager) drainInProcess(ctx context.Context, vmID string) error {
	plan, err := fm.drainBegin(ctx, vmID, false)
	if err != nil || !plan.Command {
		return err
	}
	if msg := fm.DrainCommand(ctx, vmID); msg != "" {
		return fmt.Errorf("drain vm %s: %s", vmID, msg)
	}
	return nil
}

// DrainPlan is what DrainBegin settled. restate journals it.
type DrainPlan struct {
	// Command is set when there is a drain command to run in the guest.
	Command bool `json:"command,omitempty"`
	// Grace is how long the vm may stay draining before it is destroyed.
	Grace time.Duration `json:"grace"`
}

// drainable reports why vmID cannot be drained, or nil.
func (fm *FleetManager) drainable(vmID string) error {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	v, ok := fm.vms[vmID]
	if !ok {
		return fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	if v.state != VMStateRunning {
		return fmt.Errorf("%w: vm %s is in state %s", ErrVMNotRunning, vmID, v.state)
	}
	return nil
}

// the two methods below are the steps of a drain, in order. the durable
// Environment/{vm_id}.Drain handler runs each as a journaled step and arms
// the grace timer between them; mode off runs them in turn.

// DrainCheck refuses a vm that is not running. the durable drain journals it
// before DrainBegin, so a second drain queued behind the first on the object
// is refused here, while a replay of the first after a crash, whose check is
// already journaled, goes on to DrainBegin.
func (fm *FleetManager) DrainCheck(_ context.Context, vmID string) error {
	return fm.drainable(vmID)
}

// DrainBegin moves a running vm to draining and persists it. a vm already
// draining is a replay of this step after a crash and is not an error.
func (fm *FleetManager) DrainBegin(ctx context.Context, vmID string) (DrainPlan, error) {
	return fm.drainBegin(ctx, vmID, true)
}

func (fm *FleetManager) drainBegin(ctx context.Context, vmID string, replay bool) (DrainPlan, error) {
	fm.mu.Lock()
	v, ok := fm.vms[vmID]
	if !ok {
		fm.mu.Unlock()
		return DrainPlan{}, fmt.Errorf("%w: %s", ErrVMNotFound, vmID)
	}
	plan := DrainPlan{Command: v.env != nil && v.drainCommand != "", Grace: fm.drainingTimeout}
	if replay && v.state == VMStateDraining {
		// after a restart the vm has no env handle yet: DrainCommand
		// attaches one, so the command still runs.
		plan.Command = v.drainCommand != ""
		fm.mu.Unlock()
		return plan, nil
	}
	if v.state != VMStateRunning {
		current := v.state
		fm.mu.Unlock()
		return DrainPlan{}, fmt.Errorf("%w: vm %s is in state %s", ErrVMNotRunning, vmID, current)
	}

	// Snapshot what we need before releasing the lock so the drain command
	// runs outside the critical section.
	hasEnv := v.env != nil
	prevState := v.state
	prevUpdatedAt := v.updatedAt
	v.state = VMStateDraining
	v.updatedAt = time.Now()
	fm.mu.Unlock()

	if err := fm.persistVMByID(ctx, vmID); err != nil {
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
		return DrainPlan{}, fmt.Errorf("persist vm %s draining state: %w", vmID, err)
	}
	fm.appendEvent(ctx, "vm", vmID, "vm.draining", map[string]any{"reason": "drain_requested"})
	fm.publishStateChange(vmID, "")
	fm.logger.Info("draining vm", "vm", vmID)

	if !hasEnv {
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
		return plan, nil
	}

	if !plan.Command {
		// No drain command configured for this agent: leave the VM Draining
		// for the caller to DELETE (back-compat). No graceful command runs.
		fm.appendEventBackground("vm", vmID, "vm.drain_skipped", map[string]any{"reason": "no_drain_command"})
	}
	return plan, nil
}

// DrainCommand runs the vm's drain command in the guest and returns why it
// failed, or "" when it succeeded. a failure is a result, not an error: the
// vm stays draining and the command is not retried, since running a partial
// quiesce again is the caller's call.
func (fm *FleetManager) DrainCommand(ctx context.Context, vmID string) string {
	fm.mu.RLock()
	v, ok := fm.vms[vmID]
	var env Environment
	drainCmd := ""
	var attach *vm
	if ok {
		env = v.env
		drainCmd = v.drainCommand
		attach = &vm{id: v.id, hostID: v.hostID, authTokenEncrypted: v.authTokenEncrypted}
	}
	fm.mu.RUnlock()
	if !ok || drainCmd == "" {
		return ""
	}
	if env == nil {
		// recovery attaches only running vms, so a drain resumed after a
		// restart attaches its own handle.
		if err := fm.reattachVMFromProvider(ctx, attach); err != nil {
			msg := fmt.Sprintf("drain command: attach environment: %v", err)
			fm.recordDrainError(vmID, msg)
			return msg
		}
		env = attach.env
		fm.mu.Lock()
		if v, ok := fm.vms[vmID]; ok && v.env == nil {
			v.env = env
			v.url = attach.url
		}
		fm.mu.Unlock()
	}

	// Bound the drain command even if the caller's context has none.
	rpcCtx, cancel := context.WithTimeout(ctx, drainTimeout)
	defer cancel()

	if err := env.ExecStream(rpcCtx, io.Discard, io.Discard, "sh", "-lc", drainCmd); err != nil {
		msg := fmt.Sprintf("drain command: %v", err)
		fm.recordDrainError(vmID, msg)
		fm.appendEventBackground("vm", vmID, "vm.drain_failed", map[string]any{"reason": "exec", "error": err.Error()})
		fm.logger.Warn("drain command failed", "vm", vmID, "err", err)
		// Stay in Draining: caller decides whether to retry or DELETE.
		return msg
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
	return ""
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
