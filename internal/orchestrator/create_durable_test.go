package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/folsomintel/fuse/internal/egress"
	"github.com/folsomintel/fuse/internal/secrets"
)

var createTestKey = bytes.Repeat([]byte{9}, 32)

// createEnv is a guest that remembers what a create put in it.
type createEnv struct {
	*mockEnv
	mu       sync.Mutex
	files    map[string][]byte
	agents   int
	agentErr error
}

func (e *createEnv) Upload(_ context.Context, data []byte, path string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.files[path] = data
	return nil
}

func (e *createEnv) StartAgent(context.Context, AgentSpec) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.agents++
	return e.agentErr
}

func (e *createEnv) state() (files map[string][]byte, agents int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	files = make(map[string][]byte, len(e.files))
	for k, v := range e.files {
		files[k] = v
	}
	return files, e.agents
}

// createProvider is a host whose guests are createEnvs. it outlives a fleet,
// as a host outlives an orchestrator restart.
type createProvider struct {
	mu       sync.Mutex
	envs     map[string]*createEnv
	creates  int
	specs    []Spec
	execErr  error
	agentErr error
	destroys []string
}

func newCreateProvider() *createProvider {
	return &createProvider{envs: map[string]*createEnv{}}
}

func (p *createProvider) Create(_ context.Context, spec Spec) (Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.creates++
	p.specs = append(p.specs, spec)
	e := &createEnv{mockEnv: &mockEnv{name: spec.Name, url: "http://" + spec.Name + ".test", execErr: p.execErr}, files: map[string][]byte{}, agentErr: p.agentErr}
	p.envs[spec.Name] = e
	return e, nil
}

func (p *createProvider) Get(_ context.Context, name string) (Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.envs[name]
	if !ok {
		return nil, &HTTPStatusError{Code: 404, Body: "not found"}
	}
	return e, nil
}

func (p *createProvider) Destroy(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.destroys = append(p.destroys, name)
	delete(p.envs, name)
	return nil
}

func (p *createProvider) List(context.Context, string) ([]Environment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Environment, 0, len(p.envs))
	for _, e := range p.envs {
		out = append(out, e)
	}
	return out, nil
}

func (p *createProvider) Close() error { return nil }

func (p *createProvider) env(name string) *createEnv {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.envs[name]
}

// errCrash stops an inlineCreator after a step, as kill -9 would.
var errCrash = errors.New("orchestrator killed")

// inlineCreator stands in for restate: it runs the create steps in order on
// the fleet and, when one fails for good, the compensation, the way the
// Environment.Create handler does. crashAfter stops it after the named step
// with errCrash, keeping the state journaled so far in st for a resume.
type inlineCreator struct {
	fm         *FleetManager
	destroys   *inlineDestroyer
	crashAfter string

	mu       sync.Mutex
	req      CreateRequest
	st       CreateState
	done     []string
	inFlight bool
}

func (c *inlineCreator) CreateInFlight(context.Context, string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inFlight, nil
}

func (c *inlineCreator) Create(ctx context.Context, vmID string, req CreateRequest) error {
	c.mu.Lock()
	c.req = req
	c.mu.Unlock()
	return c.run(ctx, vmID, nil)
}

// resume runs the steps after the ones already done, on fm.
func (c *inlineCreator) resume(ctx context.Context, fm *FleetManager, vmID string) error {
	c.fm = fm
	c.crashAfter = ""
	c.mu.Lock()
	done := map[string]bool{}
	for _, s := range c.done {
		done[s] = true
	}
	c.mu.Unlock()
	return c.run(ctx, vmID, done)
}

