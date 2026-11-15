package durable

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/orchestrator"
)

// migFleet scripts the migrate steps over snapFleet's snapshot steps. every
// step records its name; the destroy steps record which vm they destroy.
type migFleet struct {
	*snapFleet
	plan   orchestrator.MigratePlan
	prefix string

	mmu      sync.Mutex
	poll     orchestrator.MemoryPoll
	frozen   []string
	settled  []orchestrator.MemoryPoll
	deltaJob orchestrator.ArtifactMoveJob
}

// newMigFleet migrates src to h2 under a fresh new vm id.
func newMigFleet(src string, live, delta, lazy bool) *migFleet {
	id := fmt.Sprintf("migrate-%d", time.Now().UnixNano())
	return &migFleet{
		snapFleet: newSnapFleet(),
		prefix:    id,
		plan: orchestrator.MigratePlan{
			SourceVMID: src, SourceHostID: "h1", TargetHostID: "h2",
			MigrationID: id, NewVMID: "fuse-" + id,
			Live: live, Delta: delta, Lazy: lazy,
		},
	}
}

// restarted is a fresh fleet for the same migrate, as after a restart.
func (f *migFleet) restarted() *migFleet {
	g := newMigFleet(f.plan.SourceVMID, f.plan.Live, f.plan.Delta, f.plan.Lazy)
	g.plan, g.prefix = f.plan, f.prefix
	return g
}

func (f *migFleet) role(vm string) string {
	if vm == f.plan.SourceVMID {
		return "src"
	}
	return "target"
}

// SnapshotCheckpoint names each snapshot after the migrate, so pins and moves
// on one never meet another test's.
func (f *migFleet) SnapshotCheckpoint(ctx context.Context, vm string, plan orchestrator.SnapshotPlan) (orchestrator.Checkpoint, error) {
	cp, err := f.snapFleet.SnapshotCheckpoint(ctx, vm, plan)
	cp.ID = f.prefix + "-" + cp.ID
	return cp, err
}

// SnapshotRecordCreating keeps the checkpoint's kind, as the fleet's does.
func (f *migFleet) SnapshotRecordCreating(ctx context.Context, vm string, plan orchestrator.SnapshotPlan, cp orchestrator.Checkpoint) (orchestrator.SnapshotRecord, error) {
	rec, err := f.snapFleet.SnapshotRecordCreating(ctx, vm, plan, cp)
	rec.Kind = cp.Kind
	return rec, err
}

func (f *migFleet) DestroyBegin(ctx context.Context, vm, _ string) (orchestrator.DestroyTarget, error) {
	return orchestrator.DestroyTarget{Tracked: true, HostID: "h"}, f.enter(ctx, "destroy_begin:"+f.role(vm))
}

func (f *migFleet) DestroyOnHost(ctx context.Context, vm, _ string) error {
	return f.enter(ctx, "destroy_host:"+f.role(vm))
}

func (f *migFleet) DestroyFinish(ctx context.Context, vm, _ string) error {
	return f.enter(ctx, "destroy_finish:"+f.role(vm))
}

func (f *migFleet) MigrateBegin(ctx context.Context, _ string, _ orchestrator.MigrateRequest) (orchestrator.MigratePlan, error) {
	return f.plan, f.enter(ctx, "migrate_plan")
}

func (f *migFleet) MigratePlaceholder(ctx context.Context, _ orchestrator.MigratePlan) error {
	return f.enter(ctx, "placeholder")
}

func (f *migFleet) MigrateTransferPlan(ctx context.Context, plan orchestrator.MigratePlan, seed orchestrator.SnapshotRecord) (orchestrator.ChainMove, error) {
	job := orchestrator.ArtifactMoveJob{
		SnapshotID: seed.SnapshotID, SourceSnapshotID: seed.SnapshotID, SourceHostID: plan.SourceHostID,
		HostID: plan.TargetHostID, LocalID: seed.SnapshotID + "-on-" + plan.TargetHostID,
	}
	return orchestrator.ChainMove{Job: job}, f.enter(ctx, "plan_transfer")
}

func (f *migFleet) MigrateDeltaPlan(ctx context.Context, plan orchestrator.MigratePlan, child orchestrator.SnapshotRecord, base string) (orchestrator.ArtifactMoveJob, error) {
	job := orchestrator.ArtifactMoveJob{
		SnapshotID: child.SnapshotID, SourceSnapshotID: child.SnapshotID, SourceHostID: plan.SourceHostID,
		HostID: plan.TargetHostID, LocalID: child.SnapshotID + "-on-" + plan.TargetHostID, Base: base, DropBase: true,
	}
	f.mmu.Lock()
	f.deltaJob = job
	f.mmu.Unlock()
	return job, f.enter(ctx, "plan_delta")
}

