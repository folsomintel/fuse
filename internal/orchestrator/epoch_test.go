package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

// incarnationProvider is a host that holds name at epoch host: a Get under
// another epoch is the IncarnationError a real provider returns. it records
// the epoch every Destroy and Create came under.
type incarnationProvider struct {
	*mockProvider
	host int64

	mu       sync.Mutex
	destroys []int64
	creates  []int64
}

func (p *incarnationProvider) Get(ctx context.Context, name string) (Environment, error) {
	if err := CheckIncarnation(ctx, name, p.host); err != nil {
		return nil, err
	}
	return p.mockProvider.Get(ctx, name)
}

func (p *incarnationProvider) Destroy(ctx context.Context, name string) error {
	p.mu.Lock()
	p.destroys = append(p.destroys, VMEpochFrom(ctx))
	p.mu.Unlock()
	return p.mockProvider.Destroy(ctx, name)
}

func (p *incarnationProvider) Create(ctx context.Context, spec Spec) (Environment, error) {
	p.mu.Lock()
	p.creates = append(p.creates, VMEpochFrom(ctx))
	p.mu.Unlock()
	return p.mockProvider.Create(ctx, spec)
}

func TestRestoreOrCreateReplacesAnEarlierIncarnation(t *testing.T) {
	p := &incarnationProvider{mockProvider: newMockProvider(), host: 100}
	ctx := context.Background()
	if _, err := p.mockProvider.Create(ctx, Spec{Name: "vm"}); err != nil {
		t.Fatal(err)
	}
	// it used to adopt and restore whatever held the name.
	_, fromCache, err := restoreOrCreate(WithVMEpoch(ctx, 200), p, Spec{Name: "vm"})
	if err != nil {
		t.Fatal(err)
	}
	if fromCache {
		t.Fatal("an earlier incarnation's vm was restored instead of replaced")
	}
	if len(p.destroys) != 1 || p.destroys[0] != 100 {
		t.Fatalf("destroys under epochs %v, want one under the earlier incarnation's 100", p.destroys)
	}
	if len(p.creates) != 1 || p.creates[0] != 200 {
		t.Fatalf("creates under epochs %v, want one under 200", p.creates)
	}
}

func TestRestoreOrCreateRefusesForALaterIncarnation(t *testing.T) {
	p := &incarnationProvider{mockProvider: newMockProvider(), host: 300}
	ctx := context.Background()
	if _, err := p.mockProvider.Create(ctx, Spec{Name: "vm"}); err != nil {
		t.Fatal(err)
	}
	_, _, err := restoreOrCreate(WithVMEpoch(ctx, 200), p, Spec{Name: "vm"})
	if !errors.Is(err, ErrOtherIncarnation) {
		t.Fatalf("err = %v, want ErrOtherIncarnation", err)
	}
	if len(p.destroys) != 0 || len(p.creates) != 0 {
		t.Fatalf("a stale boot destroyed %v and created %v; want neither", p.destroys, p.creates)
	}
}

func TestRestoreOrCreateKeepsItsOwnIncarnation(t *testing.T) {
	p := &incarnationProvider{mockProvider: newMockProvider(), host: 200}
	ctx := context.Background()
	if _, err := p.mockProvider.Create(ctx, Spec{Name: "vm"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := restoreOrCreate(WithVMEpoch(ctx, 200), p, Spec{Name: "vm"}); err != nil {
		t.Fatal(err)
	}
	if len(p.destroys) != 0 || len(p.creates) != 0 {
		t.Fatalf("a retried boot destroyed %v and created %v; want neither", p.destroys, p.creates)
	}
}

func TestDurableDestroyIsFencedToTheVM(t *testing.T) {
	p := &incarnationProvider{mockProvider: newMockProvider()}
	fm, _, _ := newDurableFleet(t, p)
	ctx := context.Background()
	if _, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatal(err)
	}
	fm.mu.RLock()
	want := fm.vms["fuse-task-1"].epoch()
	fm.mu.RUnlock()
	if err := fm.DestroyVM(ctx, "fuse-task-1"); err != nil {
		t.Fatal(err)
	}
	if want == 0 || len(p.destroys) != 1 || p.destroys[0] != want {
		t.Fatalf("destroys under epochs %v, want one under the vm's %d", p.destroys, want)
	}
	if len(p.creates) != 1 || p.creates[0] != want {
		t.Fatalf("creates under epochs %v, want one under the vm's %d", p.creates, want)
	}
}

func TestDurableDestroyTakesAStaleEpochAsGone(t *testing.T) {
	p := &destroyErrProvider{mockProvider: newMockProvider(), err: &HTTPStatusError{Code: http.StatusPreconditionFailed, Body: "vm is epoch 2"}}
	fm, _, store := newDurableFleet(t, p)
	ctx := context.Background()
	if _, err := fm.ProvisionAndAssign(ctx, "task-1", Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := fm.DestroyVM(ctx, "fuse-task-1"); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if len(fm.ListFleet()) != 0 {
		t.Fatal("vm still tracked after its host reported another incarnation")
	}
	if rows, err := store.ListVMs(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("rows %v (err %v) after its host reported another incarnation; want none", rows, err)
	}
}

func TestOrphanDestroyTakesAStaleEpochAsGone(t *testing.T) {
	stale := &destroyErrProvider{mockProvider: newMockProvider(), err: &HTTPStatusError{Code: http.StatusPreconditionFailed}}
	if err := destroyOrphan(context.Background(), stale, "vm"); err != nil {
		t.Fatalf("412: %v, want nil", err)
	}
	down := &destroyErrProvider{mockProvider: newMockProvider(), err: &HTTPStatusError{Code: http.StatusServiceUnavailable}}
	if err := destroyOrphan(context.Background(), down, "vm"); err == nil {
		t.Fatal("503 was taken as destroyed")
	}
}

func TestVMEpochSurvivesStorePrecision(t *testing.T) {
	// the store keeps microseconds; a restart must not change the epoch.
	at := time.Date(2026, 11, 24, 10, 0, 0, 123456789, time.UTC)
	if VMEpoch(at) != VMEpoch(at.Truncate(time.Microsecond)) {
		t.Fatal("epoch changed when createdAt lost its nanoseconds")
	}
	if VMEpoch(time.Time{}) != 0 {
		t.Fatal("an unset createdAt has an epoch")
	}
	if VMEpoch(at.Add(time.Millisecond)) <= VMEpoch(at) {
		t.Fatal("a later incarnation's epoch is not newer")
	}
}
