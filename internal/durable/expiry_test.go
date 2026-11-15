package durable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// expiryFleet scripts the drain and expiry steps on top of scriptedFleet's
// destroy steps, so a test sees the destroy the timer sends.
type expiryFleet struct {
	*scriptedFleet
	plan     orchestrator.DrainPlan
	checkErr error
	beginErr error
	cmdMsg   string
	// verdicts are what successive checks return; past the end a check
	// finds nothing left to watch.
	verdicts []orchestrator.ExpiryVerdict
	strikes  []string
}

func newExpiryFleet(vm string, verdicts ...orchestrator.ExpiryVerdict) *expiryFleet {
	return &expiryFleet{scriptedFleet: newScriptedFleet(0, vm), verdicts: verdicts}
}

func (f *expiryFleet) DrainCheck(_ context.Context, vm string) error {
	f.record(vm, "drain_check")
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.checkErr
	// the first drain leaves the vm draining: a second one is refused.
	f.checkErr = fmt.Errorf("%w: vm %s is in state draining", orchestrator.ErrVMNotRunning, vm)
	return err
}

func (f *expiryFleet) DrainBegin(_ context.Context, vm string) (orchestrator.DrainPlan, error) {
	f.record(vm, "drain_begin")
	return f.plan, f.beginErr
}

func (f *expiryFleet) DrainCommand(_ context.Context, vm string) string {
	f.record(vm, "drain_command")
	return f.cmdMsg
}

func (f *expiryFleet) ExpiryCheck(_ context.Context, vm, strike string) (orchestrator.ExpiryVerdict, error) {
	f.record(vm, "check")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.strikes = append(f.strikes, strike)
	if len(f.verdicts) == 0 {
		return orchestrator.ExpiryVerdict{}, nil
	}
	v := f.verdicts[0]
	f.verdicts = f.verdicts[1:]
	return v, nil
}

func (f *expiryFleet) seenStrikes() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.strikes, "|")
}

// waitSteps waits until vm's steps are want.
func waitSteps(t *testing.T, f *scriptedFleet, vm, want string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if f.steps(vm) == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("steps %s, want %s", f.steps(vm), want)
}

const destroySteps = "begin,egress,host,ingress,finish"

// TestDrainThroughRestateDestroysAfterGrace: a drain arms its grace timer,
// runs the command, and returns; once the grace is up the timer's check
// reaps the vm and the destroy runs on the same object.
func TestDrainThroughRestateDestroysAfterGrace(t *testing.T) {
	vm := uniqueVM("drain")
	fleet := newExpiryFleet(vm, orchestrator.ExpiryVerdict{Reap: true})
	fleet.plan = orchestrator.DrainPlan{Command: true, Grace: 300 * time.Millisecond}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Drain(ctx, vm); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := fleet.steps(vm); got != "drain_check,drain_begin,drain_command" {
		t.Fatalf("steps right after the drain %s, want only the drain's: the grace has not passed", got)
	}
	waitSteps(t, fleet.scriptedFleet, vm, "drain_check,drain_begin,drain_command,check,"+destroySteps)
}

// TestDrainFailureStillDestroys: a failed drain command comes back as the
// fleet's message, and the grace timer armed before it still reaps the vm.
func TestDrainFailureStillDestroys(t *testing.T) {
	vm := uniqueVM("drain-fail")
	fleet := newExpiryFleet(vm, orchestrator.ExpiryVerdict{Reap: true})
	fleet.plan = orchestrator.DrainPlan{Command: true, Grace: 200 * time.Millisecond}
	fleet.cmdMsg = "drain command: guest refused"
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := client.Drain(ctx, vm)
	if err == nil || err.Error() != fmt.Sprintf("drain vm %s: drain command: guest refused", vm) {
		t.Fatalf("drain error %v, want the fleet's message", err)
	}
	waitSteps(t, fleet.scriptedFleet, vm, "drain_check,drain_begin,drain_command,check,"+destroySteps)
}

