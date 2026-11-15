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
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-"})
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

// TestDurableDrain: with restate on, Drain goes through the durable handler,
// which runs the command once and leaves the vm draining.
func TestDurableDrain(t *testing.T) {
	p := newMockProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
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

	// a second drain is refused before restate sees it, as in mode off.
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
	fm := NewFleetManager(FleetConfig{Provider: p, Prefix: "fuse-"})
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

	v, err := fm.ExpiryCheck(ctx, vmID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Strike || v.Reap || v.In != fm.reconcileInterval {
		t.Fatalf("first check %+v, want a strike rechecked after one reconcile interval", v)
	}
	if s := vmState(fm, vmID); s != VMStateRunning {
		t.Fatalf("state after one strike %s, want running", s)
	}

	v, err = fm.ExpiryCheck(ctx, vmID, 1)
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
	if v, _ := fm.ExpiryCheck(ctx, vmID, 1); !v.Reap {
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
	for strikes := range 2 {
		v, err := fm.ExpiryCheck(context.Background(), vmID, strikes)
		if err != nil {
			t.Fatal(err)
		}
		if v.Reap || v.Strike || v.In != expiryRecheck {
			t.Fatalf("check with %d strikes %+v, want a recheck", strikes, v)
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
	if v, _ := fm.ExpiryCheck(ctx, vmID, 0); v.Strike || v.Reap || !near(v.In, 4*time.Minute) {
		t.Fatalf("idle for 1m of 5m: %+v, want a check in 4m", v)
	}

	fm.mu.Lock()
	fm.vms[vmID].attachSessions = 1
	fm.vms[vmID].lastActivityAt = time.Now().Add(-time.Hour)
	fm.mu.Unlock()
	if v, _ := fm.ExpiryCheck(ctx, vmID, 0); v.Strike || !near(v.In, 5*time.Minute) {
		t.Fatalf("attached: %+v, want a check in one timeout", v)
	}

	fm.mu.Lock()
	fm.vms[vmID].attachSessions = 0
	fm.mu.Unlock()
	fm.hitlMu.Lock()
	fm.signalStateLocked(vmID).extendedUntil = time.Now().Add(20 * time.Minute)
	fm.hitlMu.Unlock()
	if v, _ := fm.ExpiryCheck(ctx, vmID, 1); v.Reap || v.Strike || !near(v.In, 20*time.Minute) {
		t.Fatalf("extended: %+v, want a check when the extension ends", v)
	}

	// no task means no runtime ceiling, and no idle timeout: nothing to watch.
	plain, plainID := fleetWithRunningVM(t, FleetConfig{}, "", time.Hour)
	if v, _ := plain.ExpiryCheck(ctx, plainID, 0); v != (ExpiryVerdict{}) {
		t.Fatalf("unwatched vm: %+v, want nothing", v)
	}
	if v, _ := plain.ExpiryCheck(ctx, "fuse-gone", 0); v != (ExpiryVerdict{}) {
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
	if v, _ := fm.ExpiryCheck(ctx, vmID, 1); v.Reap || v.In != expiryRecheck {
		t.Fatalf("pending expiry: %+v, want a recheck", v)
	}
	if err := store.UpsertIntervention(ctx, InterventionRecord{
		ID: "int_exp", EnvID: vmID, Kind: string(hitl.KindExpiry), State: string(hitl.StateRejected),
		Deadline: time.Now().Add(time.Hour), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if v, _ := fm.ExpiryCheck(ctx, vmID, 0); !v.Strike {
		t.Fatalf("past the ceiling: %+v, want a strike", v)
	}
	if v, _ := fm.ExpiryCheck(ctx, vmID, 1); !v.Reap {
		t.Fatalf("second strike: %+v, want a reap", v)
	}
}

// TestExpiryCheckDraining: a draining vm is reaped once the grace is up,
// rescheduled for the rest before then, and left alone when its hitl policy
// gates destroy.
func TestExpiryCheckDraining(t *testing.T) {
	ctx := context.Background()
	fm, vmID := drainingFleet(t, FleetConfig{DrainingTimeout: 10 * time.Minute}, "task-1", 4*time.Minute)
	if v, _ := fm.ExpiryCheck(ctx, vmID, 0); v.Reap || v.In <= 5*time.Minute || v.In > 6*time.Minute {
		t.Fatalf("4m into a 10m grace: %+v, want a check in about 6m", v)
	}
	fm.mu.Lock()
	fm.vms[vmID].updatedAt = time.Now().Add(-11 * time.Minute)
	fm.mu.Unlock()
	if v, _ := fm.ExpiryCheck(ctx, vmID, 0); !v.Reap {
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
	if v, _ := gated.ExpiryCheck(ctx, gatedID, 0); v != (ExpiryVerdict{}) {
		t.Fatalf("destroy gated: %+v, want nothing", v)
	}
}

// TestDurableReconcileArmsInsteadOfReaping: with restate on, reconcile keeps
// no strikes and reaps nothing itself; it arms each vm once, and again after
// a failed send.
func TestDurableReconcileArmsInsteadOfReaping(t *testing.T) {
	fm, vmID := idleFleet(t, FleetConfig{DefaultIdleTimeout: 5 * time.Minute}, "task-1", 30*time.Minute)
	d := withDurableExpiry(fm)
	d.failArms = 1
	for range 3 {
		fm.reconcile(context.Background())
		fm.expirySends.Wait()
	}
	if s := vmState(fm, vmID); s != VMStateRunning {
		t.Fatalf("state %s, want running: reconcile reaped with restate on", s)
	}
	fm.mu.RLock()
	strikes := len(fm.idleStrikes) + len(fm.stuckStrikes)
	fm.mu.RUnlock()
	if strikes != 0 {
		t.Fatalf("reconcile kept %d strikes in memory with restate on", strikes)
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