func (f *migFleet) MigrateCreate(ctx context.Context, _ orchestrator.MigratePlan, _ orchestrator.SnapshotRecord, _ string) error {
	return f.enter(ctx, "create")
}

func (f *migFleet) MigrateIngress(ctx context.Context, _ orchestrator.MigratePlan) ([]byte, error) {
	return []byte("sealed-ingress"), f.enter(ctx, "ingress")
}

func (f *migFleet) MigrateCredentials(ctx context.Context, _ orchestrator.MigratePlan, ingress []byte) ([]byte, error) {
	if string(ingress) != "sealed-ingress" {
		return nil, fmt.Errorf("credentials got ingress %q", ingress)
	}
	return []byte("sealed-token"), f.enter(ctx, "credentials")
}

func (f *migFleet) MigrateStartAgent(ctx context.Context, _ orchestrator.MigratePlan, started orchestrator.MigrateStarted) error {
	if string(started.TokenEncrypted) != "sealed-token" {
		return fmt.Errorf("start agent got token %q", started.TokenEncrypted)
	}
	return f.enter(ctx, "start_agent")
}

func (f *migFleet) MigratePersist(ctx context.Context, _ orchestrator.MigratePlan, _ orchestrator.MigrateStarted) error {
	return f.enter(ctx, "persist")
}

func (f *migFleet) MigrateTask(ctx context.Context, _ orchestrator.MigratePlan) error {
	return f.enter(ctx, "task")
}

func (f *migFleet) MigrateCutover(ctx context.Context, _ orchestrator.MigratePlan, _ orchestrator.MigrateStarted) error {
	return f.enter(ctx, "cutover")
}

func (f *migFleet) MigrateLineage(ctx context.Context, _ orchestrator.MigratePlan, _ orchestrator.SnapshotRecord, _ bool) error {
	return f.enter(ctx, "lineage")
}

func (f *migFleet) MigrateHandover(ctx context.Context, _ orchestrator.MigratePlan) error {
	return f.enter(ctx, "handover")
}

func (f *migFleet) MigrateDrainSource(ctx context.Context, _ orchestrator.MigratePlan) error {
	return f.enter(ctx, "drain")
}

func (f *migFleet) MigrateResumeSource(ctx context.Context, _ orchestrator.MigratePlan) error {
	return f.enter(ctx, "resume_source")
}

func (f *migFleet) MigrateFrozen(ctx context.Context, _ orchestrator.MigratePlan, d time.Duration, outcome string) error {
	f.mmu.Lock()
	f.frozen = append(f.frozen, outcome)
	f.mmu.Unlock()
	if d < 0 {
		return fmt.Errorf("frozen for %v", d)
	}
	return f.enter(ctx, "frozen")
}

func (f *migFleet) MigrateDeleteSeed(ctx context.Context, _ orchestrator.SnapshotRecord) error {
	return f.enter(ctx, "delete_seed")
}

func (f *migFleet) MigrateMemoryPoll(ctx context.Context, _ string) (orchestrator.MemoryPoll, error) {
	f.mmu.Lock()
	poll := f.poll
	f.mmu.Unlock()
	return poll, f.enter(ctx, "memory_status")
}

func (f *migFleet) MigrateMemorySettled(ctx context.Context, _, _ string, poll orchestrator.MemoryPoll, _ bool) error {
	f.mmu.Lock()
	f.settled = append(f.settled, poll)
	f.mmu.Unlock()
	return f.enter(ctx, "memory_settled")
}

func (f *migFleet) outcomes() ([]string, []orchestrator.MemoryPoll) {
	f.mmu.Lock()
	defer f.mmu.Unlock()
	return append([]string(nil), f.frozen...), append([]orchestrator.MemoryPoll(nil), f.settled...)
}

// seedSteps are the snapshot steps of the seed or the delta.
const seedSteps = "begin,checkpoint,record,list,ready"

// sourceDestroy is the destroy of the source at the end of a migrate.
const sourceDestroy = "destroy_begin:src,destroy_host:src,destroy_finish:src"

// TestMigrateThroughRestate: a live migrate runs every step once, a create
// that fails twice is retried inside its step, and the source is drained and
// destroyed through the destroy steps.
func TestMigrateThroughRestate(t *testing.T) {
	src := uniqueVM("migrate")
	fleet := newMigFleet(src, true, false, false)
	fleet.fail["create"] = []error{errors.New("agent 503"), errors.New("agent 503")}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id, err := client.Migrate(ctx, src, orchestrator.MigrateRequest{Live: true})
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if id != fleet.plan.NewVMID {
		t.Fatalf("migrate returned %s, want %s", id, fleet.plan.NewVMID)
	}
	want := "migrate_plan,placeholder," + seedSteps + ",plan_transfer,create,create,create," +
		"ingress,credentials,start_agent,persist,task,cutover,lineage,handover,drain," + sourceDestroy
	if got := fleet.steps(); got != want {
		t.Fatalf("steps\n %s\nwant\n %s", got, want)
	}
	if frozen, _ := fleet.outcomes(); len(frozen) != 0 {
		t.Fatalf("frozen %v for a migrate that never paused its source", frozen)
	}
}

