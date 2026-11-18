// Package orchestratortest stands in for restate in tests: Inline runs each
// durable op's fleet steps in process, in the order the internal/durable
// handler does. import it only from tests.
package orchestratortest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/folsomintel/fuse/internal/hitl"
	"github.com/folsomintel/fuse/internal/orchestrator"
)

// TestKey is the TOKEN_ENCRYPTION_KEY NewFleet gives a fleet that has none,
// so creates and forks can seal their inputs.
const TestKey = "inline-durable-test-key-32-bytes"

// the waits the durable handlers take between attempts and polls, copied
// from internal/durable. vars so tests can shorten them.
var (
	webhookFirstBackoff = time.Second
	webhookMaxBackoff   = time.Hour
	webhookWindow       = 24 * time.Hour
	imagePollFirst      = 5 * time.Second
	imagePollMax        = 30 * time.Second
	memoryPollFirst     = 2 * time.Second
	memoryPollMax       = 30 * time.Second
)

// pendingLapse is how long a chain waits on a copy that never reports
// back before it forgets it, as in internal/durable.
const pendingLapse = 2 * time.Hour

// retrying marks an error restate would keep retrying instead of ending
// the invocation on: a step with no attempt bound, or one whose failure is
// not marked terminal. a handler returns it without running its
// compensation, the way it returns a suspension.
type retrying struct{ err error }

func (e *retrying) Error() string { return e.err.Error() }
func (e *retrying) Unwrap() error { return e.err }

// held is the failure of a step restate retries until it succeeds or
// the invocation gives up: terminal only when permanent says so.
func held(err error, permanent func(error) bool) error {
	err = bounded(err)
	if err == nil || (permanent != nil && permanent(err)) {
		return err
	}
	return &retrying{err: err}
}

// bounded is the failure of a step with an attempt bound: once the
// attempts run out it is terminal, whatever it was.
func bounded(err error) error {
	if r, ok := err.(*retrying); ok {
		return r.err
	}
	return err
}

// terminal reports whether err ends the invocation for good, which is
// what runs a handler's compensation.
func terminal(err error) bool {
	_, retrying := err.(*retrying)
	return err != nil && !retrying
}

// crossed is a handler's failure as the durable client hands it back: a
// terminal error keeps its identity only when it is one of the sentinels
// tables lists (the handler carries those across restate as error codes),
// and is otherwise its message alone. an error restate would still be
// retrying, or the caller's own cancellation, is returned as it is.
func crossed(err error, tables ...[]error) error {
	if err == nil || !terminal(err) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// the last match in a table wins, as internal/durable's sentinelIndex.
	for _, table := range tables {
		for i := len(table) - 1; i >= 0; i-- {
			if errors.Is(err, table[i]) {
				return &sentinelError{msg: err.Error(), sentinel: table[i]}
			}
		}
	}
	return errors.New(err.Error())
}

// sentinelError is a sentinel brought back across restate with the
// handler's message, as internal/durable's of the same name.
type sentinelError struct {
	msg      string
	sentinel error
}

func (e *sentinelError) Error() string { return e.msg }
func (e *sentinelError) Unwrap() error { return e.sentinel }

// handlerCtx is the context a handler runs under: the caller's values, but
// not its cancellation, as an invocation keeps running when its caller gives
// up.
func handlerCtx(ctx context.Context) context.Context {
	return context.WithoutCancel(ctx)
}

// call is one awaited invocation, which a retry under the same
// idempotency key attaches to.
type call struct {
	done chan struct{}
	val  any
	err  error
}

// request runs fn as an awaited invocation. scope names the handler
// and object key the caller's idempotency key is scoped to, "" for a call the
// client sends unkeyed. a second call under the same key gets the first one's
// result. fn keeps running if ctx ends first; the caller then gets ctx's
// error as one that did not settle.
func request[T any](d *Inline, ctx context.Context, scope string, fn func(context.Context) (T, error)) (T, error) {
	key := ""
	if scope != "" {
		key = orchestrator.IdempotencyKey(ctx)
	}
	hctx := handlerCtx(ctx)
	d.mu.Lock()
	c := d.calls[scope+"|"+key]
	first := c == nil || key == ""
	if first {
		c = &call{done: make(chan struct{})}
		if key != "" {
			d.calls[scope+"|"+key] = c
		}
	}
	d.mu.Unlock()
	if first {
		d.goTracked(func() {
			v, err := fn(hctx)
			c.val, c.err = v, err
			close(c.done)
		})
	}
	var zero T
	select {
	case <-c.done:
	case <-ctx.Done():
		select {
		case <-c.done:
		default:
			return zero, &retrying{err: ctx.Err()}
		}
	}
	v, ok := c.val.(T)
	if !ok {
		v = zero
	}
	return v, c.err
}

// chainObject is the checkpoint chain the Environment object keeps for a vm.
type chainObject struct {
	chain   orchestrator.ChainState
	pending *chainPending
	dropped bool
}

// chainPending is a tick's checkpoint on its way to the standby.
type chainPending struct {
	child   orchestrator.SnapshotRecord
	standby string
	diff    bool
	localID string
	since   int64
}

// chainHolder is the ArtifactPin holder of vmID's chain base.
func chainHolder(vmID string) string { return "chain:" + vmID }

// forking is one run of the fork handler and what its compensation undoes.
type forking struct {
	plan   orchestrator.ForkPlan
	booted bool
	hostID string
	seed   orchestrator.SnapshotRecord
	pinned string
}

// migration is one run of the migrate handler and what its
// compensation undoes.
type migration struct {
	plan     orchestrator.MigratePlan
	placed   bool
	frozenAt int64
	seed     orchestrator.SnapshotRecord
}

// Inline stands in for restate and the internal/durable handlers: each
// method runs the fleet's step methods in the order the handler it names
// does, with the same compensation. a step runs once; a failure counts as
// terminal where the handler bounds its attempts, and as still retrying
// (returned, nothing compensated) where it does not. virtual-object state
// lives in maps, and ops on one object key hold that key's mutex.
type Inline struct {
	fm *orchestrator.FleetManager

	mu      sync.Mutex
	idle    *sync.Cond
	running int
	keys    map[string]*sync.Mutex
	calls   map[string]*call
	sent    map[string]bool
	moves   int

	creating map[string]bool
	chains   map[string]chainObject
	pins     map[string]map[string]int64
	arms     []string
	graces   map[string]time.Duration
}

func newInline(fm *orchestrator.FleetManager) *Inline {
	d := &Inline{
		fm:       fm,
		keys:     map[string]*sync.Mutex{},
		calls:    map[string]*call{},
		sent:     map[string]bool{},
		creating: map[string]bool{},
		chains:   map[string]chainObject{},
		pins:     map[string]map[string]int64{},
		graces:   map[string]time.Duration{},
	}
	d.idle = sync.NewCond(&d.mu)
	return d
}

// Wire builds an Inline for fm and sets it as all twelve of fm's durable
// clients. call it before Start.
func Wire(fm *orchestrator.FleetManager) *Inline {
	d := newInline(fm)
	fm.SetDurable(d)
	return d
}

// NewFleet is orchestrator.NewFleetManager(cfg) wired to an Inline, with
// TestKey as the token key when cfg has none.
func NewFleet(cfg orchestrator.FleetConfig) (*orchestrator.FleetManager, *Inline) {
	if len(cfg.TokenEncryptionKey) == 0 {
		cfg.TokenEncryptionKey = []byte(TestKey)
	}
	fm := orchestrator.NewFleetManager(cfg)
	return fm, Wire(fm)
}

