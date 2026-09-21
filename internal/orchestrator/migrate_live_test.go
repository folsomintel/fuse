package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
)

var liveTestFiles = map[string]string{"vmstate": "d-vmstate", "mem": "d-mem", "live.json": "d-manifest"}

// liveMigrateEnv takes live checkpoints that report per-file digests, which is
// what makes them eligible to move between hosts.
type liveMigrateEnv struct {
	digestForkEnv
	files map[string]string
}

func (e *liveMigrateEnv) CheckpointLive(ctx context.Context, comment string) (Checkpoint, error) {
	cp, err := e.CheckpointWithDigest(ctx, comment)
	cp.Kind = SnapshotKindLive
	cp.Files = e.files
	return cp, err
}

// liveMigrateProvider records the specs it was asked to create, and can refuse
// a resume the way a target host agent does.
type liveMigrateProvider struct {
	*digestForkProvider
	files        map[string]string
	refuseResume bool
	created      []Spec
}

func newLiveMigrateProvider() *liveMigrateProvider {
	return &liveMigrateProvider{digestForkProvider: newDigestForkProvider(), files: liveTestFiles}
}

func (p *liveMigrateProvider) Create(_ context.Context, spec Spec) (Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if spec.ResumeSeed && p.refuseResume {
		return nil, &HTTPStatusError{Code: http.StatusConflict, Body: "network slot 3 is in use on this host"}
	}
	p.created = append(p.created, spec)
	env := &liveMigrateEnv{
		digestForkEnv: digestForkEnv{snapshotTestEnv{name: spec.Name, url: "http://" + spec.Name + ".test"}},
		files:         p.files,
	}
	p.envs[spec.Name] = &env.digestForkEnv
	return env, nil
}

func newLiveMigrateFleet(t *testing.T, provider *liveMigrateProvider, mover *recordingMover) *FleetManager {
	t.Helper()
	ctx := context.Background()
	fm := NewFleetManager(FleetConfig{Provider: provider, Prefix: "fuse-", ArtifactMover: mover})
	for _, id := range []string{"h-a", "h-b"} {
		if err := fm.RegisterHost(ctx, artifactHost(id, 8), provider); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fm.ProvisionAndAssign(ctx, "task-src",
		Spec{CPUs: 1, RamMB: 512, HostID: "h-a"}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatalf("provision source: %v", err)
	}
	return fm
}

func TestMigrateVM_liveMovesTheMemoryImageAndResumes(t *testing.T) {
	provider := newLiveMigrateProvider()
	mover := &recordingMover{}
	fm := newLiveMigrateFleet(t, provider, mover)

	newID, err := fm.MigrateVM(context.Background(), "fuse-task-src", MigrateOptions{TargetHostID: "h-b", Live: true})
	if err != nil {
		t.Fatalf("live migrate: %v", err)
	}

	moves := mover.calls()
	if len(moves) != 1 {
		t.Fatalf("%d artifact copies, want 1", len(moves))
	}
	if !reflect.DeepEqual(moves[0].Files, liveTestFiles) {
		t.Errorf("moved files %v, want the digests the snapshot recorded %v", moves[0].Files, liveTestFiles)
	}
	info, _ := fm.GetVM(newID)
	if info.HostID != "h-b" || !info.Spec.ResumeSeed || info.Spec.SeedSnapshotID != moves[0].SnapshotID {
		t.Errorf("migrated vm = host %q resume %v seed %q, want a resume on h-b from %q",
			info.HostID, info.Spec.ResumeSeed, info.Spec.SeedSnapshotID, moves[0].SnapshotID)
	}
	// the copy has to be usable as the source of the next move.
	replica, err := fm.GetSnapshotByID(context.Background(), moves[0].SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if replica.Kind != SnapshotKindLive || !reflect.DeepEqual(liveFiles(replica), liveTestFiles) {
		t.Errorf("replica kind %q files %v, want a live record carrying the digests", replica.Kind, liveFiles(replica))
	}
	if _, ok := fm.GetVM("fuse-task-src"); ok {
		t.Error("source vm was not destroyed after the migration")
	}
}

func TestMigrateVM_coldMigrateIsUnchanged(t *testing.T) {
	provider := newLiveMigrateProvider()
	mover := &recordingMover{}
	fm := newLiveMigrateFleet(t, provider, mover)

	newID, err := fm.MigrateVM(context.Background(), "fuse-task-src", MigrateOptions{TargetHostID: "h-b"})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if moves := mover.calls(); len(moves) != 1 || len(moves[0].Files) != 0 {
		t.Errorf("moves = %+v, want one rootfs-only copy", moves)
	}
	if info, _ := fm.GetVM(newID); info.Spec.ResumeSeed {
		t.Error("a cold migrate asked the target to resume")
	}
}

// every refusal has to leave the source running: it is the only copy of the
// guest until the target has actually resumed it.
func TestMigrateVM_liveRefusalsLeaveTheSourceRunning(t *testing.T) {
	cases := []struct {
		name    string
		opts    MigrateOptions
		arrange func(*liveMigrateProvider, *recordingMover)
		want    error
	}{
		{name: "no target host", opts: MigrateOptions{Live: true}, want: ErrLiveMigrateRefused},
		{name: "target is the source host", opts: MigrateOptions{TargetHostID: "h-a", Live: true}, want: ErrLiveMigrateRefused},
		{
			name:    "target refuses the resume",
			opts:    MigrateOptions{TargetHostID: "h-b", Live: true},
			arrange: func(p *liveMigrateProvider, _ *recordingMover) { p.refuseResume = true },
			want:    ErrLiveMigrateRefused,
		},
		{
			name:    "source agent recorded no memory digests",
			opts:    MigrateOptions{TargetHostID: "h-b", Live: true},
			arrange: func(p *liveMigrateProvider, _ *recordingMover) { p.files = nil },
			want:    ErrArtifactImmovable,
		},
		{
			name:    "target agent committed the rootfs alone",
			opts:    MigrateOptions{TargetHostID: "h-b", Live: true},
			arrange: func(_ *liveMigrateProvider, m *recordingMover) { m.dropFiles = true },
			want:    ErrArtifactTransferFailed,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := newLiveMigrateProvider()
			mover := &recordingMover{}
			if tc.arrange != nil {
				tc.arrange(provider, mover)
			}
			fm := newLiveMigrateFleet(t, provider, mover)

			_, err := fm.MigrateVM(context.Background(), "fuse-task-src", tc.opts)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if info, ok := fm.GetVM("fuse-task-src"); !ok || info.State != VMStateRunning {
				t.Errorf("source vm = %+v (tracked %v), want it still running", info.State, ok)
			}
			if vms := fm.ListFleet(); len(vms) != 1 {
				t.Errorf("%d vms tracked, want only the source", len(vms))
			}
			if h, _ := fm.GetHost("h-b"); h.Allocated.VMCount != 0 {
				t.Errorf("h-b allocated = %+v, want nothing charged for a migrate that did not happen", h.Allocated)
			}
			if tc.want == ErrArtifactTransferFailed {
				if holders, _ := fm.HostsHoldingArtifact(context.Background(), "", "fork-digest"); len(holders) > 1 {
					t.Errorf("holders = %v, want no record of the half-copied snapshot", holders)
				}
			}
		})
	}
}