// TestMigrateRefusalCompensates: a target that refuses the resume fails the
// migrate with ErrLiveMigrateRefused, after the target placeholder is
// destroyed through its own Environment object and the seed is deleted. the
// source is never touched.
func TestMigrateRefusalCompensates(t *testing.T) {
	src := uniqueVM("migrate-refused")
	fleet := newMigFleet(src, true, false, false)
	fleet.fail["create"] = []error{fmt.Errorf("%w: host h2 cannot resume", orchestrator.ErrLiveMigrateRefused)}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := client.Migrate(ctx, src, orchestrator.MigrateRequest{Live: true})
	if !errors.Is(err, orchestrator.ErrLiveMigrateRefused) {
		t.Fatalf("migrate err = %v, want ErrLiveMigrateRefused", err)
	}
	want := "migrate_plan,placeholder," + seedSteps + ",plan_transfer,create," +
		"destroy_begin:target,destroy_host:target,destroy_finish:target,delete_seed"
	if got := fleet.steps(); got != want {
		t.Fatalf("steps\n %s\nwant\n %s", got, want)
	}
}

// TestMigratePlanRefusalKeepsItsSentinel: a refusal at the plan maps back to
// the fleet's sentinel, and nothing is created or undone.
func TestMigratePlanRefusalKeepsItsSentinel(t *testing.T) {
	src := uniqueVM("migrate-gpu")
	fleet := newMigFleet(src, false, false, false)
	fleet.fail["migrate_plan"] = []error{fmt.Errorf("%w: vm has a gpu", orchestrator.ErrGPUUnsupported)}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.Migrate(ctx, src, orchestrator.MigrateRequest{}); !errors.Is(err, orchestrator.ErrGPUUnsupported) {
		t.Fatalf("migrate err = %v, want ErrGPUUnsupported", err)
	}
	if got := fleet.steps(); got != "migrate_plan" {
		t.Fatalf("steps %s, want only the plan", got)
	}
}

// TestMigrateDeltaFailureResumesTheSource: a delta migrate whose persist
// fails for good destroys the target first, then resumes the source the diff
// paused, records the pause as rolled back, and deletes the seed.
func TestMigrateDeltaFailureResumesTheSource(t *testing.T) {
	src := uniqueVM("migrate-delta-fail")
	fleet := newMigFleet(src, true, true, false)
	fleet.fail["persist"] = []error{errAlways}
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.Migrate(ctx, src, orchestrator.MigrateRequest{Live: true}); err == nil {
		t.Fatal("migrate succeeded with a persist that always fails")
	}
	persists := strings.Repeat("persist,", fastPolicy.MaxAttempts)
	want := "migrate_plan,placeholder," + seedSteps + ",plan_transfer," + seedSteps + ",plan_delta,create," +
		"ingress,credentials,start_agent," + persists +
		"destroy_begin:target,destroy_host:target,destroy_finish:target,resume_source,frozen,delete_seed"
	if got := fleet.steps(); got != want {
		t.Fatalf("steps\n %s\nwant\n %s", got, want)
	}
	if frozen, _ := fleet.outcomes(); fmt.Sprint(frozen) != "[rolled_back]" {
		t.Fatalf("frozen %v, want one rolled_back", frozen)
	}
}

// TestMigrateSurvivesRestartWithTheSourcePaused: an endpoint killed while the
// target is being created, after the diff paused the source, comes back and
// finishes the migrate from the create on. the source is destroyed and never
// resumed, so exactly one copy of the guest runs.
func TestMigrateSurvivesRestartWithTheSourcePaused(t *testing.T) {
	src := uniqueVM("migrate-restart")
	first := newMigFleet(src, true, true, false)
	first.block["create"] = make(chan struct{})
	defer close(first.block["create"])
	client, addr, stop := serveFleetAt(t, first, "")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	type result struct {
		id  string
		err error
	}
	done := make(chan result, 1)
	go func() {
		id, err := client.Migrate(ctx, src, orchestrator.MigrateRequest{Live: true})
		done <- result{id, err}
	}()
	select {
	case <-first.entered:
	case <-ctx.Done():
		t.Fatal("create never ran")
	}
	if got, want := first.steps(), "migrate_plan,placeholder,"+seedSteps+",plan_transfer,"+seedSteps+",plan_delta,create"; got != want {
		t.Fatalf("steps before the restart\n %s\nwant\n %s", got, want)
	}

	stop()
	second := first.restarted()
	_, _, _ = serveFleetAt(t, second, addr)
	res := <-done
	if res.err != nil {
		t.Fatalf("migrate: %v", res.err)
	}
	if res.id != first.plan.NewVMID {
		t.Fatalf("migrate returned %s, want %s", res.id, first.plan.NewVMID)
	}
	want := "create,ingress,credentials,start_agent,persist,frozen,task,cutover,lineage,handover," + sourceDestroy
	if got := second.steps(); got != want {
		t.Fatalf("steps after the restart\n %s\nwant\n %s", got, want)
	}
	if frozen, _ := second.outcomes(); fmt.Sprint(frozen) != "[cutover]" {
		t.Fatalf("frozen %v, want one cutover", frozen)
	}
}