// lock holds object/key, as restate runs one exclusive handler per key at a
// time. it returns the unlock.
func (d *Inline) lock(object, key string) func() {
	d.mu.Lock()
	m := d.keys[object+"/"+key]
	if m == nil {
		m = &sync.Mutex{}
		d.keys[object+"/"+key] = m
	}
	d.mu.Unlock()
	m.Lock()
	return m.Unlock
}

// lockEnv holds Environment/{vmID}.
func (d *Inline) lockEnv(vmID string) func() { return d.lock("Environment", vmID) }

// goTracked runs fn on a goroutine Wait waits for.
func (d *Inline) goTracked(fn func()) {
	d.mu.Lock()
	d.running++
	d.mu.Unlock()
	go func() {
		defer func() {
			d.mu.Lock()
			d.running--
			if d.running == 0 {
				d.idle.Broadcast()
			}
			d.mu.Unlock()
		}()
		fn()
	}()
}

// after runs fn once delay has passed, as a delayed send. Wait covers it only
// once it has started.
func (d *Inline) after(delay time.Duration, fn func()) {
	if delay <= 0 {
		d.goTracked(fn)
		return
	}
	time.AfterFunc(delay, func() { d.goTracked(fn) })
}

// Wait blocks until no one-way send or awaited invocation is running.
func (d *Inline) Wait() {
	d.mu.Lock()
	for d.running > 0 {
		d.idle.Wait()
	}
	d.mu.Unlock()
}

// once reports whether key is new, marking it seen: an idempotency key or a
// workflow id restate runs once.
func (d *Inline) once(key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.sent[key] {
		return false
	}
	d.sent[key] = true
	return true
}

// Armed is every vm id SendArmExpiry was given, in order.
func (d *Inline) Armed() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.arms...)
}

// destroy

// Destroy mirrors Environment/{vm_id}.Destroy, awaited.
func (d *Inline) Destroy(ctx context.Context, vmID string, req orchestrator.DestroyRequest) error {
	_, err := request(d, ctx, "Environment/"+vmID+"/Destroy", func(c context.Context) (struct{}, error) {
		return struct{}{}, d.destroyLocked(c, vmID, req)
	})
	return err
}

// SendDestroy mirrors a one-way send to Environment/{vm_id}.Destroy. it runs
// the handler before it returns: callers that send it rely on the teardown
// having happened, as with the orchestrator package's inlineDestroyer.
func (d *Inline) SendDestroy(ctx context.Context, vmID string, req orchestrator.DestroyRequest) error {
	_ = d.destroyLocked(handlerCtx(ctx), vmID, req)
	return nil
}

func (d *Inline) destroyLocked(ctx context.Context, vmID string, req orchestrator.DestroyRequest) error {
	defer d.lockEnv(vmID)()
	return d.destroy(ctx, vmID, req)
}

// destroy mirrors the steps of Environment/{vm_id}.Destroy (destroy in
// internal/durable/destroy.go), without taking the key.
func (d *Inline) destroy(ctx context.Context, vmID string, req orchestrator.DestroyRequest) error {
	fm := d.fm
	target, err := fm.DestroyBegin(ctx, vmID, req.Reason)
	if err != nil {
		return held(err, nil)
	}
	if target.Replaced {
		return nil
	}
	hostID := target.HostID
	if !target.Tracked {
		if req.HostID == "" && !orchestrator.CleanupOfCreatedVM(req.Reason) {
			return d.dropChain(ctx, vmID)
		}
		hostID = req.HostID
	}
	if err := fm.DestroyReleaseEgress(ctx, vmID); err != nil {
		return held(err, nil)
	}
	if err := fm.DestroyOnHost(ctx, vmID, hostID); err != nil {
		hostErr := bounded(err)
		if ferr := fm.DestroyFailed(ctx, vmID, req.Reason, hostErr.Error()); ferr != nil {
			return held(ferr, nil)
		}
		return hostErr
	}
	if err := fm.DestroyUnpublishIngress(ctx, vmID); err != nil {
		return held(err, nil)
	}
	if err := fm.DestroyFinish(ctx, vmID, req.Reason); err != nil {
		return held(err, nil)
	}
	return d.dropChain(ctx, vmID)
}

// create

// Create mirrors Environment/{vm_id}.Create (createMarked), awaited. the
// cleanup destroy it sends runs once the create has let go of the key.
func (d *Inline) Create(ctx context.Context, vmID string, req orchestrator.CreateRequest) error {
	_, err := request(d, ctx, "Environment/"+vmID+"/Create", func(c context.Context) (struct{}, error) {
		cleanup, err := d.createLocked(c, vmID, req)
		if cleanup != nil {
			_ = d.SendDestroy(c, vmID, *cleanup)
		}
		return struct{}{}, err
	})
	return crossed(err, orchestrator.CreateFleetErrors)
}

// CreateInFlight mirrors Environment/{vm_id}.CreateInFlight.
func (d *Inline) CreateInFlight(_ context.Context, vmID string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.creating[vmID], nil
}

func (d *Inline) setCreating(vmID string, on bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if on {
		d.creating[vmID] = true
		return
	}
	delete(d.creating, vmID)
}

// createLocked mirrors createMarked: the in-flight mark is set before the
// first step and cleared once the create has ended either way.
func (d *Inline) createLocked(ctx context.Context, vmID string, req orchestrator.CreateRequest) (*orchestrator.DestroyRequest, error) {
	defer d.lockEnv(vmID)()
	d.setCreating(vmID, true)
	cleanup, err := d.create(ctx, vmID, req)
	if err == nil || terminal(err) {
		d.setCreating(vmID, false)
	}
	return cleanup, err
}

// create mirrors create in internal/durable/create.go. it returns the cleanup
// destroy the compensation sends, if any.
func (d *Inline) create(ctx context.Context, vmID string, req orchestrator.CreateRequest) (*orchestrator.DestroyRequest, error) {
	fm := d.fm
	st, err := fm.CreateBegin(ctx, vmID, req)
	if err != nil {
		err = held(err, orchestrator.PermanentCreateError)
		if terminal(err) {
			if cerr := fm.CreateBeginFailed(ctx, vmID, req, err.Error()); cerr != nil {
				return nil, held(cerr, orchestrator.PermanentCreateError)
			}
		}
		return nil, err
	}
	if err := d.createSteps(ctx, vmID, req, &st); err != nil {
		return d.compensateCreate(ctx, vmID, req, st, err)
	}
	return nil, nil
}

// createSteps mirrors createSteps: every step is bounded, so any failure is
// terminal.
func (d *Inline) createSteps(ctx context.Context, vmID string, req orchestrator.CreateRequest, st *orchestrator.CreateState) error {
	fm := d.fm
	var err error
	if st.HostID, err = fm.CreatePlace(ctx, vmID, req, *st); err != nil {
		return bounded(err)
	}
	if st.SeedSnapshotID, err = fm.CreateArtifact(ctx, vmID, req, *st); err != nil {
		return bounded(err)
	}
	if st.Ingress, err = fm.CreatePublishIngress(ctx, vmID, req, *st); err != nil {
		return bounded(err)
	}
	st.Booted = true
	if st.FromCache, err = fm.CreateRestoreOrCreate(ctx, vmID, req, *st); err != nil {
		return bounded(err)
	}
	for _, fn := range []func(context.Context, string, orchestrator.CreateRequest, orchestrator.CreateState) error{
		fm.CreateProvisionEgress, fm.CreateUpload, fm.CreateStartupScript,
	} {
		if err := fn(ctx, vmID, req, *st); err != nil {
			return bounded(err)
		}
	}
	if st.Endpoints, err = fm.CreateStartAgent(ctx, vmID, req, *st); err != nil {
		return bounded(err)
	}
	return bounded(fm.CreateRunning(ctx, vmID, req, *st))
}