func (c *inlineCreator) run(ctx context.Context, vmID string, done map[string]bool) error {
	fm, req := c.fm, c.req
	step := func(name string, fn func() error) error {
		if done[name] {
			return nil
		}
		if err := fn(); err != nil {
			return err
		}
		c.mu.Lock()
		c.done = append(c.done, name)
		c.mu.Unlock()
		if c.crashAfter == name {
			return errCrash
		}
		return nil
	}
	if err := step("begin", func() (err error) {
		c.st, err = fm.CreateBegin(ctx, vmID, req)
		return err
	}); err != nil {
		return err
	}
	steps := []struct {
		name string
		fn   func() error
	}{
		{"place", func() (err error) { c.st.HostID, err = fm.CreatePlace(ctx, vmID, req, c.st); return err }},
		{"artifact", func() (err error) { c.st.SeedSnapshotID, err = fm.CreateArtifact(ctx, vmID, req, c.st); return err }},
		{"ingress", func() (err error) { c.st.Ingress, err = fm.CreatePublishIngress(ctx, vmID, req, c.st); return err }},
		{"boot", func() (err error) {
			c.st.Booted = true
			c.st.FromCache, err = fm.CreateRestoreOrCreate(ctx, vmID, req, c.st)
			return err
		}},
		{"egress", func() error { return fm.CreateProvisionEgress(ctx, vmID, req, c.st) }},
		{"upload", func() error { return fm.CreateUpload(ctx, vmID, req, c.st) }},
		{"startup", func() error { return fm.CreateStartupScript(ctx, vmID, req, c.st) }},
		{"agent", func() (err error) { c.st.Endpoints, err = fm.CreateStartAgent(ctx, vmID, req, c.st); return err }},
		{"running", func() error { return fm.CreateRunning(ctx, vmID, req, c.st) }},
	}
	for _, s := range steps {
		err := step(s.name, s.fn)
		if err == nil {
			continue
		}
		if errors.Is(err, errCrash) || !PermanentCreateError(err) {
			return err
		}
		_ = fm.CreateReleaseEgress(ctx, vmID)
		_ = fm.CreateUnpublishIngress(ctx, vmID)
		if aerr := fm.CreateAbandon(ctx, vmID, req, err.Error()); aerr != nil {
			return aerr
		}
		if c.st.Booted {
			_ = c.destroys.SendDestroy(ctx, vmID, DestroyRequest{Reason: DestroyReasonCreateCleanup, HostID: c.st.HostID})
		}
		return err
	}
	return nil
}

// newCreateFleet is a fleet on restate, with one host, over store and p.
func newCreateFleet(t *testing.T, store StateStore, p *createProvider, cfg FleetConfig) (*FleetManager, *inlineCreator) {
	t.Helper()
	cfg.Provider = p
	cfg.StateStore = store
	cfg.Prefix = "fuse-"
	cfg.TokenEncryptionKey = createTestKey
	cfg.HostProviderFactory = func(string, string, HostBackend) Provider { return p }
	fm := NewFleetManager(cfg)
	d := &inlineDestroyer{fm: fm}
	fm.SetDurableDestroyer(d)
	c := &inlineCreator{fm: fm, destroys: d}
	fm.SetDurableCreator(c)
	if err := fm.recoverState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := fm.GetHost("h1"); !ok {
		if err := fm.RegisterHost(context.Background(), artifactHost("h1", 8), p); err != nil {
			t.Fatal(err)
		}
	}
	return fm, c
}

