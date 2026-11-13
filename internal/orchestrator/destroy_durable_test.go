package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

// inlineDestroyer stands in for restate: it runs the destroy steps in order
// on the fleet, the way the Environment.Destroy handler does, and records
// every request it gets.
type inlineDestroyer struct {
	fm *FleetManager

	mu   sync.Mutex
	sent []DestroyRequest
	sync []DestroyRequest
}

func (d *inlineDestroyer) run(ctx context.Context, vmID string, req DestroyRequest) error {
	target, err := d.fm.DestroyBegin(ctx, vmID, req.Reason)
	if err != nil {
		return err
	}
	hostID := target.HostID
	if !target.Tracked {
		if req.HostID == "" {
			return nil
		}
		hostID = req.HostID
	}
	_ = d.fm.DestroyReleaseEgress(ctx, vmID)
	if err := d.fm.DestroyOnHost(ctx, vmID, hostID); err != nil {
		_ = d.fm.DestroyFailed(ctx, vmID, req.Reason, err.Error())
		return err
	}
	_ = d.fm.DestroyUnpublishIngress(ctx, vmID)
	return d.fm.DestroyFinish(ctx, vmID, req.Reason)
}

func (d *inlineDestroyer) Destroy(ctx context.Context, vmID string, req DestroyRequest) error {
	d.mu.Lock()
	d.sync = append(d.sync, req)
	d.mu.Unlock()
	return d.run(ctx, vmID, req)
}

func (d *inlineDestroyer) SendDestroy(ctx context.Context, vmID string, req DestroyRequest) error {
	d.mu.Lock()
	d.sent = append(d.sent, req)
	d.mu.Unlock()
	return d.run(ctx, vmID, req)
}

func (d *inlineDestroyer) requests() (sent, sync []DestroyRequest) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]DestroyRequest(nil), d.sent...), append([]DestroyRequest(nil), d.sync...)
}

// destroyErrProvider fails every Destroy with err.
type destroyErrProvider struct {
	*mockProvider
	err error
}

func (p *destroyErrProvider) Destroy(context.Context, string) error { return p.err }

func newDurableFleet(t *testing.T, p Provider) (*FleetManager, *inlineDestroyer, *MemoryStateStore) {
	t.Helper()
	store := NewMemoryStateStore()
	fm := NewFleetManager(FleetConfig{Provider: p, StateStore: store, Prefix: "fuse-"})
	d := &inlineDestroyer{fm: fm}
	fm.SetDurableDestroyer(d)
	return fm, d, store
}