// compensateCreate mirrors compensateCreate: release and unpublish are best
// effort, the abandon has no attempt bound, and a booted vm gets a cleanup
// destroy told its host.
func (d *Inline) compensateCreate(ctx context.Context, vmID string, req orchestrator.CreateRequest, st orchestrator.CreateState, cause error) (*orchestrator.DestroyRequest, error) {
	fm := d.fm
	_ = fm.CreateReleaseEgress(ctx, vmID)
	_ = fm.CreateUnpublishIngress(ctx, vmID)
	if err := fm.CreateAbandon(ctx, vmID, req, cause.Error()); err != nil {
		return nil, held(err, orchestrator.PermanentCreateError)
	}
	if st.Booted {
		return &orchestrator.DestroyRequest{Reason: orchestrator.DestroyReasonCreateCleanup, HostID: st.HostID}, cause
	}
	return nil, cause
}

// fork

// Fork mirrors Environment/{vm_id}.Fork (forkMarked), keyed by the new vm's
// id, awaited. the rollback destroy runs once the fork has let go of the key.
func (d *Inline) Fork(ctx context.Context, vmID string, req orchestrator.ForkRequest) error {
	_, err := request(d, ctx, "Environment/"+vmID+"/Fork", func(c context.Context) (struct{}, error) {
		cleanup, err := d.forkLocked(c, vmID, req)
		if cleanup != nil {
			_ = d.SendDestroy(c, vmID, *cleanup)
		}
		return struct{}{}, err
	})
	return crossed(err, orchestrator.ForkRefusals, orchestrator.SnapshotFleetErrors)
}

func (d *Inline) forkLocked(ctx context.Context, vmID string, req orchestrator.ForkRequest) (*orchestrator.DestroyRequest, error) {
	defer d.lockEnv(vmID)()
	d.setCreating(vmID, true)
	cleanup, err := d.fork(ctx, vmID, req)
	if err == nil || terminal(err) {
		d.setCreating(vmID, false)
	}
	return cleanup, err
}

// fork mirrors fork in internal/durable/fork.go. begin's every failure is a
// refusal, so terminal.
func (d *Inline) fork(ctx context.Context, vmID string, req orchestrator.ForkRequest) (*orchestrator.DestroyRequest, error) {
	plan, err := d.fm.ForkBegin(ctx, vmID, req)
	if err != nil {
		return nil, bounded(err)
	}
	f := &forking{plan: plan}
	seed, err := d.forkRun(ctx, f)
	if err != nil {
		if !terminal(err) {
			return nil, err
		}
		cleanup, cerr := d.forkCompensate(ctx, f, err)
		if cerr != nil {
			return nil, cerr
		}
		return cleanup, err
	}
	return nil, d.forkFinish(ctx, f, seed)
}

// forkRun mirrors forking.run: up to running. the seed is taken by a call to
// the source's own Environment.Snapshot.
func (d *Inline) forkRun(ctx context.Context, f *forking) (orchestrator.SnapshotRecord, error) {
	fm, plan := d.fm, f.plan
	if err := fm.ForkPlaceholder(ctx, plan); err != nil {
		return orchestrator.SnapshotRecord{}, bounded(err)
	}
	seed := plan.Seed
	if seed.SnapshotID == "" {
		var err error
		seed, err = d.snapshotLocked(ctx, plan.SourceVMID, orchestrator.SnapshotOptions{Comment: plan.Comment})
		if err != nil {
			return seed, err
		}
		f.seed = seed
	}
	if err := d.pin(seed.SnapshotID, "fork:"+plan.NewVMID, 24*time.Hour); err != nil {
		return seed, err
	}
	f.pinned = seed.SnapshotID

	hostID, err := fm.ForkPlace(ctx, plan, seed)
	if err != nil {
		return seed, bounded(err)
	}
	localID := ""
	if hostID != plan.SourceHostID {
		if localID, err = fm.ForkArtifact(ctx, plan, seed, hostID); err != nil {
			return seed, bounded(err)
		}
	}
	f.booted, f.hostID = true, hostID
	if err := fm.ForkCreate(ctx, plan, seed, hostID, localID); err != nil {
		return seed, bounded(err)
	}
	var started orchestrator.MigrateStarted
	if started.Ingress, err = fm.ForkIngress(ctx, plan); err != nil {
		return seed, bounded(err)
	}
	if started.Credentials, err = fm.ForkCredentials(ctx, plan); err != nil {
		return seed, bounded(err)
	}
	for _, fn := range []func(context.Context, orchestrator.ForkPlan, orchestrator.MigrateStarted) error{
		fm.ForkUpload, fm.ForkStartAgent, fm.ForkRunning,
	} {
		if err := fn(ctx, plan, started); err != nil {
			return seed, bounded(err)
		}
	}
	return seed, nil
}

// forkFinish mirrors forking.finish: task, lineage, then the seed's pin goes.
func (d *Inline) forkFinish(ctx context.Context, f *forking, seed orchestrator.SnapshotRecord) error {
	if err := d.fm.ForkTask(ctx, f.plan); err != nil {
		return bounded(err)
	}
	if err := d.fm.ForkLineage(ctx, f.plan, seed); err != nil {
		return bounded(err)
	}
	d.forkRelease(f)
	return nil
}

func (d *Inline) forkRelease(f *forking) {
	if f.pinned != "" {
		d.dropHolder(f.pinned, "fork:"+f.plan.NewVMID, 0, false)
	}
}

// forkCompensate mirrors forking.compensate. it returns the rollback destroy
// of a vm that may exist on its host.
func (d *Inline) forkCompensate(ctx context.Context, f *forking, cause error) (*orchestrator.DestroyRequest, error) {
	fm, plan := d.fm, f.plan
	_ = fm.ForkUnpublishIngress(ctx, plan)
	if err := fm.ForkAbandon(ctx, plan, cause.Error()); err != nil {
		return nil, held(err, nil)
	}
	var cleanup *orchestrator.DestroyRequest
	if f.booted {
		cleanup = &orchestrator.DestroyRequest{Reason: orchestrator.DestroyReasonForkRollback, HostID: f.hostID}
	}
	d.forkRelease(f)
	if f.seed.SnapshotID != "" {
		_ = fm.ForkDeleteSeed(ctx, f.seed)
	}
	return cleanup, nil
}

// snapshots

// Snapshot mirrors Environment/{vm_id}.Snapshot, awaited.
func (d *Inline) Snapshot(ctx context.Context, vmID string, opts orchestrator.SnapshotOptions) (orchestrator.SnapshotRecord, error) {
	v, err := request(d, ctx, "Environment/"+vmID+"/Snapshot", func(c context.Context) (orchestrator.SnapshotRecord, error) {
		return d.snapshotLocked(c, vmID, opts)
	})
	return v, crossed(err, orchestrator.SnapshotFleetErrors)
}

func (d *Inline) snapshotLocked(ctx context.Context, vmID string, opts orchestrator.SnapshotOptions) (orchestrator.SnapshotRecord, error) {
	defer d.lockEnv(vmID)()
	return d.snapshotSteps(ctx, vmID, opts)
}

// snapshotSteps mirrors snapshotSteps in internal/durable/snapshot.go, without
// taking the key: begin, checkpoint, record, list, ready, and the abort of a
// record that cannot be finished.
func (d *Inline) snapshotSteps(ctx context.Context, vmID string, opts orchestrator.SnapshotOptions) (orchestrator.SnapshotRecord, error) {
	fm := d.fm
	plan, err := fm.SnapshotBegin(ctx, vmID, opts)
	if err != nil {
		return orchestrator.SnapshotRecord{}, held(err, orchestrator.PermanentSnapshotError)
	}
	created, err := fm.SnapshotCheckpoint(ctx, vmID, plan)
	if err != nil {
		return orchestrator.SnapshotRecord{}, bounded(err)
	}
	record, err := fm.SnapshotRecordCreating(ctx, vmID, plan, created)
	var listed orchestrator.Checkpoint
	if err == nil {
		listed, err = fm.SnapshotList(ctx, vmID, created.ID)
	}
	if err == nil {
		record, err = fm.SnapshotReady(ctx, record, listed)
	}
	if err != nil {
		err = bounded(err)
		_ = fm.SnapshotAbort(ctx, vmID, plan, created, err.Error())
		return orchestrator.SnapshotRecord{}, err
	}
	return record, nil
}

