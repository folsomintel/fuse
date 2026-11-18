package orchestrator

import (
	"context"
	"errors"
	"fmt"
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
	if target.Replaced {
		return nil
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

// blockingDestroyer holds every send until release is closed.
type blockingDestroyer struct {
	inlineDestroyer
	release chan struct{}
	mu      sync.Mutex
	active  int
	peak    int
}

func (d *blockingDestroyer) SendDestroy(ctx context.Context, vmID string, req DestroyRequest) error {
	d.mu.Lock()
	d.active++
	if d.active > d.peak {
		d.peak = d.active
	}
	d.mu.Unlock()
	<-d.release
	d.mu.Lock()
	d.active--
	d.mu.Unlock()
	return nil
}

// a restate that never answers must not hold up the caller, and holds at
// most maxDestroySends sends at once.
func TestDestroyAsyncDoesNotBlockAndIsBounded(t *testing.T) {
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), Prefix: "fuse-"})
	d := &blockingDestroyer{release: make(chan struct{})}
	fm.SetDurableDestroyer(d)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 3*maxDestroySends; i++ {
			fm.destroyAsync(fmt.Sprintf("fuse-%d", i), DestroyReasonExpired)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("destroyAsync blocked on a send")
	}
	time.Sleep(50 * time.Millisecond)
	close(d.release)
	fm.destroySends.Wait()
	if d.peak > maxDestroySends {
		t.Fatalf("%d sends in flight, want at most %d", d.peak, maxDestroySends)
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
	fm.destroySends.Wait()
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

// a retried create reuses the vm id; the first attempt's cleanup arriving
// after it must not destroy the replacement.
func TestCreateCleanupSparesReplacement(t *testing.T) {
	p := newMockProvider()
	fm, d, _ := newDurableFleet(t, p)
	ctx := context.Background()
	if _, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := d.SendDestroy(ctx, "fuse-task-1", DestroyRequest{Reason: DestroyReasonCreateCleanup, HostID: "h1"}); err != nil {
		t.Fatal(err)
	}
	info, ok := fm.GetVM("fuse-task-1")
	if !ok || info.State != VMStateRunning {
		t.Fatalf("vm %+v (tracked %v), want the replacement still running", info, ok)
	}
	if p.count() != 1 {
		t.Fatal("cleanup destroyed the replacement on the host")
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

// deadLetterErrStore fails every dead-letter write.
type deadLetterErrStore struct{ *MemoryStateStore }

func (deadLetterErrStore) UpsertDeadLetter(context.Context, DeadLetterRecord) error {
	return errors.New("injected dead letter failure")
}

// the dead letter is the record of a destroy that gave up, so a failed write
// fails the step for restate to retry instead of being dropped.
func TestDestroyFailedReturnsDeadLetterError(t *testing.T) {
	fm := NewFleetManager(FleetConfig{
		Provider:   newMockProvider(),
		StateStore: deadLetterErrStore{NewMemoryStateStore()},
		Prefix:     "fuse-",
	})
	if err := fm.DestroyFailed(context.Background(), "fuse-gone", DestroyReasonManual, "host down"); err == nil {
		t.Fatal("dead letter write failed but the step succeeded")
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

// failOnceStore fails the next DeleteVM or UpsertVM once.
type failOnceStore struct {
	*MemoryStateStore
	mu         sync.Mutex
	failDelete bool
	failUpsert bool
}

func (s *failOnceStore) DeleteVM(ctx context.Context, vmID string) error {
	s.mu.Lock()
	fail := s.failDelete
	s.failDelete = false
	s.mu.Unlock()
	if fail {
		return errors.New("injected delete failure")
	}
	return s.MemoryStateStore.DeleteVM(ctx, vmID)
}

func (s *failOnceStore) UpsertVM(ctx context.Context, rec VMRecord) error {
	s.mu.Lock()
	fail := s.failUpsert
	s.failUpsert = false
	s.mu.Unlock()
	if fail {
		return errors.New("injected upsert failure")
	}
	return s.MemoryStateStore.UpsertVM(ctx, rec)
}

// waitForState reads ch until an event in state want arrives.
func waitForState(t *testing.T, ch <-chan EnvironmentEvent, want VMState) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.State == want {
				return
			}
		case <-timeout:
			t.Fatalf("no %s event", want)
		}
	}
}

// a step that fails a write and is retried must still send the events the
// first attempt never reached.
func TestDestroyStepRetriesKeepEvents(t *testing.T) {
	store := &failOnceStore{MemoryStateStore: NewMemoryStateStore()}
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), StateStore: store, Prefix: "fuse-"})
	ctx := context.Background()
	if _, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatal(err)
	}
	ch, cancel := fm.SubscribeEnvironmentEvents("fuse-task-1")
	defer cancel()

	store.mu.Lock()
	store.failUpsert = true
	store.mu.Unlock()
	if _, err := fm.DestroyBegin(ctx, "fuse-task-1", DestroyReasonManual); err == nil {
		t.Fatal("begin succeeded with a failing persist")
	}
	if _, err := fm.DestroyBegin(ctx, "fuse-task-1", DestroyReasonManual); err != nil {
		t.Fatalf("retried begin: %v", err)
	}
	waitForState(t, ch, VMStateDestroying)

	store.mu.Lock()
	store.failDelete = true
	store.mu.Unlock()
	if err := fm.DestroyFinish(ctx, "fuse-task-1", DestroyReasonManual); err == nil {
		t.Fatal("finish succeeded with a failing delete")
	}
	if err := fm.DestroyFinish(ctx, "fuse-task-1", DestroyReasonManual); err != nil {
		t.Fatalf("retried finish: %v", err)
	}
	waitForState(t, ch, VMStateDestroyed)
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
	fm.destroySends.Wait()
	if sent, _ := d.requests(); len(sent) != 1 || sent[0].Reason != DestroyReasonRetry {
		t.Fatalf("sent %+v, want one teardown_retry destroy", sent)
	}
	if p.count() != 0 {
		t.Fatal("re-sent destroy did not reach the host")
	}
}

// a destroy_failed vm that later vanishes from its host is reaped with its
// keys revoked: the failed destroy never reached DestroyFinish to do it.
func TestReapDestroyFailedRevokesKeys(t *testing.T) {
	p := &destroyErrProvider{mockProvider: newMockProvider(), err: errors.New("host down")}
	fm, _, _ := newDurableFleet(t, p)
	keys := &fakeEnvKeys{}
	fm.SetEnvKeyStore(keys)
	ctx := context.Background()
	if _, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := fm.DestroyVM(ctx, "fuse-task-1"); err == nil {
		t.Fatal("destroy succeeded against a failing host")
	}
	if len(keys.revoked) != 0 {
		t.Fatalf("keys revoked before the vm is gone: %v", keys.revoked)
	}

	fm.reapDestroyedVMs(ctx, map[string]bool{}, vmListing{})
	if _, ok := fm.GetVM("fuse-task-1"); ok {
		t.Fatal("destroy_failed vm not reaped")
	}
	keys.mu.Lock()
	defer keys.mu.Unlock()
	if len(keys.revoked) != 1 || keys.revoked[0] != "fuse-task-1" {
		t.Fatalf("revoked %v, want the reaped vm", keys.revoked)
	}
}
