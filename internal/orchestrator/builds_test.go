package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/fusefile"
)

// buildEnv is a builder guest: it records every script it is asked to exec,
// fails the one containing failOn, and takes checkpoints with distinct ids.
type buildEnv struct {
	name   string
	failOn string

	mu          sync.Mutex
	scripts     []string
	checkpoints []Checkpoint
}

func (e *buildEnv) Name() string  { return e.name }
func (e *buildEnv) URL() string   { return "http://" + e.name + ".test" }
func (e *buildEnv) Token() string { return "" }

func (e *buildEnv) Exec(_ context.Context, cmd []string, _ ExecOptions) (ExecResult, error) {
	script := cmd[len(cmd)-1]
	e.mu.Lock()
	e.scripts = append(e.scripts, script)
	e.mu.Unlock()
	if e.failOn != "" && strings.Contains(script, e.failOn) {
		return ExecResult{ExitCode: 2, Stdout: []byte("installing\n"), Stderr: []byte("token hunter2hunter2 rejected\n")}, nil
	}
	return ExecResult{}, nil
}
func (e *buildEnv) ExecStream(context.Context, io.Writer, io.Writer, string, ...string) error {
	return nil
}
func (e *buildEnv) Upload(context.Context, []byte, string) error { return nil }
func (e *buildEnv) StartAgent(context.Context, AgentSpec) error  { return nil }

func (e *buildEnv) Checkpoint(ctx context.Context, comment string) (string, error) {
	cp, err := e.CheckpointWithDigest(ctx, comment)
	return cp.ID, err
}

func (e *buildEnv) CheckpointWithDigest(_ context.Context, comment string) (Checkpoint, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := Checkpoint{ID: fmt.Sprintf("%s-cp-%d", e.name, len(e.checkpoints)), Comment: comment, SizeBytes: 4096}
	e.checkpoints = append(e.checkpoints, cp)
	return cp, nil
}

func (e *buildEnv) Restore(context.Context, string) error { return nil }

func (e *buildEnv) ListCheckpoints(context.Context) ([]Checkpoint, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Checkpoint(nil), e.checkpoints...), nil
}

func (e *buildEnv) execs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.scripts...)
}

// buildProvider hands out one buildEnv per vm and records destroys.
type buildProvider struct {
	failOn string

	mu        sync.Mutex
	envs      map[string]*buildEnv
	destroyed []string
}

func (p *buildProvider) Create(_ context.Context, spec Spec) (Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.envs == nil {
		p.envs = map[string]*buildEnv{}
	}
	e := &buildEnv{name: spec.Name, failOn: p.failOn}
	p.envs[spec.Name] = e
	return e, nil
}

func (p *buildProvider) Get(_ context.Context, name string) (Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.envs[name]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrVMNotFound, name)
	}
	return e, nil
}

func (p *buildProvider) Destroy(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.destroyed = append(p.destroyed, name)
	return nil
}

func (p *buildProvider) List(context.Context, string) ([]Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Environment, 0, len(p.envs))
	for _, e := range p.envs {
		out = append(out, e)
	}
	return out, nil
}

func (*buildProvider) Close() error { return nil }

func (p *buildProvider) env(name string) *buildEnv {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.envs[name]
}

func newBuildFleet(t *testing.T, p *buildProvider) *FleetManager {
	t.Helper()
	fm := newInlineFleet(FleetConfig{Provider: p, Prefix: "fuse-"})
	if err := fm.RegisterHost(context.Background(), layerHost("h1", "amd64"), p); err != nil {
		t.Fatal(err)
	}
	return fm
}

// testBuildInput is a three-step setup: two cacheable steps, then one that is
// not, so a build exercises both the per-layer execs and the tail.
func testBuildInput(id string, keys ...string) BuildInput {
	return BuildInput{
		ID:          id,
		Name:        "app",
		TenantID:    "tenant-a",
		Fingerprint: "fp-" + strings.Join(keys, ","),
		Spec:        Spec{CPUs: 1, RamMB: 256, Image: "ubuntu-24.04"},
		Manifest:    []byte(`{}`),
		Setup: fusefile.SetupPlan{
			Prelude: "set -eu\n",
			Files:   "echo files\n",
			Steps:   []string{"apt-get install -y git", "npm ci", "date > /stamp"},
		},
		Keys: keys,
	}
}