// Restore mirrors Environment/{vm_id}.Restore, awaited.
func (d *Inline) Restore(ctx context.Context, vmID, snapshotID string) error {
	_, err := request(d, ctx, "Environment/"+vmID+"/Restore", func(c context.Context) (struct{}, error) {
		defer d.lockEnv(vmID)()
		return struct{}{}, d.restore(c, vmID, snapshotID)
	})
	return crossed(err, orchestrator.SnapshotFleetErrors)
}

// restore mirrors restore in internal/durable/snapshot.go.
func (d *Inline) restore(ctx context.Context, vmID, snapshotID string) error {
	fm := d.fm
	record, err := fm.RestoreValidate(ctx, vmID, snapshotID)
	if err != nil {
		return held(err, orchestrator.PermanentSnapshotError)
	}
	if err := fm.RestoreBegin(ctx, record); err != nil {
		return bounded(err)
	}
	if err := fm.RestoreOnHost(ctx, vmID, snapshotID); err != nil {
		hostErr := bounded(err)
		_ = fm.RestoreFailed(ctx, record, hostErr.Error())
		return hostErr
	}
	return bounded(fm.RestoreFinish(ctx, record))
}

// SendCollectSnapshot mirrors a one-way send to
// Environment/{vm_id}.CollectSnapshot, deduped on key.
func (d *Inline) SendCollectSnapshot(ctx context.Context, vmID, snapshotID, key string) error {
	if !d.once("Environment/" + vmID + "/CollectSnapshot|" + key) {
		return nil
	}
	hctx := handlerCtx(ctx)
	d.goTracked(func() {
		defer d.lockEnv(vmID)()
		d.collectSnapshot(hctx, snapshotID)
	})
	return nil
}

// collectSnapshot mirrors collectSnapshot: a deletion that fails for good is
// recorded on the row, and the invocation itself is done.
func (d *Inline) collectSnapshot(ctx context.Context, snapshotID string) {
	if err := d.fm.SnapshotCollect(ctx, snapshotID); err != nil {
		_ = d.fm.SnapshotCollectFailed(ctx, snapshotID, err.Error())
	}
}

// checkpoint chain

func (d *Inline) chain(vmID string) chainObject {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.chains[vmID]
}

func (d *Inline) setChain(vmID string, st chainObject) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.chains[vmID] = st
}

func (d *Inline) clearChain(vmID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.chains, vmID)
}

// saveChain mirrors saveChain: st is stored and mirrored into the fleet, and
// an object holding nothing is cleared.
func (d *Inline) saveChain(vmID string, st chainObject) {
	d.fm.MirrorChain(vmID, st.chain, st.pending != nil)
	if st.chain.Empty() && st.pending == nil && !st.dropped {
		d.clearChain(vmID)
		return
	}
	d.setChain(vmID, st)
}

// Checkpoint mirrors Environment/{vm_id}.Checkpoint, awaited. the copy to the
// standby runs on after it returns.
func (d *Inline) Checkpoint(ctx context.Context, vmID string) error {
	_, err := request(d, ctx, "", func(c context.Context) (struct{}, error) {
		defer d.lockEnv(vmID)()
		return struct{}{}, d.checkpointTick(c, vmID)
	})
	return err
}

// checkpointTick mirrors checkpointTick in internal/durable/checkpoint.go.
func (d *Inline) checkpointTick(ctx context.Context, vmID string) error {
	fm := d.fm
	st := d.chain(vmID)
	fm.MirrorChain(vmID, st.chain, st.pending != nil)
	if st.dropped {
		return nil
	}
	now := time.Now().UnixMilli()
	forgot, err := d.forgetLapsedCopy(ctx, vmID, &st, now)
	if err != nil {
		return err
	}
	if forgot {
		d.saveChain(vmID, st)
		return nil
	}
	if st.pending != nil {
		return nil
	}

	plan, err := fm.ChainBegin(ctx, vmID, st.chain)
	if err != nil {
		return held(err, nil)
	}
	if plan.Gone || !plan.Diff {
		if err := d.retireChain(ctx, vmID, st.chain); err != nil {
			return err
		}
		st.chain = orchestrator.ChainState{}
	}
	if plan.Gone || plan.Standby == "" {
		d.saveChain(vmID, st)
		return nil
	}

	child, err := d.snapshotSteps(ctx, vmID, plan.Options)
	if err != nil {
		if !terminal(err) {
			return err
		}
		if plan.Diff {
			if err := d.chainReset(ctx, vmID, err.Error()); err != nil {
				return err
			}
			if err := d.retireChain(ctx, vmID, st.chain); err != nil {
				return err
			}
			st.chain = orchestrator.ChainState{}
		}
		d.saveChain(vmID, st)
		return nil
	}

	pending := &chainPending{child: child, standby: plan.Standby, diff: plan.Diff}
	mv, err := fm.ChainMovePlan(ctx, st.chain, plan, child)
	if err != nil {
		return d.chainMoved(ctx, vmID, st, pending, orchestrator.ChainMoveResult{Err: err.Error()})
	}
	if mv.Existing != "" {
		return d.chainMoved(ctx, vmID, st, pending, orchestrator.ChainMoveResult{ID: mv.Existing})
	}
	job := mv.Job
	job.Notify = vmID
	pending.localID, pending.since = job.LocalID, now
	st.pending = pending
	d.saveChain(vmID, st)
	d.goTracked(func() { _, _ = d.moveLocked(context.Background(), job) })
	return nil
}

// chainMovedHandler mirrors Environment/{vm_id}.ChainMoved. the caller holds
// the key.
func (d *Inline) chainMovedHandler(ctx context.Context, vmID string, res orchestrator.ChainMoveResult) error {
	st := d.chain(vmID)
	p := st.pending
	if p == nil || p.localID != res.LocalID {
		d.fm.MirrorChain(vmID, st.chain, p != nil)
		return nil
	}
	st.pending = nil
	return d.chainMoved(ctx, vmID, st, p, res)
}

// chainMoved mirrors chainMoved: the end of a tick's copy.
func (d *Inline) chainMoved(ctx context.Context, vmID string, st chainObject, p *chainPending, res orchestrator.ChainMoveResult) error {
	if st.dropped {
		if res.Err == "" {
			if err := d.retireChain(ctx, vmID, orchestrator.ChainState{Standby: p.standby, Base: res.ID}); err != nil {
				return err
			}
		}
		if err := d.retireChain(ctx, vmID, orchestrator.ChainState{Head: p.child}); err != nil {
			return err
		}
		return d.dropChainNow(ctx, vmID, st)
	}
	if res.Err != "" {
		if err := d.chainReset(ctx, vmID, res.Err); err != nil {
			return err
		}
		if err := d.retireChain(ctx, vmID, orchestrator.ChainState{Head: p.child}); err != nil {
			return err
		}
		if p.diff {
			if err := d.retireChain(ctx, vmID, st.chain); err != nil {
				return err
			}
			st.chain = orchestrator.ChainState{}
		}
		d.saveChain(vmID, st)
		return nil
	}

	if err := d.pin(res.ID, chainHolder(vmID), 0); err != nil {
		return err
	}
	old := st.chain
	if p.diff {
		if old.Base != "" && old.Base != res.ID {
			d.dropHolder(old.Base, chainHolder(vmID), 0, false)
		}
		st.chain.Head, st.chain.Base = p.child, res.ID
		if err := d.retireChain(ctx, vmID, orchestrator.ChainState{Head: old.Head}); err != nil {
			return err
		}
	} else {
		st.chain = orchestrator.ChainState{Standby: p.standby, Head: p.child, Base: res.ID, Bytes: p.child.SizeBytes}
	}
	if err := d.fm.ChainAdvanced(ctx, vmID, st.chain, p.diff); err != nil {
		return held(err, nil)
	}
	d.saveChain(vmID, st)
	return nil
}

