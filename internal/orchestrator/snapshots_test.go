package orchestrator

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"
)

type snapshotTestEnv struct {
	name string
	url  string

	files       map[string][]byte
	checkpoints []Checkpoint
}

func (e *snapshotTestEnv) Name() string  { return e.name }
func (e *snapshotTestEnv) URL() string   { return e.url }
func (e *snapshotTestEnv) Token() string { return "" }

func (e *snapshotTestEnv) Exec(context.Context, string, ...string) ([]byte, error) { return nil, nil }
func (e *snapshotTestEnv) ExecStream(context.Context, io.Writer, io.Writer, string, ...string) error {
	return nil
}
func (e *snapshotTestEnv) Upload(_ context.Context, data []byte, path string) error {
	if e.files == nil {
		e.files = make(map[string][]byte)
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	e.files[path] = cp
	return nil
}
func (e *snapshotTestEnv) StartAgent(context.Context, AgentSpec) error { return nil }
func (e *snapshotTestEnv) Checkpoint(_ context.Context, comment string) (string, error) {
	var sizeBytes int64
	for _, file := range e.files {
		sizeBytes += int64(len(file))
	}
	id := fmt.Sprintf("cp-%d", len(e.checkpoints)+1)
	e.checkpoints = append(e.checkpoints, Checkpoint{
		ID:        id,
		Comment:   comment,
		SizeBytes: sizeBytes,
		CreatedAt: time.Now(),
	})
	return id, nil
}
func (e *snapshotTestEnv) Restore(_ context.Context, checkpointID string) error {
	for _, checkpoint := range e.checkpoints {
		if checkpoint.ID == checkpointID {
			return nil
		}
	}
	return fmt.Errorf("checkpoint %s not found", checkpointID)
}
func (e *snapshotTestEnv) DeleteCheckpoint(_ context.Context, checkpointID string) error {
	for i, checkpoint := range e.checkpoints {
		if checkpoint.ID != checkpointID {
			continue
		}
		e.checkpoints = append(e.checkpoints[:i], e.checkpoints[i+1:]...)
		return nil
	}
	return fmt.Errorf("checkpoint %s not found", checkpointID)
}
func (e *snapshotTestEnv) ListCheckpoints(context.Context) ([]Checkpoint, error) {
	out := make([]Checkpoint, len(e.checkpoints))
	copy(out, e.checkpoints)
	return out, nil
}

type snapshotTestProvider struct {
	envs map[string]*snapshotTestEnv
}

func newSnapshotTestProvider() *snapshotTestProvider {
	return &snapshotTestProvider{envs: make(map[string]*snapshotTestEnv)}
}

func (p *snapshotTestProvider) Create(_ context.Context, spec Spec) (Environment, error) {
	env := &snapshotTestEnv{name: spec.Name, url: "http://" + spec.Name + ".test"}
	p.envs[spec.Name] = env
	return env, nil
}
func (p *snapshotTestProvider) Get(_ context.Context, name string) (Environment, error) {
	env, ok := p.envs[name]
	if !ok {
		return nil, fmt.Errorf("env %s not found", name)
	}
	return env, nil
}
func (p *snapshotTestProvider) Destroy(_ context.Context, name string) error {
	delete(p.envs, name)
	return nil
}
func (p *snapshotTestProvider) List(_ context.Context, _ string) ([]Environment, error) {
	out := make([]Environment, 0, len(p.envs))
	for _, env := range p.envs {
		out = append(out, env)
	}
	return out, nil
}
func (*snapshotTestProvider) Close() error { return nil }

func provisionSnapshotTestVM(t *testing.T, fm *FleetManager, taskID string) string {
	t.Helper()
	manifest := base64.StdEncoding.EncodeToString([]byte(`{"version":"1","services":{}}`))
	_, err := fm.ProvisionAndAssign(context.Background(), taskID, Spec{}, []byte(mustDecodeBase64(t, manifest)), nil, BootOptions{})
	if err != nil {
		t.Fatalf("provision vm: %v", err)
	}
	return "fuse-" + taskID
}

func mustDecodeBase64(t *testing.T, encoded string) []byte {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return raw
}

func TestCreateSnapshot_persistsLineageAndMetadata(t *testing.T) {
	provider := newSnapshotTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	first, err := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{Comment: "first"})
	if err != nil {
		t.Fatalf("create first snapshot: %v", err)
	}
	second, err := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{Comment: "second"})
	if err != nil {
		t.Fatalf("create second snapshot: %v", err)
	}

	if first.State != SnapshotStateReady {
		t.Fatalf("first state = %s, want ready", first.State)
	}
	if first.TaskID != "task-1" {
		t.Fatalf("first task_id = %q, want task-1", first.TaskID)
	}
	if first.TenantID != "task-1" {
		t.Fatalf("first tenant_id = %q, want task-1", first.TenantID)
	}
	if first.SizeBytes <= 0 {
		t.Fatalf("first size_bytes = %d, want > 0", first.SizeBytes)
	}
	if second.ParentSnapshotID != first.SnapshotID {
		t.Fatalf("second parent = %q, want %q", second.ParentSnapshotID, first.SnapshotID)
	}
}