func waitBuild(t *testing.T, fm *FleetManager, id string) BuildRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, err := fm.GetBuild(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if rec.State != BuildRunning {
			return rec
		}
		if time.Now().After(deadline) {
			t.Fatalf("build %s still running at step %s", id, rec.Step)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func layerStatuses(rec BuildRecord) string {
	out := make([]string, len(rec.Layers))
	for i, l := range rec.Layers {
		out[i] = l.Status
	}
	return strings.Join(out, ",")
}

func TestPlanExecs(t *testing.T) {
	cases := []struct {
		name string
		keys []string
		hit  int
		want string
	}{
		// no cacheable step: one exec of the whole setup phase with the
		// file block, which is the build script byte for byte.
		{"cache off", nil, -1, "tail[0,3)+files"},
		{"cold", []string{"a", "b", ""}, -1, "layer_0[0,1)+files layer_1[1,2) tail[2,3)"},
		{"warm at the first layer", []string{"a", "b", ""}, 0, "layer_1[1,2) tail[2,3)"},
		{"warm at the deepest layer", []string{"a", "b", ""}, 1, "tail[2,3)"},
		{"every step cached and hit", []string{"a", "b", "c"}, 2, ""},
		{"first step uncacheable", []string{"", "b", "c"}, -1, "tail[0,3)+files"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, ex := range planExecs(tc.keys, 3, tc.hit) {
				s := fmt.Sprintf("%s[%d,%d)", ex.Step, ex.From, ex.To)
				if ex.Files {
					s += "+files"
				}
				got = append(got, s)
			}
			if strings.Join(got, " ") != tc.want {
				t.Errorf("execs = %q, want %q", strings.Join(got, " "), tc.want)
			}
		})
	}
}

// a cold build snapshots each cacheable layer, and a second build of the same
// plan seeds from the deepest one and runs only what is left: the hits a
// client-side `fuse build` got for the same keys.
func TestBuildInProcessColdThenWarm(t *testing.T) {
	ctx := context.Background()
	p := &buildProvider{}
	fm := newBuildFleet(t, p)

	in := testBuildInput("bld_cold", "k0", "k1", "")
	if _, replayed, err := fm.SubmitBuild(ctx, in, nil); err != nil || replayed {
		t.Fatalf("submit: replayed=%v err=%v", replayed, err)
	}
	cold := waitBuild(t, fm, in.ID)
	if cold.State != BuildSucceeded || cold.Step != BuildStepDone {
		t.Fatalf("cold build = %s at %s: %s", cold.State, cold.Step, cold.Error)
	}
	if got := layerStatuses(cold); got != "miss,miss,skip" || cold.CacheHits != 0 {
		t.Errorf("cold layers = %s with %d hits, want miss,miss,skip with 0", got, cold.CacheHits)
	}
	if cold.Arch != ArchAMD64 || cold.SnapshotID == "" || cold.Layers[1].SnapshotID == "" {
		t.Errorf("cold record = %+v", cold)
	}
	builder := p.env(cold.BuilderID)
	want := []string{
		in.Setup.Range(0, 1, true),
		in.Setup.Range(1, 2, false),
		in.Setup.Range(2, 3, false),
	}
	if fmt.Sprint(builder.execs()) != fmt.Sprint(want) {
		t.Errorf("cold execs = %q, want %q", builder.execs(), want)
	}
	if _, ok := fm.GetVM(cold.BuilderID); ok {
		t.Errorf("builder %s still tracked after the build", cold.BuilderID)
	}

	warmIn := testBuildInput("bld_warm", "k0", "k1", "")
	if _, _, err := fm.SubmitBuild(ctx, warmIn, nil); err != nil {
		t.Fatal(err)
	}
	warm := waitBuild(t, fm, warmIn.ID)
	if warm.State != BuildSucceeded {
		t.Fatalf("warm build failed: %s", warm.Error)
	}
	if got := layerStatuses(warm); got != "hit,hit,skip" || warm.CacheHits != 2 {
		t.Errorf("warm layers = %s with %d hits, want hit,hit,skip with 2", got, warm.CacheHits)
	}
	if warm.SeedSnapshotID != cold.Layers[1].SnapshotID {
		t.Errorf("warm seed = %s, want the deepest cold layer %s", warm.SeedSnapshotID, cold.Layers[1].SnapshotID)
	}
	if got := p.env(warm.BuilderID).execs(); fmt.Sprint(got) != fmt.Sprint([]string{in.Setup.Range(2, 3, false)}) {
		t.Errorf("warm execs = %q, want only the tail without the file block", got)
	}
}

// a layer that exits non-zero fails the build with its redacted output,
// destroys the builder, and keeps the layers before it for the next try.
func TestBuildFailedExecDestroysTheBuilder(t *testing.T) {
	ctx := context.Background()
	p := &buildProvider{failOn: "npm ci"}
	fm := newBuildFleet(t, p)

	in := testBuildInput("bld_fail", "k0", "k1", "")
	if _, _, err := fm.SubmitBuild(ctx, in, map[string]string{"NPM_TOKEN": "hunter2hunter2"}); err != nil {
		t.Fatal(err)
	}
	rec := waitBuild(t, fm, in.ID)
	if rec.State != BuildFailed || !strings.Contains(rec.Error, "setup[1] exited 2") {
		t.Fatalf("record = %s: %q", rec.State, rec.Error)
	}
	if strings.Contains(rec.Output, "hunter2hunter2") || !strings.Contains(rec.Output, "installing") {
		t.Errorf("output = %q, want it kept and the secret redacted", rec.Output)
	}
	if rec.Layers[0].SnapshotID == "" || rec.Layers[1].SnapshotID != "" {
		t.Errorf("layers = %+v, want layer 0 kept and layer 1 not taken", rec.Layers)
	}
	if rec.SnapshotID != "" {
		t.Errorf("a failed build has artifact %s", rec.SnapshotID)
	}
	if _, ok := fm.GetVM(rec.BuilderID); ok {
		t.Errorf("builder %s still tracked after the failure", rec.BuilderID)
	}
}

func TestBuildKeepLeavesTheBuilder(t *testing.T) {
	p := &buildProvider{}
	fm := newBuildFleet(t, p)
	in := testBuildInput("bld_keep")
	in.Keep = true
	if _, _, err := fm.SubmitBuild(context.Background(), in, nil); err != nil {
		t.Fatal(err)
	}
	rec := waitBuild(t, fm, in.ID)
	if rec.State != BuildSucceeded {
		t.Fatalf("build failed: %s", rec.Error)
	}
	if _, ok := fm.GetVM(rec.BuilderID); !ok {
		t.Errorf("builder %s destroyed despite keep", rec.BuilderID)
	}
}

// a build id is an Idempotency-Key: the same request gets the first build
// back, a different one is refused.
func TestSubmitBuildReplayAndConflict(t *testing.T) {
	ctx := context.Background()
	fm := newBuildFleet(t, &buildProvider{})
	in := testBuildInput("bld_key", "k0")
	if _, _, err := fm.SubmitBuild(ctx, in, nil); err != nil {
		t.Fatal(err)
	}
	first := waitBuild(t, fm, in.ID)

	again, replayed, err := fm.SubmitBuild(ctx, in, nil)
	if err != nil || !replayed || again.SnapshotID != first.SnapshotID {
		t.Fatalf("replay = %+v replayed=%v err=%v", again, replayed, err)
	}
	other := in
	other.Fingerprint = "something else"
	if _, _, err := fm.SubmitBuild(ctx, other, nil); !errors.Is(err, ErrBuildConflict) {
		t.Fatalf("reuse for a different request: err = %v, want ErrBuildConflict", err)
	}
}

// a build an older release ran in process and never finished is failed on
// its next read instead of reading as running forever.
func TestOrphanedInProcessBuildReadsFailed(t *testing.T) {
	ctx := context.Background()
	fm := newBuildFleet(t, &buildProvider{})
	in := testBuildInput("bld_orphan")
	if err := fm.store.(BuildStore).UpsertBuild(ctx, newBuildRecord(in, time.Now())); err != nil {
		t.Fatal(err)
	}
	rec, err := fm.GetBuild(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != BuildFailed || rec.FinishedAt == nil {
		t.Errorf("orphaned build = %s, want failed", rec.State)
	}
}

// the lookup walks the chain deepest first and stops at the first stored
// layer, the walk resolveDeepestLayer does client-side: with only the first
// layer stored the build seeds from it, and without an arch it does not look
// at all.
func TestBuildResolveCacheWalksDeepestFirst(t *testing.T) {
	ctx := context.Background()
	fm := newBuildFleet(t, &buildProvider{})
	if err := fm.store.UpsertSnapshot(ctx, SnapshotRecord{
		SnapshotID: "snap-k0", VMID: "fuse-old", HostID: "h1", TenantID: "tenant-a",
		State: SnapshotStateReady, Mode: SnapshotModeBuild, LayerKey: "k0", Arch: ArchAMD64,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := fm.BuildResolveCache(ctx, testBuildInput("bld_walk", "k0", "k1", ""))
	if err != nil {
		t.Fatal(err)
	}
	if plan.SeedSnapshotID != "snap-k0" || plan.SeedHostID != "h1" || len(plan.Execs) != 2 || plan.Execs[0].Step != "layer_1" {
		t.Errorf("plan = %+v, want seed snap-k0 then layer_1 and the tail", plan)
	}

	// another tenant's layer is never a hit.
	other := testBuildInput("bld_other", "k0", "k1", "")
	other.TenantID = "tenant-b"
	if plan, err := fm.BuildResolveCache(ctx, other); err != nil || plan.SeedSnapshotID != "" {
		t.Errorf("cross-tenant plan = %+v err=%v, want a miss", plan, err)
	}
}

func TestBuildArchNeedsOneAnswer(t *testing.T) {
	ctx := context.Background()
	p := &buildProvider{}
	fm := newBuildFleet(t, p)
	if got := fm.buildArch(Spec{}); got != ArchAMD64 {
		t.Errorf("one amd64 host: arch = %q", got)
	}
	if got := fm.buildArch(Spec{Arch: ArchARM64}); got != ArchARM64 {
		t.Errorf("spec arch: arch = %q", got)
	}
	if err := fm.RegisterHost(ctx, layerHost("h2", "aarch64"), p); err != nil {
		t.Fatal(err)
	}
	if got := fm.buildArch(Spec{}); got != "" {
		t.Errorf("mixed fleet: arch = %q, want none", got)
	}
	if got := fm.buildArch(Spec{HostID: "h2"}); got != ArchARM64 {
		t.Errorf("pinned host: arch = %q", got)
	}

	// no arch means no layers: the build runs as one exec and snapshots
	// nothing but its artifact.
	plan, err := fm.BuildResolveCache(ctx, testBuildInput("bld_mixed", "k0", "k1", ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Execs) != 1 || plan.Execs[0].Step != BuildStepTail || !plan.Execs[0].Files {
		t.Errorf("mixed fleet plan = %+v, want one exec of the whole setup", plan)
	}
}

// of many requests for one build id at once, exactly one runs the build and
// the rest get its record back.
func TestSubmitBuildClaimsTheIDOnce(t *testing.T) {
	ctx := context.Background()
	p := &buildProvider{}
	fm := newBuildFleet(t, p)
	in := testBuildInput("bld_race", "k0")

	var wg sync.WaitGroup
	var mu sync.Mutex
	started := 0
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, replayed, err := fm.SubmitBuild(ctx, in, nil)
			if err != nil {
				t.Error(err)
				return
			}
			if !replayed {
				mu.Lock()
				started++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if started != 1 {
		t.Fatalf("%d submits started the build, want 1", started)
	}
	if rec := waitBuild(t, fm, in.ID); rec.State != BuildSucceeded {
		t.Fatalf("build %s: %s", rec.State, rec.Error)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.envs) != 1 {
		t.Errorf("%d builders created, want 1", len(p.envs))
	}
}

// persistentBuildStore stands in for postgres, a store whose build records
// outlive the process.
type persistentBuildStore struct{ *MemoryStateStore }

// recordingBuilds is a restate that takes every build and counts the sends.
type recordingBuilds struct {
	mu   sync.Mutex
	sent []string
}

func (r *recordingBuilds) SubmitBuild(_ context.Context, in BuildInput) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, in.ID)
	return nil
}

// a submit that stopped between the record and the send leaves a running
// durable record nothing runs. a retry with the same key sends the workflow
// again, which restate starts once; a finished build is not sent again.
func TestSubmitBuildResendsAnInterruptedSubmit(t *testing.T) {
	ctx := context.Background()
	store := persistentBuildStore{NewMemoryStateStore()}
	fm := newInlineFleet(FleetConfig{Provider: &buildProvider{}, Prefix: "fuse-", StateStore: store})
	d := &recordingBuilds{}
	fm.SetDurableBuilds(d)

	in := testBuildInput("bld_lost", "k0")
	rec := newBuildRecord(in, time.Now())
	rec.Durable = true
	if _, _, err := store.InsertBuild(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, replayed, err := fm.SubmitBuild(ctx, in, nil)
	if err != nil || !replayed || got.State != BuildRunning {
		t.Fatalf("retry: %+v replayed=%v err=%v", got, replayed, err)
	}
	if fmt.Sprint(d.sent) != "[bld_lost]" {
		t.Fatalf("sends %v, want the workflow sent again", d.sent)
	}

	rec.State = BuildSucceeded
	if err := store.UpsertBuild(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fm.SubmitBuild(ctx, in, nil); err != nil {
		t.Fatal(err)
	}
	if len(d.sent) != 1 {
		t.Errorf("a finished build was sent again: %v", d.sent)
	}
}

// the builder's stream closes when the builder is destroyed, so its last
// build event already carries how the build ended.
func TestBuildEventsCarryTheOutcomeBeforeTheBuilderGoes(t *testing.T) {
	for _, tc := range []struct {
		name, failOn, want string
	}{
		{"succeeded", "", BuildSucceeded},
		{"failed", "npm ci", BuildFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fm := newBuildFleet(t, &buildProvider{failOn: tc.failOn})
			in := testBuildInput("bld_events_"+tc.name, "k0", "k1")
			events, stop := fm.SubscribeEnvironmentEvents("fuse-" + buildTaskID(in.ID))
			defer stop()
			if _, _, err := fm.SubmitBuild(context.Background(), in, nil); err != nil {
				t.Fatal(err)
			}
			deadline := time.After(5 * time.Second)
			for {
				select {
				case ev, ok := <-events:
					if !ok {
						t.Fatalf("stream closed without a %s build event", tc.want)
					}
					if ev.Build != nil && ev.Build.State == tc.want {
						return
					}
				case <-deadline:
					t.Fatalf("no %s build event", tc.want)
				}
			}
		})
	}
}

// a record that had to start again (gone from the store mid-build) still
// names the artifact once the build finishes: the id comes from the snapshot
// step, not the record.
func TestBuildFinishNamesTheArtifactOnARecreatedRecord(t *testing.T) {
	ctx := context.Background()
	fm := newInlineFleet(FleetConfig{Provider: &buildProvider{}, Prefix: "fuse-",
		StateStore: persistentBuildStore{NewMemoryStateStore()}})
	in := testBuildInput("bld_recreated", "k0")
	if err := fm.BuildFinish(ctx, in, "snap-final"); err != nil {
		t.Fatal(err)
	}
	rec, err := fm.GetBuild(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State != BuildSucceeded || rec.SnapshotID != "snap-final" {
		t.Fatalf("record %s with artifact %q, want succeeded with snap-final", rec.State, rec.SnapshotID)
	}
}