// forgetLapsedCopy mirrors forgetLapsedCopy: a copy that has not reported
// back within pendingLapse is dropped with the chain, deleting nothing.
func (d *Inline) forgetLapsedCopy(ctx context.Context, vmID string, st *chainObject, now int64) (bool, error) {
	if st.pending == nil || now-st.pending.since < pendingLapse.Milliseconds() {
		return false, nil
	}
	if err := d.chainReset(ctx, vmID, "checkpoint copy "+st.pending.localID+" never reported back"); err != nil {
		return false, err
	}
	if st.chain.Base != "" {
		d.dropHolder(st.chain.Base, chainHolder(vmID), 0, false)
	}
	st.pending, st.chain = nil, orchestrator.ChainState{}
	return true, nil
}

// chainReset mirrors chainReset.
func (d *Inline) chainReset(ctx context.Context, vmID, reason string) error {
	return held(d.fm.ChainReset(ctx, vmID, reason), nil)
}

// retireChain mirrors retireChain: what chain holds is removed and the pin on
// its base dropped.
func (d *Inline) retireChain(ctx context.Context, vmID string, chain orchestrator.ChainState) error {
	if chain.Empty() {
		return nil
	}
	if chain.Base != "" {
		d.dropHolder(chain.Base, chainHolder(vmID), 0, false)
	}
	return held(d.fm.ChainRetire(ctx, vmID, chain), nil)
}

// dropChainNow mirrors dropChainNow.
func (d *Inline) dropChainNow(ctx context.Context, vmID string, st chainObject) error {
	if err := d.retireChain(ctx, vmID, st.chain); err != nil {
		return err
	}
	d.fm.MirrorChain(vmID, orchestrator.ChainState{}, false)
	d.clearChain(vmID)
	return nil
}

// SendRetireChain mirrors a one-way send to Environment/{vm_id}.RetireChain.
func (d *Inline) SendRetireChain(ctx context.Context, vmID string) error {
	hctx := handlerCtx(ctx)
	d.goTracked(func() {
		defer d.lockEnv(vmID)()
		_ = d.dropChain(hctx, vmID)
	})
	return nil
}

// dropChain mirrors dropChain (Environment/{vm_id}.RetireChain and the last
// step of a destroy), without taking the key. a copy still in flight only
// marks the chain dropped, with a delayed retry for when its lapse is due.
func (d *Inline) dropChain(ctx context.Context, vmID string) error {
	st := d.chain(vmID)
	if st.chain.Empty() && st.pending == nil {
		return nil
	}
	now := time.Now().UnixMilli()
	forgot, err := d.forgetLapsedCopy(ctx, vmID, &st, now)
	if err != nil {
		return err
	}
	if forgot {
		d.fm.MirrorChain(vmID, orchestrator.ChainState{}, false)
		d.clearChain(vmID)
		return nil
	}
	if st.pending != nil {
		st.dropped = true
		d.setChain(vmID, st)
		due := st.pending.since + pendingLapse.Milliseconds()
		delay := time.Duration(max(due-now, 0))*time.Millisecond + time.Minute
		d.after(delay, func() {
			defer d.lockEnv(vmID)()
			_ = d.dropChain(context.Background(), vmID)
		})
		return nil
	}
	return d.dropChainNow(ctx, vmID, st)
}

// artifacts

// MoveArtifact mirrors Artifacts/{local_id}.Move, awaited. an error that is
// not the move's own failure wraps ErrArtifactMoveUnsettled.
func (d *Inline) MoveArtifact(ctx context.Context, job orchestrator.ArtifactMoveJob) (string, error) {
	id, err := request(d, ctx, "", func(c context.Context) (string, error) {
		return d.moveLocked(c, job)
	})
	if err != nil && !terminal(err) {
		return "", fmt.Errorf("%w: %w", orchestrator.ErrArtifactMoveUnsettled, err)
	}
	return id, err
}

// moveLocked mirrors move: the steps under Artifacts/{local_id}, then, for a
// checkpoint chain's copy, a one-way send of the result to the vm's
// Environment.ChainMoved.
func (d *Inline) moveLocked(ctx context.Context, job orchestrator.ArtifactMoveJob) (string, error) {
	unlock := d.lock("Artifacts", job.LocalID)
	id, err := d.moveSteps(ctx, job)
	unlock()
	if job.Notify != "" && (err == nil || terminal(err)) {
		res := orchestrator.ChainMoveResult{LocalID: job.LocalID, ID: id}
		if err != nil {
			res.Err = err.Error()
		}
		d.goTracked(func() {
			defer d.lockEnv(job.Notify)()
			_ = d.chainMovedHandler(context.Background(), job.Notify, res)
		})
	}
	return id, err
}

// moveSteps mirrors moveSteps in internal/durable/artifacts.go: check, pin
// both ends, pull, record, and the discard of a copy that cannot be finished.
func (d *Inline) moveSteps(ctx context.Context, job orchestrator.ArtifactMoveJob) (string, error) {
	fm := d.fm
	committed, err := fm.ArtifactMoveCommitted(ctx, job)
	if err != nil {
		return "", held(err, nil)
	}
	if committed != "" {
		return committed, nil
	}
	d.mu.Lock()
	d.moves++
	holder := fmt.Sprintf("move:inline-%d", d.moves)
	d.mu.Unlock()
	ids := job.PinIDs()
	for _, id := range ids {
		if err := d.pin(id, holder, 0); err != nil {
			return "", err
		}
	}
	moved, err := fm.ArtifactPull(ctx, job)
	var id string
	if err == nil {
		id, err = fm.ArtifactRecord(ctx, job, moved)
	}
	if err != nil {
		err = bounded(err)
		_ = fm.ArtifactDiscard(ctx, job)
		d.releasePins(holder, ids)
		return "", err
	}
	d.releasePins(holder, ids)
	return id, nil
}

func (d *Inline) releasePins(holder string, ids []string) {
	for _, id := range ids {
		d.dropHolder(id, holder, 0, false)
	}
}

// pin mirrors ArtifactPin/{snapshot_id}.Pin: a zero ttl never lapses, any
// other lapses by a delayed Expire.
func (d *Inline) pin(id, holder string, ttl time.Duration) error {
	if holder == "" || ttl < 0 {
		return errors.New("pin needs a holder and a ttl that is not negative")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	holders := d.pins[id]
	if holders == nil {
		holders = map[string]int64{}
		d.pins[id] = holders
	}
	if ttl == 0 {
		holders[holder] = 0
		return nil
	}
	deadline := time.Now().UnixMilli() + ttl.Milliseconds()
	holders[holder] = deadline
	time.AfterFunc(ttl, func() { d.dropHolder(id, holder, deadline, true) })
	return nil
}

// dropHolder mirrors ArtifactPin/{snapshot_id}.Release (lapse false) and
// .Expire (lapse true, which drops only the pin deadline belongs to).
func (d *Inline) dropHolder(id, holder string, deadline int64, lapse bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	holders := d.pins[id]
	got, ok := holders[holder]
	if !ok || (lapse && (got == 0 || got != deadline)) {
		return
	}
	delete(holders, holder)
	if len(holders) == 0 {
		delete(d.pins, id)
	}
}

// PinArtifacts mirrors ArtifactPin/{id}.Pin for each id.
func (d *Inline) PinArtifacts(_ context.Context, holder string, ttl time.Duration, ids ...string) error {
	for _, id := range ids {
		if err := d.pin(id, holder, ttl); err != nil {
			return err
		}
	}
	return nil
}

// ReleaseArtifacts mirrors ArtifactPin/{id}.Release for each id.
func (d *Inline) ReleaseArtifacts(_ context.Context, holder string, ids ...string) error {
	for _, id := range ids {
		d.dropHolder(id, holder, 0, false)
	}
	return nil
}

// ArtifactPinned mirrors ArtifactPin/{id}.Pinned.
func (d *Inline) ArtifactPinned(_ context.Context, id string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.pins[id]) > 0, nil
}