func TestDurableCreateRunsEveryStep(t *testing.T) {
	ctx := context.Background()
	p := newCreateProvider()
	fm, c := newCreateFleet(t, NewMemoryStateStore(), p, FleetConfig{})

	info, err := fm.ProvisionAndAssign(ctx, "t1", Spec{CPUs: 1, RamMB: 512}, []byte(`{"version":"1"}`),
		map[string]string{"API_KEY": "hunter2-secret"}, BootOptions{StartupScript: "true", Files: map[string][]byte{"/app/x": []byte("x")}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if info.State != VMStateRunning || info.HostID != "h1" {
		t.Fatalf("info %+v, want running on h1", info)
	}
	if got := strings.Join(c.done, ","); got != "begin,place,artifact,ingress,boot,egress,upload,startup,agent,running" {
		t.Fatalf("steps %s", got)
	}
	files, agents := p.env("fuse-t1").state()
	if agents != 1 || len(files[fuseManifestPath]) == 0 || string(files["/app/x"]) != "x" || !bytes.Contains(files[FuseSecretsPath], []byte("hunter2-secret")) {
		t.Fatalf("guest has files %v and %d agent starts", keys(files), agents)
	}
	tasks, _ := fm.store.ListTasks(ctx)
	if len(tasks) != 1 || tasks[0].RunStatus != TaskRunRunning {
		t.Fatalf("tasks %+v, want t1 running", tasks)
	}
	// what restate journals carries no plaintext secret or token.
	for name, b := range map[string][]byte{"request": c.req.Sealed, "credentials": c.st.Credentials} {
		if bytes.Contains(b, []byte("hunter2-secret")) {
			t.Fatalf("%s carries the secret in plaintext", name)
		}
	}
	if len(fm.createRuns) != 0 {
		t.Fatalf("finished create left %d runs behind", len(fm.createRuns))
	}
}

func keys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// lossyStore reads vms back the way postgres does: without the parts of a
// spec it has no column for.
type lossyStore struct{ *MemoryStateStore }

func (s lossyStore) ListVMs(ctx context.Context) ([]VMRecord, error) {
	records, err := s.MemoryStateStore.ListVMs(ctx)
	for i := range records {
		records[i].Spec.Name = ""
		records[i].Spec.Image = ""
		records[i].Spec.SeedSnapshotID = ""
		records[i].Spec.Egress.Secret = ""
	}
	return records, err
}

// TestDurableCreateResumesAfterRestart stands in for kill -9 of the
// orchestrator after the vm is created and before anything is uploaded: the
// restarted fleet keeps the vm provisioning and its task assigned, and the
// resumed create finishes it with its files and agent.
func TestDurableCreateResumesAfterRestart(t *testing.T) {
	ctx := context.Background()
	store := lossyStore{NewMemoryStateStore()}
	p := newCreateProvider()
	fm, c := newCreateFleet(t, store, p, FleetConfig{})
	c.crashAfter = "place"
	if _, err := fm.ProvisionAndAssign(ctx, "t2", Spec{CPUs: 1, RamMB: 512, Image: "img-x"}, []byte(`{"version":"1"}`),
		map[string]string{"K": "v"}, BootOptions{}); !errors.Is(err, errCrash) {
		t.Fatalf("create: %v, want the crash", err)
	}
	if p.env("fuse-t2") != nil {
		t.Fatal("vm exists on the host before the crash point")
	}

	fm2, _ := newCreateFleet(t, store, p, FleetConfig{})
	if v, ok := fm2.GetVM("fuse-t2"); !ok || v.State != VMStateProvisioning || v.HostID != "h1" {
		t.Fatalf("recovered vm %+v, want provisioning on h1", v)
	}
	if h, _ := fm2.GetHost("h1"); h.Allocated.CPUs != 1 {
		t.Fatalf("recovered host allocation %+v, want the vm's reservation", h.Allocated)
	}
	tasks, _ := store.ListTasks(ctx)
	if len(tasks) != 1 || tasks[0].RunStatus != TaskRunAssigned {
		t.Fatalf("tasks after restart %+v, want t2 still assigned", tasks)
	}

	if err := c.resume(ctx, fm2, "fuse-t2"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if v, _ := fm2.GetVM("fuse-t2"); v.State != VMStateRunning {
		t.Fatalf("vm after resume %+v, want running", v)
	}
	files, agents := p.env("fuse-t2").state()
	if agents != 1 || len(files[fuseManifestPath]) == 0 || len(files[FuseSecretsPath]) == 0 {
		t.Fatalf("guest after resume has files %v and %d agent starts", keys(files), agents)
	}
	if p.creates != 1 || p.specs[0].Name != "fuse-t2" || p.specs[0].Image != "img-x" {
		t.Fatalf("host saw creates %+v, want one of fuse-t2 from img-x", p.specs)
	}
	tasks, _ = store.ListTasks(ctx)
	if tasks[0].RunStatus != TaskRunRunning {
		t.Fatalf("task after resume %+v, want running", tasks[0])
	}
}

// a step run again after its effect (a crash before its journal entry) does
// not double the effect.
func TestDurableCreateStepsRunTwice(t *testing.T) {
	ctx := context.Background()
	p := newCreateProvider()
	fm, c := newCreateFleet(t, NewMemoryStateStore(), p, FleetConfig{})
	sealed := sealCreate(t, fm, createInputs{Spec: Spec{CPUs: 2, RamMB: 512}, Manifest: []byte(`{}`)})
	c.req = CreateRequest{TaskID: "t3", Sealed: sealed}

	for range 2 {
		st, err := fm.CreateBegin(ctx, "fuse-t3", c.req)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		c.st = st
	}
	for range 2 {
		hostID, err := fm.CreatePlace(ctx, "fuse-t3", c.req, c.st)
		if err != nil || hostID != "h1" {
			t.Fatalf("place: %q %v", hostID, err)
		}
		c.st.HostID = hostID
	}
	if h, _ := fm.GetHost("h1"); h.Allocated.CPUs != 2 {
		t.Fatalf("allocation %+v after two placements, want one reservation", h.Allocated)
	}
	for range 2 {
		if _, err := fm.CreateRestoreOrCreate(ctx, "fuse-t3", c.req, c.st); err != nil {
			t.Fatalf("boot: %v", err)
		}
	}
	if p.creates != 1 {
		t.Fatalf("host saw %d creates, want the second boot to keep the first vm", p.creates)
	}
	for _, s := range []func(context.Context, string, CreateRequest, CreateState) error{
		fm.CreateProvisionEgress, fm.CreateUpload, fm.CreateStartupScript, fm.CreateRunning, fm.CreateRunning,
	} {
		if err := s(ctx, "fuse-t3", c.req, c.st); err != nil {
			t.Fatal(err)
		}
	}
	if v, _ := fm.GetVM("fuse-t3"); v.State != VMStateRunning {
		t.Fatalf("vm %+v, want running", v)
	}
	// a create of the running vm's task now fails without touching it.
	if _, err := fm.CreateBegin(ctx, "fuse-t3", c.req); !errors.Is(err, ErrTaskAlreadyAssigned) {
		t.Fatalf("begin over a running vm: %v, want ErrTaskAlreadyAssigned", err)
	}
}

func sealCreate(t *testing.T, fm *FleetManager, in createInputs) []byte {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := secrets.EncryptBytes(raw, fm.tokenEncryptionKey)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

// TestDurableCreateStartupFailureRollsBack: a startup script that exits
// non-zero fails the create for good, and the rollback releases its egress,
// unpublishes its ingress, fails the task and sends the vm's destroy.
func TestDurableCreateStartupFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		spec Spec
	}{
		{"egress", Spec{CPUs: 1, RamMB: 512, Egress: egress.Spec{Mode: egress.ModeProxy, Provider: egress.MockName}}},
		{"ingress", Spec{CPUs: 1, RamMB: 512}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := newCreateProvider()
			p.execErr = errors.New("exec exit 3")
			mock := newEgressMock()
			proxy := newFakeIngress()
			fm, c := newCreateFleet(t, NewMemoryStateStore(), p, FleetConfig{EgressRegistry: egress.NewRegistry(mock), Ingress: proxy})

			_, err := fm.ProvisionAndAssign(ctx, "t4", tc.spec, []byte(`{}`), nil, BootOptions{StartupScript: "exit 3"})
			if err == nil || !strings.Contains(err.Error(), "exec exit 3") {
				t.Fatalf("create: %v, want the startup failure", err)
			}
			if got := strings.Join(c.done, ","); got != "begin,place,artifact,ingress,boot,egress,upload" {
				t.Fatalf("steps %s", got)
			}
			if active := mock.Active(); len(active) != 0 {
				t.Fatalf("egress left behind: %v", active)
			}
			if len(proxy.owners()) != 0 {
				t.Fatalf("ingress left behind: %v", proxy.owners())
			}
			if _, ok := fm.GetVM("fuse-t4"); ok {
				t.Fatal("failed create left its vm tracked")
			}
			if h, _ := fm.GetHost("h1"); h.Allocated.CPUs != 0 {
				t.Fatalf("failed create kept its reservation %+v", h.Allocated)
			}
			tasks, _ := fm.store.ListTasks(ctx)
			if len(tasks) != 1 || tasks[0].RunStatus != TaskRunFailed {
				t.Fatalf("tasks %+v, want t4 failed", tasks)
			}
			sent, _ := c.destroys.requests()
			if len(sent) != 1 || sent[0].Reason != DestroyReasonCreateCleanup || sent[0].HostID != "h1" {
				t.Fatalf("destroys sent %+v, want one create cleanup on h1", sent)
			}
			if p.env("fuse-t4") != nil {
				t.Fatal("the cleanup destroy left the vm on its host")
			}
		})
	}
}

// a placement that cannot succeed fails the create with its sentinel, and
// nothing was booted, so no destroy is sent.
func TestDurableCreateUnplaceable(t *testing.T) {
	ctx := context.Background()
	p := newCreateProvider()
	fm, c := newCreateFleet(t, NewMemoryStateStore(), p, FleetConfig{})
	_, err := fm.ProvisionAndAssign(ctx, "t5", Spec{CPUs: 64, RamMB: 512}, []byte(`{}`), nil, BootOptions{})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("create: %v, want ErrNoCapacity", err)
	}
	if sent, _ := c.destroys.requests(); len(sent) != 0 {
		t.Fatalf("destroys sent %+v for a vm that never booted", sent)
	}
	if _, ok := fm.GetVM("fuse-t5"); ok {
		t.Fatal("unplaceable create left its vm tracked")
	}
}

