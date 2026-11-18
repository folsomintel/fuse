package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDurableArtifacts stands in for the restate client: it records the jobs
// it was sent and the pins it holds.
type fakeDurableArtifacts struct {
	mu      sync.Mutex
	jobs    []ArtifactMoveJob
	moveErr error
	pinErr  error
	pins    map[string]map[string]time.Duration
}

func newFakeDurableArtifacts() *fakeDurableArtifacts {
	return &fakeDurableArtifacts{pins: map[string]map[string]time.Duration{}}
}

func (f *fakeDurableArtifacts) MoveArtifact(_ context.Context, job ArtifactMoveJob) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs = append(f.jobs, job)
	if f.moveErr != nil {
		return "", f.moveErr
	}
	return job.LocalID, nil
}

func (f *fakeDurableArtifacts) PinArtifacts(_ context.Context, holder string, ttl time.Duration, ids ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pinErr != nil {
		return f.pinErr
	}
	for _, id := range ids {
		if f.pins[id] == nil {
			f.pins[id] = map[string]time.Duration{}
		}
		f.pins[id][holder] = ttl
	}
	return nil
}

func (f *fakeDurableArtifacts) ReleaseArtifacts(_ context.Context, holder string, ids ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		delete(f.pins[id], holder)
	}
	return nil
}

func (f *fakeDurableArtifacts) ArtifactPinned(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pinErr != nil {
		return false, f.pinErr
	}
	return len(f.pins[id]) > 0, nil
}

// deletingProvider is a stub provider that can delete free-standing copies.
type deletingProvider struct {
	*stubLoadProvider
	mu      sync.Mutex
	deleted []string
}

func (p *deletingProvider) DeleteArtifact(_ context.Context, snapshotID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.deleted = append(p.deleted, snapshotID)
	return nil
}

func twoHostFleet(t *testing.T, mover ArtifactMover) (*FleetManager, *deletingProvider) {
	t.Helper()
	stub := &deletingProvider{stubLoadProvider: newStubProvider()}
	fm := newInlineFleet(FleetConfig{Provider: stub, Prefix: "fuse-", ArtifactMover: mover})
	for _, id := range []string{"h-a", "h-b"} {
		if err := fm.RegisterHost(context.Background(), artifactHost(id, 8), stub); err != nil {
			t.Fatal(err)
		}
	}
	return fm, stub
}

// TestEnsureArtifactOnHost_durable: the move goes to restate, carrying host
// ids and no agent token, and the fleet never calls the mover itself.
func TestEnsureArtifactOnHost_durable(t *testing.T) {
	ctx := context.Background()
	mover := &recordingMover{}
	fm, _ := twoHostFleet(t, mover)
	d := newFakeDurableArtifacts()
	fm.SetDurableArtifacts(d)
	record := seedArtifactRecord(t, fm, "art-1", "tenant-1", "h-a", "d1")

	got, err := fm.ensureArtifactOnHost(ctx, record, "h-b", false)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if want := replicaSnapshotID("d1", "h-b"); got != want {
		t.Fatalf("committed %q, want %q", got, want)
	}
	if n := len(mover.calls()); n != 0 {
		t.Fatalf("mover called directly %d times, want 0", n)
	}
	if len(d.jobs) != 1 {
		t.Fatalf("%d durable moves, want 1", len(d.jobs))
	}
	job := d.jobs[0]
	if job.SourceHostID != "h-a" || job.HostID != "h-b" || job.SnapshotID != "art-1" || job.TenantID != "tenant-1" {
		t.Fatalf("job %+v names the wrong ends", job)
	}
	if strings.Contains(fmt.Sprintf("%+v", job), "token-") {
		t.Fatalf("job %+v carries an agent token into the journal", job)
	}

	d.moveErr = errors.New("restate said no")
	_, err = fm.ensureArtifactOnHost(ctx, seedArtifactRecord(t, fm, "art-2", "tenant-1", "h-a", "d2"), "h-b", false)
	if !errors.Is(err, ErrArtifactTransferFailed) {
		t.Fatalf("failed durable move: %v, want ErrArtifactTransferFailed", err)
	}
}