// token rotation

// RotateToken mirrors Environment/{vm_id}.RotateToken, awaited.
func (d *Inline) RotateToken(ctx context.Context, vmID string) error {
	_, err := request(d, ctx, "", func(c context.Context) (struct{}, error) {
		return struct{}{}, d.rotateLocked(c, vmID)
	})
	return crossed(err, orchestrator.SnapshotFleetErrors)
}

func (d *Inline) rotateLocked(ctx context.Context, vmID string) error {
	defer d.lockEnv(vmID)()
	return d.rotateToken(ctx, vmID)
}

// rotatePermanent is rotatePermanent's test: a rotation refusal, or a
// snapshot sentinel or agent 4xx.
func rotatePermanent(err error) bool {
	return orchestrator.TokenRotationRefused(err) || orchestrator.PermanentSnapshotError(err)
}

// rotateToken mirrors rotateToken: generate, upload (bounded), persist (no
// attempt bound).
func (d *Inline) rotateToken(ctx context.Context, vmID string) error {
	fm := d.fm
	rotated, err := fm.RotateTokenGenerate(ctx, vmID)
	if err != nil {
		return held(err, rotatePermanent)
	}
	if err := fm.RotateTokenUpload(ctx, vmID, rotated); err != nil {
		return bounded(err)
	}
	return held(fm.RotateTokenPersist(ctx, vmID, rotated), rotatePermanent)
}

// drain and expiry

// Drain mirrors Environment/{vm_id}.Drain, awaited. the grace timer it arms
// is recorded, not run.
func (d *Inline) Drain(ctx context.Context, vmID string) error {
	_, err := request(d, ctx, "Environment/"+vmID+"/Drain", func(c context.Context) (struct{}, error) {
		defer d.lockEnv(vmID)()
		return struct{}{}, d.drain(c, vmID)
	})
	return crossed(err, orchestrator.SnapshotFleetErrors)
}

// drain mirrors drain in internal/durable/expiry.go.
func (d *Inline) drain(ctx context.Context, vmID string) error {
	fm := d.fm
	if err := fm.DrainCheck(ctx, vmID); err != nil {
		return held(err, orchestrator.PermanentSnapshotError)
	}
	plan, err := fm.DrainBegin(ctx, vmID)
	if err != nil {
		return bounded(err)
	}
	d.mu.Lock()
	d.graces[vmID] = plan.Grace
	d.mu.Unlock()
	if !plan.Command {
		return nil
	}
	if msg := fm.DrainCommand(ctx, vmID); msg != "" {
		return fmt.Errorf("drain vm %s: %s", vmID, msg)
	}
	return nil
}

// SendArmExpiry stands in for a one-way send to Environment/{vm_id}.Expire.
// the timer is tested elsewhere: the arm is only recorded.
func (d *Inline) SendArmExpiry(_ context.Context, vmID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.arms = append(d.arms, vmID)
	return nil
}

// migrate

// Migrate mirrors Environment/{vm_id}.Migrate on the source's key, awaited,
// returning the new vm's id.
func (d *Inline) Migrate(ctx context.Context, vmID string, req orchestrator.MigrateRequest) (string, error) {
	v, err := request(d, ctx, "Environment/"+vmID+"/Migrate", func(c context.Context) (string, error) {
		defer d.lockEnv(vmID)()
		return d.migrate(c, vmID, req)
	})
	return v, crossed(err, orchestrator.MigrateRefusals, orchestrator.SnapshotFleetErrors)
}

// migrate mirrors migrate in internal/durable/migrate.go. plan's every
// failure is a refusal, so terminal.
func (d *Inline) migrate(ctx context.Context, vmID string, req orchestrator.MigrateRequest) (string, error) {
	plan, err := d.fm.MigrateBegin(ctx, vmID, req)
	if err != nil {
		return "", bounded(err)
	}
	m := &migration{plan: plan}
	seed, started, delta, err := d.migrateRun(ctx, m)
	if err != nil {
		if !terminal(err) {
			return "", err
		}
		if cerr := d.migrateCompensate(ctx, m); cerr != nil {
			return "", cerr
		}
		return "", err
	}
	return plan.NewVMID, d.migrateFinish(ctx, m, seed, started, delta)
}

// migrateRun mirrors migration.run: up to the target's persist.
func (d *Inline) migrateRun(ctx context.Context, m *migration) (seed orchestrator.SnapshotRecord, started orchestrator.MigrateStarted, delta bool, err error) {
	fm, plan := d.fm, m.plan
	vmID := plan.SourceVMID

	st := d.chain(vmID)
	delta = plan.Delta && st.pending == nil && !st.dropped
	staged := delta && st.chain.Standby == plan.TargetHostID && st.chain.Base != ""

	m.placed = true
	if err := fm.MigratePlaceholder(ctx, plan); err != nil {
		return seed, started, delta, bounded(err)
	}

	localID := ""
	if staged {
		seed, localID = st.chain.Head, st.chain.Base
	} else {
		seed, err = d.snapshotSteps(ctx, vmID, orchestrator.SnapshotOptions{
			Comment: "migration seed", Live: plan.Live, ShrinkMemory: plan.Live,
		})
		if err != nil {
			return seed, started, delta, err
		}
		m.seed = seed
		if plan.Live && seed.Kind != orchestrator.SnapshotKindLive {
			return seed, started, delta, fmt.Errorf("%w: host %s took a %s snapshot for a live migrate of vm %s",
				orchestrator.ErrSnapshotUnsupported, plan.SourceHostID, seed.Kind, vmID)
		}
	}

	if !plan.SameHost() {
		if localID == "" {
			mv, err := fm.MigrateTransferPlan(ctx, plan, seed)
			if err != nil {
				return seed, started, delta, bounded(err)
			}
			localID = mv.Existing
			if localID == "" {
				if localID, err = d.migrateMove(ctx, mv.Job); err != nil {
					return seed, started, delta, err
				}
			}
		}
		if delta {
			if localID, err = d.migrateDelta(ctx, m, st, seed, localID); err != nil {
				return seed, started, delta, err
			}
		}
	}

	if err := fm.MigrateCreate(ctx, plan, seed, localID); err != nil {
		return seed, started, delta, bounded(err)
	}
	if started.Ingress, err = fm.MigrateIngress(ctx, plan); err != nil {
		return seed, started, delta, bounded(err)
	}
	if started.Credentials, err = fm.MigrateCredentials(ctx, plan); err != nil {
		return seed, started, delta, bounded(err)
	}
	for _, fn := range []func(context.Context, orchestrator.MigratePlan, orchestrator.MigrateStarted) error{
		fm.MigrateUpload, fm.MigrateStartAgent, fm.MigratePersist,
	} {
		if err := fn(ctx, plan, started); err != nil {
			return seed, started, delta, bounded(err)
		}
	}
	return seed, started, delta, nil
}