func TestDurableDestroyVM(t *testing.T) {
	p := newMockProvider()
	fm, d, store := newDurableFleet(t, p)
	ctx := context.Background()
	if _, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := fm.DestroyVM(ctx, "fuse-task-1"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, sync := d.requests(); len(sync) != 1 || sync[0].Reason != DestroyReasonManual {
		t.Fatalf("durable requests %+v, want one manual destroy", sync)
	}
	if p.count() != 0 || len(fm.ListFleet()) != 0 {
		t.Fatalf("provider has %d envs, fleet %d vms; want both empty", p.count(), len(fm.ListFleet()))
	}
	vms, _ := store.ListVMs(ctx)
	if len(vms) != 0 {
		t.Fatalf("store still has %d vm rows", len(vms))
	}
	tasks, _ := store.ListTasks(ctx)
	if len(tasks) != 1 || tasks[0].RunStatus != TaskRunFailed {
		t.Fatalf("tasks %+v, want task-1 failed", tasks)
	}
}

func TestDurableDestroyVMUnknownIsNotFound(t *testing.T) {
	fm, d, _ := newDurableFleet(t, newMockProvider())
	err := fm.DestroyVM(context.Background(), "fuse-missing")
	if !errors.Is(err, ErrVMNotFound) {
		t.Fatalf("err %v, want ErrVMNotFound", err)
	}
	if _, sync := d.requests(); len(sync) != 0 {
		t.Fatalf("an unknown vm reached restate: %+v", sync)
	}
}

func TestDurableCompleteTaskSendsDestroy(t *testing.T) {
	p := newMockProvider()
	fm, d, _ := newDurableFleet(t, p)
	ctx := context.Background()
	if _, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := fm.CompleteTask("task-1"); err != nil {
		t.Fatal(err)
	}
	if sent, _ := d.requests(); len(sent) != 1 || sent[0].Reason != DestroyReasonTaskCompleted {
		t.Fatalf("sent %+v, want one task_completed destroy", sent)
	}
	if p.count() != 0 || len(fm.ListFleet()) != 0 {
		t.Fatal("vm survived the durable destroy")
	}
}

func TestDurableCreateCleanupCarriesHost(t *testing.T) {
	p := newMockProvider()
	p.createFn = func(context.Context, Spec) (Environment, error) {
		return nil, errors.New("boom")
	}
	fm, d, _ := newDurableFleet(t, p)
	if _, err := fm.ProvisionAndAssign(context.Background(), "task-1", Spec{}, []byte(`{}`), nil, BootOptions{}); err == nil {
		t.Fatal("provision succeeded, want an error")
	}
	sent, _ := d.requests()
	if len(sent) != 1 || sent[0].Reason != DestroyReasonCreateCleanup {
		t.Fatalf("sent %+v, want one create_cleanup destroy", sent)
	}
}

func TestDestroyOnHostTreatsNotFoundAsDone(t *testing.T) {
	p := &destroyErrProvider{mockProvider: newMockProvider(), err: &HTTPStatusError{Code: http.StatusNotFound}}
	fm, _, _ := newDurableFleet(t, p)
	if err := fm.DestroyOnHost(context.Background(), "fuse-gone", ""); err != nil {
		t.Fatalf("404 from the host: %v, want success", err)
	}
}

func TestDurableDestroyFailureDeadLetters(t *testing.T) {
	mp := newMockProvider()
	p := &destroyErrProvider{mockProvider: mp, err: errors.New("host down")}
	fm, _, store := newDurableFleet(t, p)
	ctx := context.Background()
	if _, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := fm.DestroyVM(ctx, "fuse-task-1"); err == nil {
		t.Fatal("destroy succeeded against a failing host")
	}
	info, ok := fm.GetVM("fuse-task-1")
	if !ok || info.State != VMStateDestroyFailed {
		t.Fatalf("vm %+v (tracked %v), want destroy_failed", info, ok)
	}
	dls, _ := store.ListDeadLetters(ctx)
	if len(dls) != 1 || dls[0].Kind != DeadLetterDestroy || dls[0].Reason != "host down" {
		t.Fatalf("dead letters %+v, want one destroy entry with the host error", dls)
	}

	// the host recovers: a DELETE on the destroy_failed vm finishes it.
	p.err = nil
	if err := fm.DestroyVM(ctx, "fuse-task-1"); err != nil {
		t.Fatalf("retry destroy: %v", err)
	}
	if _, ok := fm.GetVM("fuse-task-1"); ok {
		t.Fatal("vm still tracked after the retried destroy")
	}
}

func TestDestroyFinishIsIdempotent(t *testing.T) {
	p := newMockProvider()
	fm, _, _ := newDurableFleet(t, p)
	ctx := context.Background()
	if _, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := fm.DestroyFinish(ctx, "fuse-task-1", DestroyReasonManual); err != nil {
			t.Fatalf("finish %d: %v", i, err)
		}
	}
	if _, ok := fm.GetVM("fuse-task-1"); ok {
		t.Fatal("vm still tracked after finish")
	}
}

func TestRetryFailedTeardownsResendsDurably(t *testing.T) {
	p := newMockProvider()
	fm, d, _ := newDurableFleet(t, p)
	ctx := context.Background()
	if _, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatal(err)
	}
	// a vm left destroying long ago, e.g. by a mode off release before the
	// upgrade, with nothing in restate working on it.
	fm.mu.Lock()
	v := fm.vms["fuse-task-1"]
	v.state = VMStateDestroying
	v.updatedAt = time.Now().Add(-2 * teardownRetryAfter)
	fm.mu.Unlock()

	fm.retryFailedTeardowns(map[string]bool{"fuse-task-1": true})
	if sent, _ := d.requests(); len(sent) != 1 || sent[0].Reason != DestroyReasonRetry {
		t.Fatalf("sent %+v, want one teardown_retry destroy", sent)
	}
	if p.count() != 0 {
		t.Fatal("re-sent destroy did not reach the host")
	}
}