// without an encryption key the inputs cannot be sealed, so a create stays in
// process.
func TestDurableCreateNeedsKey(t *testing.T) {
	p := newCreateProvider()
	fm := NewFleetManager(FleetConfig{Provider: p, StateStore: NewMemoryStateStore(), Prefix: "fuse-"})
	c := &inlineCreator{fm: fm}
	fm.SetDurableCreator(c)
	if _, err := fm.ProvisionAndAssign(context.Background(), "t6", Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(c.done) != 0 {
		t.Fatalf("keyless create went through restate: %v", c.done)
	}
}

// a restored vm whose files do not upload fails its boot instead of running
// without them.
func TestRestoredBootFailsOnUploadError(t *testing.T) {
	p := &restoreProvider{env: &restoreEnv{mockEnv: &mockEnv{name: "fuse-r"}, uploadErr: errors.New("agent down")}}
	_, err := Boot(context.Background(), p, Spec{Name: "fuse-r"}, []byte(`{}`), nil, BootOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "agent down") {
		t.Fatalf("boot: %v, want the upload failure", err)
	}
	if !p.env.restored {
		t.Fatal("boot did not take the restore path")
	}
	p.env.uploadErr = nil
	p.env.agentErr = errors.New("agent will not start")
	if _, err := Boot(context.Background(), p, Spec{Name: "fuse-r"}, []byte(`{}`), nil, BootOptions{}, nil); err == nil {
		t.Fatal("boot of a restored vm whose agent does not start succeeded")
	}
}

type restoreEnv struct {
	*mockEnv
	restored  bool
	uploadErr error
	agentErr  error
}

func (e *restoreEnv) ListCheckpoints(context.Context) ([]Checkpoint, error) {
	return []Checkpoint{{ID: "cp-1"}}, nil
}
func (e *restoreEnv) Restore(context.Context, string) error {
	e.restored = true
	return nil
}
func (e *restoreEnv) Upload(context.Context, []byte, string) error { return e.uploadErr }
func (e *restoreEnv) StartAgent(context.Context, AgentSpec) error  { return e.agentErr }

type restoreProvider struct {
	mockProvider
	env *restoreEnv
}

func (p *restoreProvider) Get(context.Context, string) (Environment, error) { return p.env, nil }

// stepRecorder records in-process step durations.
type stepRecorder struct {
	mu    sync.Mutex
	steps []string
}

func (r *stepRecorder) ReconcileCompleted(ReconcileSummary) {}
func (r *stepRecorder) StepCompleted(op, step string, _ time.Duration, _ error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, op+"."+step)
}