// TestArtifactSteps: pull resolves endpoints from the fleet and record writes
// the copy, the steps the durable handler runs.
func TestArtifactSteps(t *testing.T) {
	ctx := context.Background()
	mover := &recordingMover{}
	fm, _ := twoHostFleet(t, mover)
	job := ArtifactMoveJob{
		SnapshotID: "art-1", Digest: "d1", SourceHostID: "h-a", SourceSnapshotID: "art-1",
		HostID: "h-b", LocalID: "art-copy", TenantID: "tenant-1", LayerKey: "layer-1", SizeBytes: 7,
	}
	if got, err := fm.ArtifactMoveCommitted(ctx, job); err != nil || got != "" {
		t.Fatalf("committed before any move = %q, %v", got, err)
	}
	moved, err := fm.ArtifactPull(ctx, job)
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	calls := mover.calls()
	if len(calls) != 1 || calls[0].From.Token != "token-h-a" || calls[0].To.Token != "token-h-b" {
		t.Fatalf("mover calls %+v, want one from h-a to h-b with their tokens", calls)
	}
	id, err := fm.ArtifactRecord(ctx, job, moved)
	if err != nil || id != "art-copy" {
		t.Fatalf("record = %q, %v", id, err)
	}
	rec, err := fm.GetSnapshotByID(ctx, "art-copy")
	if err != nil || rec.HostID != "h-b" || rec.LayerKey != "layer-1" || rec.Digest != "d1" {
		t.Fatalf("recorded copy %+v, %v", rec, err)
	}
	// a second move into the same copy stops at the check.
	if got, err := fm.ArtifactMoveCommitted(ctx, job); err != nil || got != "art-copy" {
		t.Fatalf("committed after the move = %q, %v", got, err)
	}

	gone := job
	gone.SourceHostID = "h-gone"
	if _, err := fm.ArtifactPull(ctx, gone); !PermanentArtifactError(err) {
		t.Fatalf("pull from an unregistered host: %v, want a permanent error", err)
	}

	// a source gc marked deleting after the move planned is not read.
	deleting := seedArtifactRecord(t, fm, "art-del", "tenant-1", "h-a", "d3")
	deleting.State = SnapshotStateDeleting
	if err := fm.upsertSnapshotRecord(ctx, deleting); err != nil {
		t.Fatalf("mark deleting: %v", err)
	}
	doomed := job
	doomed.SnapshotID, doomed.SourceSnapshotID, doomed.LocalID = "art-del", "art-del", "art-del-copy"
	if _, err := fm.ArtifactPull(ctx, doomed); !PermanentArtifactError(err) {
		t.Fatalf("pull of a deleting source: %v, want a permanent error", err)
	}
	if n := len(mover.calls()); n != 1 {
		t.Fatalf("mover ran %d times, want the deleting source never pulled", n)
	}
}

// TestArtifactDiscard deletes an unrecorded copy and keeps a recorded one.
func TestArtifactDiscard(t *testing.T) {
	ctx := context.Background()
	fm, stub := twoHostFleet(t, &recordingMover{})
	job := ArtifactMoveJob{HostID: "h-b", LocalID: "art-partial"}
	if err := fm.ArtifactDiscard(ctx, job); err != nil {
		t.Fatal(err)
	}
	seedArtifactRecord(t, fm, "art-kept", "tenant-1", "h-b", "d1")
	if err := fm.ArtifactDiscard(ctx, ArtifactMoveJob{HostID: "h-b", LocalID: "art-kept"}); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(stub.deleted) != "[art-partial]" {
		t.Fatalf("deleted %v, want only the unrecorded copy", stub.deleted)
	}
}

