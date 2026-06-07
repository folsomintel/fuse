package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// drainEnvFor returns the *mockEnv backing the provisioned VM so a test can
// inspect the commands Drain ran (or set up an error/hook before draining).
func drainEnvFor(t *testing.T, p *mockProvider, name string) *mockEnv {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.envs[name]
	if !ok {
		t.Fatalf("mock env %q not found", name)
	}
	return e
}

// assertDrainExec asserts the env recorded exactly `want` ExecStream calls and,
// when want==1, that the single call was the drain command.
func assertDrainExec(t *testing.T, e *mockEnv, want int) {
	t.Helper()
	calls := e.execCommands()
	if len(calls) != want {
		t.Fatalf("ExecStream called %d times, want %d (%v)", len(calls), want, calls)
	}
	if want == 1 {
		got := calls[0]
		wantCmd := []string{"sh", "-lc", DefaultFusedDrainCommand}
		if len(got) != len(wantCmd) {
			t.Fatalf("drain exec = %v, want %v", got, wantCmd)
		}
		for i := range wantCmd {
			if got[i] != wantCmd[i] {
				t.Fatalf("drain exec = %v, want %v", got, wantCmd)
			}
		}
	}
}

func TestDrain_HappyPath(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})

	ctx := context.Background()
	_, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	if err := fm.Drain(ctx, "fuse-task-1"); err != nil {
		t.Fatalf("drain: %v", err)
	}

	// The configured DrainCommand must have been Exec'd exactly once.
	assertDrainExec(t, drainEnvFor(t, p, "fuse-task-1"), 1)

	info, ok := fm.GetVM("fuse-task-1")
	if !ok {
		t.Fatal("vm missing after drain")
	}
	if info.State != VMStateDraining {
		t.Errorf("state = %q, want %q", info.State, VMStateDraining)
	}
	// Task ID should be preserved during drain — it's only cleared on
	// destroy. The harness still wants to correlate the drained env
	// with its task.
	if info.TaskID != "task-1" {
		t.Errorf("taskID = %q, want task-1", info.TaskID)
	}
}

func TestDrain_NotFound(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})

	err := fm.Drain(context.Background(), "fuse-missing")
	if !errors.Is(err, ErrVMNotFound) {
		t.Fatalf("err = %v, want ErrVMNotFound", err)
	}
}

func TestDrain_NotRunningRejected(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})

	ctx := context.Background()
	_, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// First drain → success → state=Draining.
	if err := fm.Drain(ctx, "fuse-task-1"); err != nil {
		t.Fatalf("first drain: %v", err)
	}

	// Second drain on already-Draining VM should be rejected.
	err = fm.Drain(ctx, "fuse-task-1")
	if !errors.Is(err, ErrVMNotRunning) {
		t.Fatalf("second drain err = %v, want ErrVMNotRunning", err)
	}
	// Exactly one drain command total (the second drain must not Exec).
	assertDrainExec(t, drainEnvFor(t, p, "fuse-task-1"), 1)
}

// TestDrain_DrainCommandErrorPreservesDrainingState merges the old
// Drain-command-failure + dial-failure cases: when the drain command fails, the VM
// stays Draining, the error is recorded, and a subsequent DestroyVM succeeds.
func TestDrain_DrainCommandErrorPreservesDrainingState(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})

	ctx := context.Background()
	_, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	env := drainEnvFor(t, p, "fuse-task-1")
	env.mu.Lock()
	env.execErr = fmt.Errorf("simulated drain command failure")
	env.mu.Unlock()

	err = fm.Drain(ctx, "fuse-task-1")
	if err == nil {
		t.Fatal("expected drain to surface the drain command error")
	}

	info, ok := fm.GetVM("fuse-task-1")
	if !ok {
		t.Fatal("vm missing after failed drain")
	}
	if info.State != VMStateDraining {
		t.Errorf("state = %q, want %q (failed drain must not roll back)", info.State, VMStateDraining)
	}
	if info.Error == "" {
		t.Error("expected drain error to be recorded on VM")
	}

	// And DELETE must still work — back-compat path.
	if err := fm.DestroyVM(ctx, "fuse-task-1"); err != nil {
		t.Fatalf("destroy after failed drain: %v", err)
	}
	if p.count() != 0 {
		t.Error("provider env not destroyed after DELETE")
	}
}

// TestDrain_EmptyDrainCommandSkips covers the back-compat path: an empty
// drain command means no Exec runs, the VM stays Draining, and DELETE still
// succeeds.
func TestDrain_EmptyDrainCommandSkips(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})

	ctx := context.Background()
	_, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// Force the empty-drain back-compat path.
	fm.mu.Lock()
	fm.vms["fuse-task-1"].drainCommand = ""
	fm.mu.Unlock()

	if err := fm.Drain(ctx, "fuse-task-1"); err != nil {
		t.Fatalf("drain with empty command: %v", err)
	}

	// No drain command ran.
	assertDrainExec(t, drainEnvFor(t, p, "fuse-task-1"), 0)

	info, ok := fm.GetVM("fuse-task-1")
	if !ok {
		t.Fatal("vm missing after empty drain")
	}
	if info.State != VMStateDraining {
		t.Errorf("state = %q, want %q", info.State, VMStateDraining)
	}

	// DELETE still succeeds.
	if err := fm.DestroyVM(ctx, "fuse-task-1"); err != nil {
		t.Fatalf("destroy after empty drain: %v", err)
	}
	if p.count() != 0 {
		t.Error("provider env not destroyed after DELETE")
	}
}

func TestDrain_ThenDestroyHappyPath(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})

	ctx := context.Background()
	_, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	if err := fm.Drain(ctx, "fuse-task-1"); err != nil {
		t.Fatalf("drain: %v", err)
	}
	env := drainEnvFor(t, p, "fuse-task-1")
	if err := fm.DestroyVM(ctx, "fuse-task-1"); err != nil {
		t.Fatalf("destroy after drain: %v", err)
	}

	if p.count() != 0 {
		t.Error("provider env not destroyed")
	}
	assertDrainExec(t, env, 1)
}

// TestDestroyWithoutDrain_BackCompat documents the unchanged
// single-phase teardown path: callers who never drain must still be
// able to DELETE a Running VM and have it torn down successfully.
func TestDestroyWithoutDrain_BackCompat(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})

	ctx := context.Background()
	_, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	if err := fm.DestroyVM(ctx, "fuse-task-1"); err != nil {
		t.Fatalf("destroy without drain: %v", err)
	}
	if p.count() != 0 {
		t.Error("provider env not destroyed in back-compat path")
	}
}

// TestDrain_ExecContextHasTimeout verifies Drain gives the drain command a
// bounded deadline derived from drainTimeout.
func TestDrain_ExecContextHasTimeout(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})

	ctx := context.Background()
	_, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	var sawDeadline bool
	env := drainEnvFor(t, p, "fuse-task-1")
	env.mu.Lock()
	env.execHook = func(execCtx context.Context) error {
		if d, ok := execCtx.Deadline(); ok && time.Until(d) > 0 && time.Until(d) <= drainTimeout {
			sawDeadline = true
		}
		return nil
	}
	env.mu.Unlock()

	if err := fm.Drain(ctx, "fuse-task-1"); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if !sawDeadline {
		t.Error("expected drain command to see a bounded deadline derived from drainTimeout")
	}
}