// migrateMove mirrors migrateMove: a move that fails for good is a transfer
// failure.
func (d *Inline) migrateMove(ctx context.Context, job orchestrator.ArtifactMoveJob) (string, error) {
	id, err := d.moveLocked(ctx, job)
	if terminal(err) {
		return "", fmt.Errorf("%w: copy %s from host %s to host %s: %w",
			orchestrator.ErrArtifactTransferFailed, job.SnapshotID, job.SourceHostID, job.HostID, err)
	}
	return id, err
}

// migrateDelta mirrors migration.moveDelta: the diff with the source paused,
// merged onto the seed's copy, and the chain forgotten either way.
func (d *Inline) migrateDelta(ctx context.Context, m *migration, st chainObject, seed orchestrator.SnapshotRecord, base string) (string, error) {
	plan := m.plan
	m.frozenAt = time.Now().UnixMilli()
	child, err := d.snapshotSteps(ctx, plan.SourceVMID, orchestrator.SnapshotOptions{
		Comment: "migration delta", Live: true, Diff: true,
		Parent: seed.SnapshotID, KeepPaused: true, NoLineage: true,
	})
	localID := ""
	if err == nil {
		var job orchestrator.ArtifactMoveJob
		job, err = d.fm.MigrateDeltaPlan(ctx, plan, child, base)
		err = bounded(err)
		if err == nil {
			localID, err = d.migrateMove(ctx, job)
		}
	}
	if err != nil && !terminal(err) {
		return "", err
	}
	if !st.chain.Empty() {
		if st.chain.Base != "" {
			d.dropHolder(st.chain.Base, chainHolder(plan.SourceVMID), 0, false)
		}
		st.chain = orchestrator.ChainState{}
		d.saveChain(plan.SourceVMID, st)
	}
	return localID, err
}

// migrateFrozen mirrors migration.frozen: how long the diff kept the source
// paused, recorded once.
func (d *Inline) migrateFrozen(ctx context.Context, m *migration, outcome string) error {
	if m.frozenAt == 0 {
		return nil
	}
	dur := time.Duration(time.Now().UnixMilli()-m.frozenAt) * time.Millisecond
	m.frozenAt = 0
	return bounded(d.fm.MigrateFrozen(ctx, m.plan, dur, outcome))
}

// migrateCompensate mirrors migration.compensate: the target destroyed on its
// own key, a frozen source resumed, the seed deleted.
func (d *Inline) migrateCompensate(ctx context.Context, m *migration) error {
	fm, plan := d.fm, m.plan
	if m.placed {
		err := d.destroyLocked(ctx, plan.NewVMID, orchestrator.DestroyRequest{Reason: orchestrator.DestroyReasonMigrateRollback, HostID: plan.TargetHostID})
		if err != nil && !terminal(err) {
			return err
		}
	}
	if m.frozenAt != 0 {
		if err := fm.MigrateResumeSource(ctx, plan); err != nil {
			return held(err, nil)
		}
		_ = d.migrateFrozen(ctx, m, "rolled_back")
	}
	if m.seed.SnapshotID != "" {
		_ = fm.MigrateDeleteSeed(ctx, m.seed)
	}
	return nil
}

// migrateFinish mirrors migration.finish. the source's destroy runs as its
// steps here, since this run holds the source's key.
func (d *Inline) migrateFinish(ctx context.Context, m *migration, seed orchestrator.SnapshotRecord, started orchestrator.MigrateStarted, delta bool) error {
	fm, plan := d.fm, m.plan
	if err := d.migrateFrozen(ctx, m, "cutover"); err != nil {
		return err
	}
	if err := fm.MigrateTask(ctx, plan); err != nil {
		return bounded(err)
	}
	if err := fm.MigrateCutover(ctx, plan, started); err != nil {
		return bounded(err)
	}
	if err := fm.MigrateLineage(ctx, plan, seed, delta); err != nil {
		return bounded(err)
	}
	if err := fm.MigrateHandover(ctx, plan); err != nil {
		return bounded(err)
	}
	if plan.Lazy {
		if err := d.pin(seed.SnapshotID, orchestrator.LazyMemoryHolder(plan.NewVMID), orchestrator.LazyMemoryPinTTL); err != nil {
			return err
		}
		w := orchestrator.MemoryWatch{VMID: plan.NewVMID, SeedID: seed.SnapshotID}
		d.goTracked(func() {
			defer d.lock("Migrate", plan.MigrationID)()
			_ = d.watchMemory(context.Background(), w)
		})
	}
	if !delta {
		if err := fm.MigrateDrainSource(ctx, plan); err != nil {
			return bounded(err)
		}
	}
	err := d.destroy(ctx, plan.SourceVMID, orchestrator.DestroyRequest{Reason: orchestrator.DestroyReasonMigrated})
	if err != nil && !terminal(err) {
		return err
	}
	return nil
}

// watchMemory mirrors Migrate/{migration_id}.WatchMemory: polls until the
// lazy vm's memory is all on the target or the watch times out, then records
// it and releases the seed's pin.
func (d *Inline) watchMemory(ctx context.Context, w orchestrator.MemoryWatch) error {
	deadline := time.Now().Add(orchestrator.LazyMemoryWatchTimeout)
	wait := memoryPollFirst
	for {
		time.Sleep(wait)
		poll, err := d.fm.MigrateMemoryPoll(ctx, w.VMID)
		if err != nil {
			return held(err, nil)
		}
		settled := poll.Settled()
		if settled || !time.Now().Before(deadline) {
			if err := d.fm.MigrateMemorySettled(ctx, w.VMID, w.SeedID, poll, !settled); err != nil {
				return held(err, nil)
			}
			d.dropHolder(w.SeedID, orchestrator.LazyMemoryHolder(w.VMID), 0, false)
			return nil
		}
		wait = min(wait*2, memoryPollMax)
	}
}

// hitl

// SendHitlRun mirrors a one-way send to Intervention/{id}.Run, keyed on the
// id, so a second send starts nothing.
func (d *Inline) SendHitlRun(ctx context.Context, id string) error {
	if !d.once("hitl-run:" + id) {
		return nil
	}
	hctx := handlerCtx(ctx)
	d.goTracked(func() {
		defer d.lock("Intervention", id)()
		_ = d.hitlRun(hctx, id)
	})
	return nil
}

// interventionGone is hitlPermanent's test.
func interventionGone(err error) bool { return errors.Is(err, orchestrator.ErrInterventionNotFound) }

// hitlRun mirrors hitlRun in internal/durable/hitl.go: begin picks the route;
// destroy, snapshot and token rotation are calls to the env's own handlers.
func (d *Inline) hitlRun(ctx context.Context, id string) error {
	fm := d.fm
	plan, err := fm.HitlRunBegin(ctx, id)
	if err != nil {
		return held(err, interventionGone)
	}
	var res hitl.Result
	switch plan.Route {
	case orchestrator.HitlRouteNone:
		return nil
	case orchestrator.HitlRouteInProcess:
		return held(fm.HitlRunInProcess(ctx, id), interventionGone)
	case orchestrator.HitlRouteDestroy:
		err = d.destroyLocked(ctx, plan.EnvID, orchestrator.DestroyRequest{Reason: orchestrator.DestroyReasonManual})
	case orchestrator.HitlRouteSnapshot:
		var rec orchestrator.SnapshotRecord
		rec, err = d.snapshotLocked(ctx, plan.EnvID, *plan.Snapshot)
		res.SnapshotID = rec.SnapshotID
	case orchestrator.HitlRouteRotateToken:
		err = d.rotateLocked(ctx, plan.EnvID)
	default:
		err = fmt.Errorf("unknown hitl run route %q", plan.Route)
	}
	if err != nil {
		if !terminal(err) {
			return err
		}
		res = hitl.Result{Error: err.Error()}
	}
	return held(fm.HitlRunFinish(ctx, id, plan, res), interventionGone)
}

