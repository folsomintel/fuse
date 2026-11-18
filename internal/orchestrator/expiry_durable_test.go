package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
)

// inlineExpiry stands in for restate: Drain runs the drain steps in turn, the
// way the durable handler does, and arms are recorded. failArms makes that
// many arm sends fail first.
type inlineExpiry struct {
	fm *FleetManager

	mu       sync.Mutex
	drains   []string
	arms     []string
	failArms int
}

func (d *inlineExpiry) Drain(ctx context.Context, vmID string) error {
	d.mu.Lock()
	d.drains = append(d.drains, vmID)
	d.mu.Unlock()
	plan, err := d.fm.DrainBegin(ctx, vmID)
	if err != nil || !plan.Command {
		return err
	}
	if msg := d.fm.DrainCommand(ctx, vmID); msg != "" {
		return errors.New(msg)
	}
	return nil
}

func (d *inlineExpiry) SendArmExpiry(_ context.Context, vmID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failArms > 0 {
		d.failArms--
		return errors.New("restate down")
	}
	d.arms = append(d.arms, vmID)
	return nil
}

func (d *inlineExpiry) armed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.arms...)
}

func withDurableExpiry(fm *FleetManager) *inlineExpiry {
	d := &inlineExpiry{fm: fm}
	fm.SetDurableExpiry(d)
	return d
}

func vmState(fm *FleetManager, vmID string) VMState {
	fm.mu.RLock()
	defer fm.mu.RUnlock()
	if v, ok := fm.vms[vmID]; ok {
		return v.state
	}
	return ""
}