func TestDeleteSnapshot_rejectsParentWithChildren(t *testing.T) {
	provider := newSnapshotTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	first, _ := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{})
	_, _ = fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{})

	err := fm.DeleteSnapshot(context.Background(), vmID, first.SnapshotID)
	if err == nil {
		t.Fatal("expected delete to fail for parent snapshot")
	}
	if !errors.Is(err, ErrSnapshotHasChildren) {
		t.Fatalf("err = %v, want ErrSnapshotHasChildren", err)
	}
}

func TestReconcileSnapshots_deletesExpiredLeaf(t *testing.T) {
	provider := newSnapshotTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	first, _ := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{})
	second, _ := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{})
	third, _ := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{})

	second.ParentSnapshotID = first.SnapshotID
	expired := time.Now().Add(-time.Hour)
	second.RetentionUntil = &expired
	if err := fm.upsertSnapshotRecord(context.Background(), second); err != nil {
		t.Fatalf("update second snapshot: %v", err)
	}

	third.ParentSnapshotID = first.SnapshotID
	if err := fm.upsertSnapshotRecord(context.Background(), third); err != nil {
		t.Fatalf("update third snapshot: %v", err)
	}

	fm.reconcileSnapshots(context.Background())

	if _, err := fm.GetSnapshot(context.Background(), vmID, second.SnapshotID); err == nil {
		t.Fatal("expected expired leaf snapshot to be deleted")
	}
	if _, err := fm.GetSnapshot(context.Background(), vmID, third.SnapshotID); err != nil {
		t.Fatalf("latest branch snapshot should remain: %v", err)
	}
}

func TestRestoreSnapshot_marksMissingProviderSnapshotError(t *testing.T) {
	provider := newSnapshotTestProvider()
	fm := NewFleetManager(FleetConfig{
		Provider: provider,
		Prefix:   "fuse-",
	})
	vmID := provisionSnapshotTestVM(t, fm, "task-1")

	snapshot, _ := fm.CreateSnapshot(context.Background(), vmID, SnapshotOptions{})
	env, err := provider.Get(context.Background(), vmID)
	if err != nil {
		t.Fatalf("get env: %v", err)
	}
	if deleter, ok := env.(SnapshotDeleter); ok {
		if err := deleter.DeleteCheckpoint(context.Background(), snapshot.SnapshotID); err != nil {
			t.Fatalf("delete checkpoint: %v", err)
		}
	} else {
		t.Fatal("test env does not implement SnapshotDeleter")
	}

	err = fm.RestoreSnapshot(context.Background(), vmID, snapshot.SnapshotID)
	if err == nil {
		t.Fatal("expected restore to fail for missing provider snapshot")
	}
	if !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("err = %v, want ErrSnapshotNotFound", err)
	}

	record, err := fm.GetSnapshot(context.Background(), vmID, snapshot.SnapshotID)
	if err != nil {
		t.Fatalf("reload snapshot: %v", err)
	}
	if record.State != SnapshotStateError {
		t.Fatalf("state = %s, want error", record.State)
	}
}