// TestDrainRefusalKeepsItsSentinel: a vm the begin step refuses answers
// ErrVMNotRunning, arms no timer and runs no command.
func TestDrainRefusalKeepsItsSentinel(t *testing.T) {
	vm := uniqueVM("drain-refused")
	fleet := newExpiryFleet(vm)
	fleet.beginErr = fmt.Errorf("%w: vm %s is in state destroying", orchestrator.ErrVMNotRunning, vm)
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Drain(ctx, vm); !errors.Is(err, orchestrator.ErrVMNotRunning) {
		t.Fatalf("drain error %v, want ErrVMNotRunning", err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := fleet.steps(vm); got != "drain_check,drain_begin" {
		t.Fatalf("steps %s, want only drain_check,drain_begin", got)
	}
}

// TestExpireStrikesThenReaps: the first check strikes, the timer it schedules
// brings the strike to the next check, which reaps.
func TestExpireStrikesThenReaps(t *testing.T) {
	vm := uniqueVM("expire-strike")
	fleet := newExpiryFleet(vm,
		orchestrator.ExpiryVerdict{Strike: "idle_timeout", In: 200 * time.Millisecond},
		orchestrator.ExpiryVerdict{Reap: true})
	client := serveFleet(t, fleet, fastPolicy)

	if err := client.SendArmExpiry(context.Background(), vm); err != nil {
		t.Fatalf("arm: %v", err)
	}
	waitSteps(t, fleet.scriptedFleet, vm, "check,check,"+destroySteps)
	if got := fleet.seenStrikes(); got != "|idle_timeout" {
		t.Fatalf("strikes the checks saw %q, want none then idle_timeout", got)
	}
}

// TestExpireRearmSupersedesTimer: a second arm takes over from the timer the
// first one scheduled, which then fires as a no-op: an extension moves the
// deadline without the old one still reaping.
func TestExpireRearmSupersedesTimer(t *testing.T) {
	vm := uniqueVM("expire-rearm")
	fleet := newExpiryFleet(vm,
		orchestrator.ExpiryVerdict{In: 300 * time.Millisecond},
		orchestrator.ExpiryVerdict{In: 600 * time.Millisecond},
		orchestrator.ExpiryVerdict{Reap: true})
	client := serveFleet(t, fleet, fastPolicy)

	ctx := context.Background()
	if err := client.SendArmExpiry(ctx, vm); err != nil {
		t.Fatal(err)
	}
	waitSteps(t, fleet.scriptedFleet, vm, "check")
	if err := client.SendArmExpiry(ctx, vm); err != nil {
		t.Fatal(err)
	}
	waitSteps(t, fleet.scriptedFleet, vm, "check,check")
	// the first timer fires around now and must not run a check.
	time.Sleep(400 * time.Millisecond)
	if got := fleet.steps(vm); got != "check,check" {
		t.Fatalf("steps %s after the superseded timer fired, want no third check yet", got)
	}
	waitSteps(t, fleet.scriptedFleet, vm, "check,check,check,"+destroySteps)
}

// TestExpireNothingToWatch: a check that finds nothing to watch schedules
// nothing.
func TestExpireNothingToWatch(t *testing.T) {
	vm := uniqueVM("expire-none")
	fleet := newExpiryFleet(vm)
	client := serveFleet(t, fleet, fastPolicy)
	if err := client.SendArmExpiry(context.Background(), vm); err != nil {
		t.Fatal(err)
	}
	waitSteps(t, fleet.scriptedFleet, vm, "check")
	time.Sleep(500 * time.Millisecond)
	if got := fleet.steps(vm); got != "check" {
		t.Fatalf("steps %s, want one check and nothing after", got)
	}
}

// TestExpireSurvivesEndpointRestart stands in for kill -9 of the
// orchestrator between a first strike and its second check: the endpoint
// goes away with the timer pending, and the restarted one gets the check
// with the strike restate kept, and reaps.
func TestExpireSurvivesEndpointRestart(t *testing.T) {
	vm := uniqueVM("expire-restart")
	first := newExpiryFleet(vm, orchestrator.ExpiryVerdict{Strike: "idle_timeout", In: time.Second})
	client, addr, stop := serveFleetAt(t, first, "")
	if err := client.SendArmExpiry(context.Background(), vm); err != nil {
		t.Fatal(err)
	}
	waitSteps(t, first.scriptedFleet, vm, "check")
	stop()

	second := newExpiryFleet(vm, orchestrator.ExpiryVerdict{Reap: true})
	_, _, _ = serveFleetAt(t, second, addr)
	waitSteps(t, second.scriptedFleet, vm, "check,"+destroySteps)
	if got := second.seenStrikes(); got != "idle_timeout" {
		t.Fatalf("strike after the restart %q, want idle_timeout: the strike lives in object state", got)
	}
	if strings.Contains(first.steps(vm), "begin") {
		t.Fatalf("the stopped endpoint ran the destroy: %s", first.steps(vm))
	}
}

// TestSecondDrainIsRefused: a second drain queued behind the first on the
// object is refused by its check step, runs no command and arms no timer.
func TestSecondDrainIsRefused(t *testing.T) {
	vm := uniqueVM("drain-twice")
	fleet := newExpiryFleet(vm)
	fleet.plan = orchestrator.DrainPlan{Command: true, Grace: time.Hour}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := client.Drain(ctx, vm); err != nil {
		t.Fatalf("first drain: %v", err)
	}
	if err := client.Drain(ctx, vm); !errors.Is(err, orchestrator.ErrVMNotRunning) {
		t.Fatalf("second drain: %v, want ErrVMNotRunning", err)
	}
	if got := fleet.steps(vm); got != "drain_check,drain_begin,drain_command,drain_check" {
		t.Fatalf("steps %s, want the second drain stopped at its check", got)
	}
}