// SendHitlExpire mirrors a delayed send to Intervention/{id}.Expire.
func (d *Inline) SendHitlExpire(ctx context.Context, id string, delay time.Duration) error {
	d.scheduleHitlExpire(handlerCtx(ctx), id, delay)
	return nil
}

func (d *Inline) scheduleHitlExpire(ctx context.Context, id string, delay time.Duration) {
	d.after(delay, func() {
		defer d.lock("Intervention", id)()
		_ = d.hitlExpire(ctx, id)
	})
}

// hitlExpire mirrors hitlExpire: a timer that fires early sends itself again
// for the rest.
func (d *Inline) hitlExpire(ctx context.Context, id string) error {
	left, err := d.fm.HitlExpire(ctx, id)
	if err != nil {
		return held(err, nil)
	}
	if left > 0 {
		d.scheduleHitlExpire(ctx, id, left)
	}
	return nil
}

// webhooks

// SendWebhook mirrors a one-way send to Webhooks.Deliver.
func (d *Inline) SendWebhook(ctx context.Context, wd orchestrator.WebhookDelivery) error {
	hctx := handlerCtx(ctx)
	d.goTracked(func() { _ = d.deliverWebhook(hctx, wd) })
	return nil
}

// webhookBackoff mirrors webhookBackoff: the wait after failed attempt
// n, counting from 1.
func webhookBackoff(attempt int) time.Duration {
	wait := webhookFirstBackoff
	for i := 1; i < attempt && wait < webhookMaxBackoff; i++ {
		wait *= 4
	}
	return min(wait, webhookMaxBackoff)
}

// deliverWebhook mirrors deliverWebhook in internal/durable/webhooks.go: one
// attempt, the next sent to itself with a delay, a dead letter once the
// window is past or the endpoint no longer decrypts.
func (d *Inline) deliverWebhook(ctx context.Context, wd orchestrator.WebhookDelivery) error {
	fm := d.fm
	res, err := fm.WebhookPost(ctx, wd)
	lastErr := res.Error
	switch {
	case err != nil && !orchestrator.WebhookEndpointSealed(err):
		return &retrying{err: err}
	case err != nil:
		lastErr = err.Error()
	case res.Delivered:
		return nil
	default:
		wait := webhookBackoff(wd.Attempt)
		if res.At.Add(wait).Sub(wd.FirstAt) <= webhookWindow {
			next := wd
			next.Attempt++
			d.after(wait, func() { _ = d.deliverWebhook(ctx, next) })
			return nil
		}
	}
	return held(fm.WebhookDeadLetter(ctx, wd, lastErr), nil)
}

// images

// ConvertImage mirrors Images.Convert, awaited.
func (d *Inline) ConvertImage(ctx context.Context, req orchestrator.ConvertImageRequest) (orchestrator.Image, error) {
	v, err := request(d, ctx, "Images/Convert", func(c context.Context) (orchestrator.Image, error) {
		return d.convertImage(c, req)
	})
	return v, crossed(err, orchestrator.ImageFleetErrors)
}

// convertImage mirrors convertImage in internal/durable/convert.go: every
// failure is terminal, and a conversion still running gets a watch, keyed on
// host, digest and attempt when the agent gives one.
func (d *Inline) convertImage(ctx context.Context, req orchestrator.ConvertImageRequest) (orchestrator.Image, error) {
	fm := d.fm
	hostID, err := fm.ImageHost(ctx, req)
	if err != nil {
		return orchestrator.Image{}, bounded(err)
	}
	req.HostID = hostID
	img, err := fm.ImageConvert(ctx, req)
	if err != nil {
		return orchestrator.Image{}, bounded(err)
	}
	if img.State == orchestrator.ImageConverting {
		if img.CreatedAt == "" || d.once("image-watch:"+img.HostID+":"+img.Digest+":"+img.CreatedAt) {
			d.goTracked(func() { _ = d.watchImage(context.Background(), img) })
		}
	}
	return img, nil
}

// watchImage mirrors Images.Watch: polls until the conversion is ready or
// failed, or ImageWatchTimeout passes, then records the outcome.
func (d *Inline) watchImage(ctx context.Context, img orchestrator.Image) error {
	deadline := time.Now().Add(orchestrator.ImageWatchTimeout)
	wait := imagePollFirst
	for {
		time.Sleep(wait)
		polled, err := d.fm.ImagePoll(ctx, img)
		if err != nil {
			return bounded(err)
		}
		finished := polled.State != orchestrator.ImageConverting
		if finished || !time.Now().Before(deadline) {
			return bounded(d.fm.ImageConvertDone(ctx, polled, !finished))
		}
		wait = min(wait*2, imagePollMax)
	}
}

// builds

// SubmitBuild mirrors a send to the Build/{build_id}.Run workflow, which
// restate runs once per id.
func (d *Inline) SubmitBuild(ctx context.Context, in orchestrator.BuildInput) error {
	if !d.once("build:" + in.ID) {
		return nil
	}
	hctx := handlerCtx(ctx)
	d.goTracked(func() {
		defer d.lock("Build", in.ID)()
		_ = d.build(hctx, in)
	})
	return nil
}

// build mirrors build in internal/durable/build.go: every step is bounded and
// carries an idempotency key of the build and step; the builder is destroyed
// on both paths before the build is recorded.
func (d *Inline) build(ctx context.Context, in orchestrator.BuildInput) error {
	fm := d.fm
	step := func(name string) context.Context {
		return orchestrator.WithIdempotencyKey(ctx, "build:"+in.ID+":"+name)
	}
	plan, err := fm.BuildResolveCache(step(orchestrator.BuildStepResolve), in)
	var vmID string
	if err == nil {
		vmID, err = fm.BuildCreateBuilder(step(orchestrator.BuildStepCreate), in, plan)
	}
	for _, ex := range plan.Execs {
		if err != nil {
			break
		}
		err = fm.BuildExec(step(ex.Step), in, vmID, ex)
		if err == nil && ex.Layer >= 0 {
			_, err = fm.BuildSnapshotLayer(step("snapshot_"+ex.Step), in, vmID, ex.Layer)
		}
	}
	var snapshotID string
	if err == nil {
		snapshotID, err = fm.BuildSnapshot(step(orchestrator.BuildStepSnapshot), in, vmID)
	}
	err = bounded(err)

	outcome := orchestrator.BuildSucceeded
	if err != nil {
		outcome = orchestrator.BuildFailed
	}
	_ = fm.BuildDestroyBuilder(step(orchestrator.BuildStepDestroy), in, outcome)
	if err != nil {
		_ = fm.BuildFailed(step("failed"), in, err.Error())
		return err
	}
	return bounded(fm.BuildFinish(step("finish"), in, snapshotID))
}

var (
	_ orchestrator.DurableDestroyer    = (*Inline)(nil)
	_ orchestrator.DurableCreator      = (*Inline)(nil)
	_ orchestrator.DurableForker       = (*Inline)(nil)
	_ orchestrator.DurableMigrator     = (*Inline)(nil)
	_ orchestrator.DurableSnapshots    = (*Inline)(nil)
	_ orchestrator.DurableArtifacts    = (*Inline)(nil)
	_ orchestrator.DurableTokenRotator = (*Inline)(nil)
	_ orchestrator.DurableExpiry       = (*Inline)(nil)
	_ orchestrator.DurableHitl         = (*Inline)(nil)
	_ orchestrator.DurableWebhooks     = (*Inline)(nil)
	_ orchestrator.DurableImages       = (*Inline)(nil)
	_ orchestrator.DurableBuilds       = (*Inline)(nil)
)