// TestMigrateDeltaOntoTheChainStandby: a delta migrate to the host that holds
// the checkpoint chain's base takes no seed and copies no bulk: it moves the
// diff onto that base, and forgets the chain, which the merge consumed,
// without deleting anything it held.
func TestMigrateDeltaOntoTheChainStandby(t *testing.T) {
	src := uniqueVM("migrate-chain")
	fleet := newMigFleet(src, true, true, false)
	client := serveFleet(t, fleet, fastPolicy)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := client.Checkpoint(ctx, src); err != nil {
		t.Fatal(err)
	}
	chain := fleet.waitSettled(t)
	if chain.Base == "" || chain.Standby != fleet.plan.TargetHostID {
		t.Fatalf("chain %+v, want a base on %s", chain, fleet.plan.TargetHostID)
	}
	fleet.mu.Lock()
	fleet.calls = nil
	fleet.mu.Unlock()

	if _, err := client.Migrate(ctx, src, orchestrator.MigrateRequest{Live: true}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	want := "migrate_plan,placeholder," + seedSteps + ",plan_delta,create," +
		"ingress,credentials,start_agent,persist,frozen,task,cutover,lineage,handover," + sourceDestroy
	if got := fleet.steps(); got != want {
		t.Fatalf("steps\n %s\nwant\n %s", got, want)
	}
	fleet.mmu.Lock()
	job := fleet.deltaJob
	fleet.mmu.Unlock()
	if job.Base != chain.Base || job.SnapshotID == chain.Head.SnapshotID {
		t.Fatalf("delta job %+v, want a new diff merged onto the chain's base %s", job, chain.Base)
	}
	if c, _ := fleet.mirrored(); !c.Empty() {
		t.Fatalf("chain %+v after the migrate, want it forgotten", c)
	}
	fleet.mu.Lock()
	retired := fleet.retired
	fleet.mu.Unlock()
	if len(retired) != 0 {
		t.Fatalf("retired %+v, want nothing deleted: the merge consumed the base", retired)
	}
}

// TestMigrateLazyWatchSurvivesRestart: a lazy migrate pins its seed and
// leaves Migrate/{migration_id}.WatchMemory polling; an endpoint killed
// mid-watch comes back, the watch resumes, settles once the memory is
// resident, and releases the pin.
func TestMigrateLazyWatchSurvivesRestart(t *testing.T) {
	prevFirst, prevMax := memoryPollFirst, memoryPollMax
	memoryPollFirst, memoryPollMax = 20*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { memoryPollFirst, memoryPollMax = prevFirst, prevMax })

	src := uniqueVM("migrate-lazy")
	first := newMigFleet(src, true, false, true)
	client, addr, stop := serveFleetAt(t, first, "")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := client.Migrate(ctx, src, orchestrator.MigrateRequest{Live: true, Lazy: true}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seed := first.prefix + "-snap-1"
	if held := pinState(t, ctx, client, []string{seed}); !held[0] {
		t.Fatal("the seed is not pinned while the target pages from it")
	}
	waitStep(t, ctx, first, "memory_status")

	stop()
	second := first.restarted()
	second.poll = orchestrator.MemoryPoll{Status: orchestrator.MemoryStatus{ResidentChunks: 8, TotalChunks: 8, Done: true}}
	_, _, _ = serveFleetAt(t, second, addr)
	waitStep(t, ctx, second, "memory_settled")
	waitUnpinned(t, ctx, client, []string{seed})
	if _, settled := second.outcomes(); len(settled) != 1 || !settled[0].Status.Done {
		t.Fatalf("settled %+v, want one resident outcome", settled)
	}
	if strings.Contains(first.steps(), "memory_settled") {
		t.Fatal("the watch settled before the memory was resident")
	}
}

// waitStep waits until fleet has run step.
func waitStep(t *testing.T, ctx context.Context, fleet *migFleet, step string) {
	t.Helper()
	for !strings.Contains(","+fleet.steps()+",", ","+step+",") {
		select {
		case <-ctx.Done():
			t.Fatalf("%s never ran; steps %s", step, fleet.steps())
		case <-time.After(20 * time.Millisecond):
		}
	}
}