func TestInProcessCreateTimesItsSteps(t *testing.T) {
	rec := &stepRecorder{}
	fm := NewFleetManager(FleetConfig{Provider: newMockProvider(), StateStore: NewMemoryStateStore(), Prefix: "fuse-", Metrics: rec})
	if _, err := fm.ProvisionAndAssign(context.Background(), "t7", Spec{}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rec.steps, ","); got != "create.boot,create.egress,create.upload,create.startup,create.agent" {
		t.Fatalf("steps %s", got)
	}
}

// a create on restate that reaches running arms its ttl timer, as the
// in-process create does.
func TestDurableCreateRunningArmsExpiry(t *testing.T) {
	p := newCreateProvider()
	fm, _ := newCreateFleet(t, NewMemoryStateStore(), p, FleetConfig{})
	d := withDurableExpiry(fm)
	if _, err := fm.ProvisionAndAssign(context.Background(), "t8", Spec{CPUs: 1, RamMB: 512}, []byte(`{}`), nil, BootOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	fm.expirySends.Wait()
	if got := d.armed(); len(got) != 1 || got[0] != "fuse-t8" {
		t.Fatalf("arms %v, want one for fuse-t8", got)
	}
}

// a provisioning vm no create will finish (one left by an in-process create
// before restate was on) is failed by reconcile and destroyed; one restate
// still holds a create for is left alone.
func TestStaleCreateIsAbandoned(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStateStore()
	p := newCreateProvider()
	fm, c := newCreateFleet(t, store, p, FleetConfig{})
	c.crashAfter = "boot"
	if _, err := fm.ProvisionAndAssign(ctx, "t9", Spec{CPUs: 1, RamMB: 512}, []byte(`{}`), nil, BootOptions{}); !errors.Is(err, errCrash) {
		t.Fatalf("create: %v, want the crash", err)
	}
	fm2, c2 := newCreateFleet(t, store, p, FleetConfig{})
	fm2.mu.Lock()
	fm2.vms["fuse-t9"].updatedAt = time.Now().Add(-time.Hour)
	fm2.mu.Unlock()

	c2.inFlight = true
	fm2.reconcileStaleCreates(ctx)
	if v, ok := fm2.GetVM("fuse-t9"); !ok || v.State != VMStateProvisioning {
		t.Fatalf("vm %+v with a create in flight, want it left provisioning", v)
	}

	c2.inFlight = false
	fm2.reconcileStaleCreates(ctx)
	if _, ok := fm2.GetVM("fuse-t9"); ok {
		t.Fatal("stale create left its vm tracked")
	}
	tasks, _ := store.ListTasks(ctx)
	if len(tasks) != 1 || tasks[0].RunStatus != TaskRunFailed {
		t.Fatalf("tasks %+v, want t9 failed", tasks)
	}
	if sent, _ := c2.destroys.requests(); len(sent) != 1 || sent[0].HostID != "h1" {
		t.Fatalf("destroys %+v, want one on h1", sent)
	}
	if h, _ := fm2.GetHost("h1"); h.Allocated.CPUs != 0 {
		t.Fatalf("stale create kept its reservation %+v", h.Allocated)
	}
}

// a begin that fails for good abandons a vm an earlier attempt recorded for
// its task, and leaves anything else under the id alone.
func TestCreateBeginFailed(t *testing.T) {
	ctx := context.Background()
	p := newCreateProvider()
	fm, c := newCreateFleet(t, NewMemoryStateStore(), p, FleetConfig{})
	c.req = CreateRequest{TaskID: "t10", Sealed: sealCreate(t, fm, createInputs{Spec: Spec{CPUs: 1}})}
	if _, err := fm.CreateBegin(ctx, "fuse-t10", c.req); err != nil {
		t.Fatal(err)
	}
	if err := fm.CreateBeginFailed(ctx, "fuse-t10", CreateRequest{TaskID: "other"}, "x"); err != nil {
		t.Fatal(err)
	}
	if _, ok := fm.GetVM("fuse-t10"); !ok {
		t.Fatal("begin_failed for another task dropped the vm")
	}
	if err := fm.CreateBeginFailed(ctx, "fuse-t10", c.req, "refused"); err != nil {
		t.Fatal(err)
	}
	if _, ok := fm.GetVM("fuse-t10"); ok {
		t.Fatal("begin_failed left its own provisioning vm")
	}
}

// a failing step's error is journaled, so it carries no gateway credential
// or auth token, which the host agent can echo back in its launch command.
func TestCreateErrorsRedactGatewayAndToken(t *testing.T) {
	ctx := context.Background()
	p := newCreateProvider()
	fm, _ := newCreateFleet(t, NewMemoryStateStore(), p, FleetConfig{})
	gateway := "wss://user:gw-password-123@gateway.test/ws"
	p.agentErr = fmt.Errorf("ssh timed out: fused --gateway %s --gateway-token tok-abcdefgh", gateway)
	_, err := fm.ProvisionAndAssign(ctx, "t11", Spec{CPUs: 1, RamMB: 512}, []byte(`{}`), nil,
		BootOptions{GatewayURL: gateway, GatewayToken: "tok-abcdefgh"})
	if err == nil {
		t.Fatal("create succeeded with an agent that fails")
	}
	for _, leak := range []string{gateway, "tok-abcdefgh"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error %q carries %q", err, leak)
		}
	}
}