// TestDurableCreateArmsExpiry: a create that reaches running arms its expiry
// timer, once.
func TestDurableCreateArmsExpiry(t *testing.T) {
	fm := newInlineFleet(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-"})
	d := withDurableExpiry(fm)
	if _, err := fm.ProvisionAndAssign(context.Background(), "task-1", Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	fm.expirySends.Wait()
	fm.armExpiries()
	fm.expirySends.Wait()
	if got := d.armed(); len(got) != 1 || got[0] != "fuse-task-1" {
		t.Fatalf("arms %v, want one for fuse-task-1", got)
	}
}

// TestDurableDrain: Drain goes through the durable handler,
// which runs the command once and leaves the vm draining.
func TestDurableDrain(t *testing.T) {
	p := newMockProvider()
	fm := newInlineFleet(FleetConfig{Provider: p, Prefix: "fuse-"})
	d := withDurableExpiry(fm)
	ctx := context.Background()
	if _, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := fm.Drain(ctx, "fuse-task-1"); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(d.drains) != 1 {
		t.Fatalf("durable drains %v, want 1", d.drains)
	}
	assertDrainExec(t, drainEnvFor(t, p, "fuse-task-1"), 1)
	if s := vmState(fm, "fuse-task-1"); s != VMStateDraining {
		t.Fatalf("state %s, want draining", s)
	}

	// a second drain is refused before restate sees it.
	if err := fm.Drain(ctx, "fuse-task-1"); !errors.Is(err, ErrVMNotRunning) {
		t.Fatalf("second drain: %v, want ErrVMNotRunning", err)
	}
	if err := fm.Drain(ctx, "fuse-missing"); !errors.Is(err, ErrVMNotFound) {
		t.Fatalf("drain of a missing vm: %v, want ErrVMNotFound", err)
	}
	if len(d.drains) != 1 {
		t.Fatalf("refused drains reached restate: %v", d.drains)
	}
}

// TestDrainBeginReplay: the durable begin step run again after a crash finds
// the vm draining and succeeds without a second transition; a vm in any
// other state is still refused.
func TestDrainBeginReplay(t *testing.T) {
	fm, vmID := fleetWithRunningVM(t, FleetConfig{DrainingTimeout: 10 * time.Minute}, "task-1", time.Minute)
	ctx := context.Background()
	plan, err := fm.DrainBegin(ctx, vmID)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if plan.Grace != 10*time.Minute {
		t.Fatalf("grace %s, want the draining timeout", plan.Grace)
	}
	fm.mu.RLock()
	first := fm.vms[vmID].updatedAt
	fm.mu.RUnlock()
	if _, err := fm.DrainBegin(ctx, vmID); err != nil {
		t.Fatalf("replayed begin: %v", err)
	}
	fm.mu.Lock()
	again := fm.vms[vmID].updatedAt
	fm.vms[vmID].state = VMStateDestroying
	fm.mu.Unlock()
	if !again.Equal(first) {
		t.Fatal("replayed begin restarted the draining clock")
	}
	if _, err := fm.DrainBegin(ctx, vmID); !errors.Is(err, ErrVMNotRunning) {
		t.Fatalf("begin on a destroying vm: %v, want ErrVMNotRunning", err)
	}
}

// TestDrainCommandFailureIsAResult: a failing drain command is reported, not
// returned as an error a retry would run again, and the vm stays draining.
func TestDrainCommandFailureIsAResult(t *testing.T) {
	p := newMockProvider()
	fm := newInlineFleet(FleetConfig{Provider: p, Prefix: "fuse-"})
	ctx := context.Background()
	if _, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatal(err)
	}
	env := drainEnvFor(t, p, "fuse-task-1")
	env.mu.Lock()
	env.execErr = errors.New("guest refused")
	env.mu.Unlock()
	if _, err := fm.DrainBegin(ctx, "fuse-task-1"); err != nil {
		t.Fatal(err)
	}
	if msg := fm.DrainCommand(ctx, "fuse-task-1"); msg == "" {
		t.Fatal("failed drain command reported success")
	}
	if s := vmState(fm, "fuse-task-1"); s != VMStateDraining {
		t.Fatalf("state %s, want draining", s)
	}
}

// TestExpiryCheckIdle: past its idle timeout a vm is struck once and reaped
// on the next check, which marks it destroying and leaves the destroy to the
// timer.
func TestExpiryCheckIdle(t *testing.T) {
	p := newMockProvider()
	fm, vmID := idleFleet(t, FleetConfig{Provider: p, DefaultIdleTimeout: 5 * time.Minute, StateStore: NewMemoryStateStore()}, "task-1", 30*time.Minute)
	withDurableExpiry(fm)
	ctx := context.Background()

	v, err := fm.ExpiryCheck(ctx, vmID, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Strike != "idle_timeout" || v.Reap || v.In != fm.reconcileInterval {
		t.Fatalf("first check %+v, want a strike rechecked after one reconcile interval", v)
	}
	if s := vmState(fm, vmID); s != VMStateRunning {
		t.Fatalf("state after one strike %s, want running", s)
	}

	v, err = fm.ExpiryCheck(ctx, vmID, "idle_timeout")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Reap {
		t.Fatalf("second check %+v, want a reap", v)
	}
	if s := vmState(fm, vmID); s != VMStateDestroying {
		t.Fatalf("state after the reap %s, want destroying", s)
	}
	if p.count() != 1 {
		t.Fatal("the check destroyed the vm itself; the timer sends the destroy")
	}

	// a replay of the reaping check after a crash sends the destroy again.
	if v, _ := fm.ExpiryCheck(ctx, vmID, "idle_timeout"); !v.Reap {
		t.Fatalf("replayed check %+v, want a reap", v)
	}
}

// TestExpiryCheckPendingInterventionHoldsIdle: an env a human is deciding
// about is never idle-reaped; the timer looks again later.
func TestExpiryCheckPendingInterventionHoldsIdle(t *testing.T) {
	store := NewMemoryStateStore()
	fm, vmID := idleFleet(t, FleetConfig{DefaultIdleTimeout: 5 * time.Minute, StateStore: store}, "task-1", 30*time.Minute)
	if err := store.UpsertIntervention(context.Background(), InterventionRecord{
		ID: "int_1", EnvID: vmID, Kind: string(hitl.KindExec), State: string(hitl.StatePending),
		Deadline: time.Now().Add(time.Hour), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	for _, strike := range []string{"", "idle_timeout"} {
		v, err := fm.ExpiryCheck(context.Background(), vmID, strike)
		if err != nil {
			t.Fatal(err)
		}
		if v.Reap || v.Strike != "" || v.In != expiryRecheck {
			t.Fatalf("check with strike %q %+v, want a recheck", strike, v)
		}
	}
	if s := vmState(fm, vmID); s != VMStateRunning {
		t.Fatalf("state %s, want running", s)
	}
}

// TestExpiryCheckSchedules: a vm not due yet is looked at again when its
// window ends, an attached one a full timeout from now, an extended one when
// the extension ends, and one nothing watches not at all.
func TestExpiryCheckSchedules(t *testing.T) {
	ctx := context.Background()
	near := func(got, want time.Duration) bool { return got > want-5*time.Second && got <= want }

	fm, vmID := idleFleet(t, FleetConfig{DefaultIdleTimeout: 5 * time.Minute}, "task-1", time.Minute)
	if v, _ := fm.ExpiryCheck(ctx, vmID, ""); v.Strike != "" || v.Reap || !near(v.In, 4*time.Minute) {
		t.Fatalf("idle for 1m of 5m: %+v, want a check in 4m", v)
	}

	fm.mu.Lock()
	fm.vms[vmID].attachSessions = 1
	fm.vms[vmID].lastActivityAt = time.Now().Add(-time.Hour)
	fm.mu.Unlock()
	if v, _ := fm.ExpiryCheck(ctx, vmID, ""); v.Strike != "" || !near(v.In, 5*time.Minute) {
		t.Fatalf("attached: %+v, want a check in one timeout", v)
	}

	fm.mu.Lock()
	fm.vms[vmID].attachSessions = 0
	fm.mu.Unlock()
	fm.hitlMu.Lock()
	fm.signalStateLocked(vmID).extendedUntil = time.Now().Add(20 * time.Minute)
	fm.hitlMu.Unlock()
	if v, _ := fm.ExpiryCheck(ctx, vmID, "idle_timeout"); v.Reap || v.Strike != "" || !near(v.In, 20*time.Minute) {
		t.Fatalf("extended: %+v, want a check when the extension ends", v)
	}

	// no task means no runtime ceiling, and no idle timeout: nothing to watch.
	plain, plainID := fleetWithRunningVM(t, FleetConfig{}, "", time.Hour)
	if v, _ := plain.ExpiryCheck(ctx, plainID, ""); v != (ExpiryVerdict{}) {
		t.Fatalf("unwatched vm: %+v, want nothing", v)
	}
	if v, _ := plain.ExpiryCheck(ctx, "fuse-gone", ""); v != (ExpiryVerdict{}) {
		t.Fatalf("missing vm: %+v, want nothing", v)
	}
}

// TestExpiryCheckRuntimeCeiling: a task past max_runtime is struck, then
// reaped; a pending expiry intervention holds it.
func TestExpiryCheckRuntimeCeiling(t *testing.T) {
	store := NewMemoryStateStore()
	fm, vmID := fleetWithRunningVM(t, FleetConfig{TaskStuckTimeout: time.Hour, StateStore: store}, "task-1", 2*time.Hour)
	ctx := context.Background()
	if err := store.UpsertIntervention(ctx, InterventionRecord{
		ID: "int_exp", EnvID: vmID, Kind: string(hitl.KindExpiry), State: string(hitl.StatePending),
		Deadline: time.Now().Add(time.Hour), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if v, _ := fm.ExpiryCheck(ctx, vmID, "stuck_task"); v.Reap || v.In != expiryRecheck {
		t.Fatalf("pending expiry: %+v, want a recheck", v)
	}
	if err := store.UpsertIntervention(ctx, InterventionRecord{
		ID: "int_exp", EnvID: vmID, Kind: string(hitl.KindExpiry), State: string(hitl.StateRejected),
		Deadline: time.Now().Add(time.Hour), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if v, _ := fm.ExpiryCheck(ctx, vmID, ""); v.Strike != "stuck_task" {
		t.Fatalf("past the ceiling: %+v, want a stuck_task strike", v)
	}
	// a strike for another window does not count toward this one.
	if v, _ := fm.ExpiryCheck(ctx, vmID, "idle_timeout"); v.Reap || v.Strike != "stuck_task" {
		t.Fatalf("past the ceiling after an idle strike: %+v, want a fresh stuck_task strike", v)
	}
	if v, _ := fm.ExpiryCheck(ctx, vmID, "stuck_task"); !v.Reap {
		t.Fatalf("second strike: %+v, want a reap", v)
	}
}

// TestExpiryCheckDraining: a draining vm is reaped once the grace is up,
// rescheduled for the rest before then, and left alone when its hitl policy
// gates destroy.
func TestExpiryCheckDraining(t *testing.T) {
	ctx := context.Background()
	fm, vmID := drainingFleet(t, FleetConfig{DrainingTimeout: 10 * time.Minute}, "task-1", 4*time.Minute)
	if v, _ := fm.ExpiryCheck(ctx, vmID, ""); v.Reap || v.In <= 5*time.Minute || v.In > 6*time.Minute {
		t.Fatalf("4m into a 10m grace: %+v, want a check in about 6m", v)
	}
	fm.mu.Lock()
	fm.vms[vmID].updatedAt = time.Now().Add(-11 * time.Minute)
	fm.mu.Unlock()
	if v, _ := fm.ExpiryCheck(ctx, vmID, ""); !v.Reap {
		t.Fatalf("past the grace: %+v, want a reap with no strike first", v)
	}
	if s := vmState(fm, vmID); s != VMStateDestroying {
		t.Fatalf("state %s, want destroying", s)
	}

	gated, gatedID := drainingFleet(t, FleetConfig{DrainingTimeout: 10 * time.Minute}, "task-2", time.Hour)
	gated.mu.Lock()
	gated.vms[gatedID].spec.Hitl = &hitl.Spec{Gates: []hitl.Gate{{
		Name: "destroy", On: hitl.KindLifecycle,
		Match: hitl.GateMatch{Action: []string{string(hitl.ActionDestroy)}}, Mode: hitl.ModeAsk,
	}}}
	gated.mu.Unlock()
	if v, _ := gated.ExpiryCheck(ctx, gatedID, ""); v != (ExpiryVerdict{}) {
		t.Fatalf("destroy gated: %+v, want nothing", v)
	}
}

// TestDurableReconcileArmsInsteadOfReaping: reconcile reaps nothing itself;
// it arms each vm once, and again after a failed send.
func TestDurableReconcileArmsInsteadOfReaping(t *testing.T) {
	fm, vmID := idleFleet(t, FleetConfig{DefaultIdleTimeout: 5 * time.Minute}, "task-1", 30*time.Minute)
	d := withDurableExpiry(fm)
	d.failArms = 1
	for range 3 {
		fm.reconcile(context.Background())
		fm.expirySends.Wait()
	}
	if s := vmState(fm, vmID); s != VMStateRunning {
		t.Fatalf("state %s, want running: reconcile reaped the vm itself", s)
	}
	if got := d.armed(); len(got) != 1 || got[0] != vmID {
		t.Fatalf("arms %v, want one after the failed one", got)
	}
}

// TestExtensionRearmsExpiry: an approved extension re-arms the env's timer
// even though it was armed before.
func TestExtensionRearmsExpiry(t *testing.T) {
	fm, vmID := idleFleet(t, FleetConfig{DefaultIdleTimeout: 5 * time.Minute}, "task-1", time.Minute)
	d := withDurableExpiry(fm)
	fm.armExpiry(vmID)
	fm.expirySends.Wait()
	until := time.Now().Add(time.Hour)
	fm.applyExtension(hitl.Intervention{EnvID: vmID, Kind: hitl.KindExpiry, Signal: &hitl.SignalPayload{ExtendedUntil: &until}})
	fm.expirySends.Wait()
	if got := d.armed(); len(got) != 2 {
		t.Fatalf("arms %v, want the first and the extension's", got)
	}
}

// TestReusedIDIsArmedAgain: a vm that reuses the id of one armed before is
// armed afresh by the sweep.
func TestReusedIDIsArmedAgain(t *testing.T) {
	fm, vmID := idleFleet(t, FleetConfig{DefaultIdleTimeout: 5 * time.Minute}, "task-1", time.Minute)
	d := withDurableExpiry(fm)
	fm.armExpiry(vmID)
	fm.expirySends.Wait()
	fm.mu.Lock()
	fm.vms[vmID].createdAt = time.Now()
	fm.mu.Unlock()
	fm.armExpiries()
	fm.expirySends.Wait()
	if got := d.armed(); len(got) != 2 {
		t.Fatalf("arms %v, want the reused id armed again", got)
	}
}

// TestDrainCommandAttachesAfterRestart: a drain resumed after a restart finds
// the recovered draining vm with no env handle, attaches one and runs the
// command instead of skipping it.
func TestDrainCommandAttachesAfterRestart(t *testing.T) {
	p := newMockProvider()
	fm, vmID := drainingFleet(t, FleetConfig{Provider: p}, "task-1", time.Minute)
	fm.mu.Lock()
	fm.vms[vmID].env = nil
	fm.vms[vmID].drainCommand = DefaultFusedDrainCommand
	fm.mu.Unlock()
	plan, err := fm.DrainBegin(context.Background(), vmID)
	if err != nil || !plan.Command {
		t.Fatalf("replayed begin %+v, %v: want the command still to run", plan, err)
	}
	if msg := fm.DrainCommand(context.Background(), vmID); msg != "" {
		t.Fatalf("drain command: %s", msg)
	}
	assertDrainExec(t, drainEnvFor(t, p, vmID), 1)
}

// TestExpiryCheckSpecWindows: a spec's idle timeout and max runtime override
// the fleet defaults either way, activity since a strike clears it, and a vm
// whose destroy failed is not watched. these are the rules
// the reconcile detectors applied before expiry moved to the durable timer.
func TestExpiryCheckSpecWindows(t *testing.T) {
	ctx := context.Background()

	// the fleet default says idle, the spec allows two hours.
	fm, vmID := idleFleet(t, FleetConfig{DefaultIdleTimeout: 5 * time.Minute}, "task-1", 30*time.Minute)
	fm.mu.Lock()
	fm.vms[vmID].spec.IdleTimeout = 2 * time.Hour
	fm.mu.Unlock()
	if v, _ := fm.ExpiryCheck(ctx, vmID, ""); v.Strike != "" || v.Reap {
		t.Fatalf("spec idle timeout over the default: %+v, want no strike", v)
	}

	// no fleet default, the spec sets one.
	fm, vmID = idleFleet(t, FleetConfig{}, "task-1", 30*time.Minute)
	fm.mu.Lock()
	fm.vms[vmID].spec.IdleTimeout = 5 * time.Minute
	fm.mu.Unlock()
	if v, _ := fm.ExpiryCheck(ctx, vmID, ""); v.Strike != "idle_timeout" {
		t.Fatalf("spec idle timeout with no default: %+v, want an idle strike", v)
	}

	// activity after the strike restarts the window: the second check does
	// not reap.
	fm.touchActivity(vmID)
	if v, _ := fm.ExpiryCheck(ctx, vmID, "idle_timeout"); v.Reap || v.Strike != "" {
		t.Fatalf("active again after a strike: %+v, want no reap and no strike", v)
	}
	if s := vmState(fm, vmID); s != VMStateRunning {
		t.Fatalf("state %s, want running", s)
	}

	// the fleet ceiling says stuck, the spec allows an hour.
	fm, vmID = fleetWithRunningVM(t, FleetConfig{TaskStuckTimeout: 10 * time.Minute}, "task-1", 30*time.Minute)
	fm.mu.Lock()
	fm.vms[vmID].spec.MaxRuntime = time.Hour
	fm.mu.Unlock()
	if v, _ := fm.ExpiryCheck(ctx, vmID, ""); v.Strike != "" || v.Reap {
		t.Fatalf("spec max runtime over the default: %+v, want no strike", v)
	}

	// a vm whose destroy failed belongs to a DELETE now and is not watched.
	fm, vmID = idleFleet(t, FleetConfig{DefaultIdleTimeout: 5 * time.Minute}, "task-1", 30*time.Minute)
	fm.mu.Lock()
	fm.vms[vmID].state = VMStateDestroyFailed
	fm.mu.Unlock()
	if v, _ := fm.ExpiryCheck(ctx, vmID, "idle_timeout"); v != (ExpiryVerdict{}) {
		t.Fatalf("destroy_failed vm: %+v, want nothing", v)
	}
}