func TestPermanentArtifactError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("x: %w", ErrArtifactImmovable), true},
		{fmt.Errorf("x: %w", errArtifactKindMismatch), true},
		{fmt.Errorf("pull: %w", &HTTPStatusError{Code: 409}), true},
		{fmt.Errorf("pull: %w", &HTTPStatusError{Code: 429}), false},
		{fmt.Errorf("pull: %w", &HTTPStatusError{Code: 502}), false},
		{errors.New("connection reset"), false},
		{context.DeadlineExceeded, false},
	}
	for _, c := range cases {
		if got := PermanentArtifactError(c.err); got != c.want {
			t.Errorf("PermanentArtifactError(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// TestReconcileArtifacts_durablePins: gc leaves an artifact a durable pin
// holds, and one whose pin cannot be read.
func TestReconcileArtifacts_durablePins(t *testing.T) {
	cases := []struct {
		name      string
		arrange   func(d *fakeDurableArtifacts)
		wantAlive bool
	}{
		{"unpinned is collected", func(*fakeDurableArtifacts) {}, false},
		{"pinned survives", func(d *fakeDurableArtifacts) {
			_ = d.PinArtifacts(context.Background(), "move:inv", time.Hour, "art-1")
		}, true},
		{"unreadable pin survives", func(d *fakeDurableArtifacts) { d.pinErr = errors.New("restate down") }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			fm := newInlineFleet(FleetConfig{Provider: newStubProvider(), Prefix: "fuse-", ArtifactIdleTTL: time.Minute})
			d := newFakeDurableArtifacts()
			fm.SetDurableArtifacts(d)
			record := seedArtifactRecord(t, fm, "art-1", "tenant-1", "h-a", "d1")
			stale := time.Now().Add(-24 * time.Hour)
			record.CreatedAt, record.UpdatedAt = stale, stale
			if err := fm.upsertSnapshotRecord(ctx, record); err != nil {
				t.Fatal(err)
			}
			tc.arrange(d)
			fm.reconcileArtifacts(ctx)
			_, err := fm.GetSnapshotByID(ctx, "art-1")
			if alive := err == nil; alive != tc.wantAlive {
				t.Fatalf("alive = %v, want %v", alive, tc.wantAlive)
			}
		})
	}
}

// moveMetrics records ArtifactMoveCompleted.
type moveMetrics struct {
	got []string
}

func (*moveMetrics) ReconcileCompleted(ReconcileSummary) {}

func (m *moveMetrics) ArtifactMoveCompleted(src, dst string, bytes int64, _ time.Duration, err error) {
	m.got = append(m.got, fmt.Sprintf("%s->%s %d %v", src, dst, bytes, err != nil))
}

func TestArtifactPull_metrics(t *testing.T) {
	ctx := context.Background()
	mover := &recordingMover{}
	fm, _ := twoHostFleet(t, mover)
	m := &moveMetrics{}
	fm.metrics = m
	job := ArtifactMoveJob{SnapshotID: "art-1", Digest: "d1", SourceHostID: "h-a", HostID: "h-b", LocalID: "c"}
	if _, err := fm.ArtifactPull(ctx, job); err != nil {
		t.Fatal(err)
	}
	mover.err = errors.New("peer reset")
	_, _ = fm.ArtifactPull(ctx, job)
	if fmt.Sprint(m.got) != "[h-a->h-b 4096 false h-a->h-b 0 true]" {
		t.Fatalf("metrics %v", m.got)
	}
}

// flakySnapshotStore fails every snapshot read.
type flakySnapshotStore struct{ *MemoryStateStore }

func (flakySnapshotStore) GetSnapshot(context.Context, string) (SnapshotRecord, error) {
	return SnapshotRecord{}, errors.New("connection refused")
}

// TestArtifactDiscard_unreadableRecordKeepsBytes: a record that cannot be
// read may exist, so the copy is not deleted and the step fails to retry.
func TestArtifactDiscard_unreadableRecordKeepsBytes(t *testing.T) {
	ctx := context.Background()
	stub := &deletingProvider{stubLoadProvider: newStubProvider()}
	fm := newInlineFleet(FleetConfig{Provider: stub, Prefix: "fuse-", StateStore: flakySnapshotStore{NewMemoryStateStore()}})
	if err := fm.RegisterHost(ctx, artifactHost("h-b", 8), stub); err != nil {
		t.Fatal(err)
	}
	if err := fm.ArtifactDiscard(ctx, ArtifactMoveJob{HostID: "h-b", LocalID: "art-x"}); err == nil {
		t.Fatal("discard succeeded with the record unreadable")
	}
	if len(stub.deleted) != 0 {
		t.Fatalf("deleted %v with the record unreadable", stub.deleted)
	}
	if _, err := fm.ArtifactMoveCommitted(ctx, ArtifactMoveJob{HostID: "h-b", LocalID: "art-x"}); err == nil {
		t.Fatal("commit check hid a failed read")
	}
}

// TestEnsureArtifactOnHost_unsettledIsDistinct: a durable move the caller
// stopped waiting on stays recognisable, so cleanup can leave its files.
func TestEnsureArtifactOnHost_unsettledIsDistinct(t *testing.T) {
	ctx := context.Background()
	fm, _ := twoHostFleet(t, &recordingMover{})
	d := newFakeDurableArtifacts()
	d.moveErr = fmt.Errorf("%w: context deadline exceeded", ErrArtifactMoveUnsettled)
	fm.SetDurableArtifacts(d)
	_, err := fm.ensureArtifactOnHost(ctx, seedArtifactRecord(t, fm, "art-1", "tenant-1", "h-a", "d1"), "h-b", false)
	if !errors.Is(err, ErrArtifactMoveUnsettled) || !errors.Is(err, ErrArtifactTransferFailed) {
		t.Fatalf("err %v, want it to stay unsettled", err)
	}
}
